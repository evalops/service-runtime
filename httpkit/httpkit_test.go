package httpkit

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"connectrpc.com/connect"
	"github.com/evalops/service-runtime/rterrors"
	"github.com/evalops/service-runtime/testutil"
	"github.com/go-chi/chi/v5"
	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	tracetest "go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
)

func TestWriteMutationJSONSetsHeaders(t *testing.T) {
	t.Parallel()

	recorder := httptest.NewRecorder()
	WriteMutationJSON(recorder, http.StatusCreated, versionedValue{Version: 7}, 42)

	if recorder.Code != http.StatusCreated {
		t.Fatalf("expected status %d, got %d", http.StatusCreated, recorder.Code)
	}
	if got := recorder.Header().Get("X-Change-Seq"); got != "42" {
		t.Fatalf("expected X-Change-Seq 42, got %q", got)
	}
	if got := recorder.Header().Get("ETag"); got != `"7"` {
		t.Fatalf("expected ETag %q, got %q", `"7"`, got)
	}
}

func TestDecodeJSONRejectsTrailingContent(t *testing.T) {
	t.Parallel()

	request := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"ok":true} {"extra":true}`))
	recorder := httptest.NewRecorder()

	var payload map[string]any
	if DecodeJSON(recorder, request, &payload) {
		t.Fatal("expected decode failure")
	}

	testutil.AssertErrorCode(t, recorder.Body.Bytes(), ErrorCodeInvalidJSON)
}

func TestPathUUIDRejectsInvalidValue(t *testing.T) {
	t.Parallel()

	recorder := httptest.NewRecorder()
	if _, ok := PathUUID(recorder, "bad", "contactID"); ok {
		t.Fatal("expected parse failure")
	}

	testutil.AssertErrorCode(t, recorder.Body.Bytes(), "invalid_contactID")
}

func TestRequireIfMatchVersion(t *testing.T) {
	t.Parallel()

	t.Run("missing header", func(t *testing.T) {
		request := httptest.NewRequest(http.MethodPatch, "/", nil)
		recorder := httptest.NewRecorder()
		if _, ok := RequireIfMatchVersion(recorder, request); ok {
			t.Fatal("expected missing If-Match to fail")
		}
		testutil.AssertErrorCode(t, recorder.Body.Bytes(), "missing_if_match")
	})

	t.Run("valid header", func(t *testing.T) {
		request := httptest.NewRequest(http.MethodPatch, "/", nil)
		request.Header.Set("If-Match", `"9"`)
		recorder := httptest.NewRecorder()
		version, ok := RequireIfMatchVersion(recorder, request)
		if !ok {
			t.Fatal("expected If-Match to parse")
		}
		if version != 9 {
			t.Fatalf("expected version 9, got %d", version)
		}
	})
}

func TestWithRequestID(t *testing.T) {
	t.Parallel()

	var seen string
	handler := WithRequestID(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		var ok bool
		seen, ok = RequestIDFromContext(request.Context())
		if !ok {
			t.Fatal("missing request id in context")
		}
		writer.WriteHeader(http.StatusNoContent)
	}))

	request := httptest.NewRequest(http.MethodGet, "/", nil)
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)

	if seen == "" {
		t.Fatal("expected generated request id")
	}
	if got := recorder.Header().Get("X-Request-Id"); got == "" {
		t.Fatal("expected X-Request-Id response header")
	}
}

func TestRequestMetadataIncludesRequestID(t *testing.T) {
	t.Parallel()

	request := httptest.NewRequest(http.MethodGet, "/items", nil)
	request = request.WithContext(context.WithValue(request.Context(), requestIDContextKey, "req-123"))

	metadata := RequestMetadata(request)
	if metadata["request_id"] != "req-123" {
		t.Fatalf("expected request_id metadata, got %#v", metadata["request_id"])
	}
}

func TestWithRequestLogging(t *testing.T) {
	t.Parallel()

	var output bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&output, nil))
	handler := WithRequestLogging(logger)(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.WriteHeader(http.StatusAccepted)
		_, _ = writer.Write([]byte(`{"ok":true}`))
	}))

	request := httptest.NewRequest(http.MethodGet, "/contacts", nil)
	request = request.WithContext(context.WithValue(request.Context(), requestIDContextKey, "req-1"))
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)

	logLine := output.String()
	if !strings.Contains(logLine, "request_id=req-1") {
		t.Fatalf("expected request_id in log line, got %q", logLine)
	}
	if !strings.Contains(logLine, "status=202") {
		t.Fatalf("expected status in log line, got %q", logLine)
	}
}

func TestWithTelemetryStartsServerSpan(t *testing.T) {
	recorder := installTelemetryTestTracerProvider(t)

	var seen trace.SpanContext
	handler := WithTelemetry("prompts")(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		seen = trace.SpanContextFromContext(request.Context())
		writer.WriteHeader(http.StatusNoContent)
	}))

	recorderHTTP := httptest.NewRecorder()
	handler.ServeHTTP(recorderHTTP, httptest.NewRequest(http.MethodGet, "/prompts", nil))

	if !seen.IsValid() {
		t.Fatal("expected valid span context in handler")
	}
	spans := recorder.Ended()
	if len(spans) != 1 {
		t.Fatalf("expected 1 ended span, got %d", len(spans))
	}
	if spans[0].Name() != "GET /prompts" {
		t.Fatalf("span name = %q, want GET /prompts", spans[0].Name())
	}
}

func TestWithTelemetryRenamesSpanAfterChiRouting(t *testing.T) {
	recorder := installTelemetryTestTracerProvider(t)

	router := chi.NewRouter()
	router.Use(WithTelemetry("contacts"))
	router.Get("/contacts/{contactID}", func(writer http.ResponseWriter, request *http.Request) {
		writer.WriteHeader(http.StatusOK)
	})

	recorderHTTP := httptest.NewRecorder()
	router.ServeHTTP(recorderHTTP, httptest.NewRequest(http.MethodGet, "/contacts/abc-123", nil))

	spans := recorder.Ended()
	if len(spans) != 1 {
		t.Fatalf("expected 1 ended span, got %d", len(spans))
	}
	if spans[0].Name() != "GET /contacts/{contactID}" {
		t.Fatalf("span name = %q, want GET /contacts/{contactID}", spans[0].Name())
	}
}

func TestReadyHandler(t *testing.T) {
	t.Parallel()

	handler := ReadyHandler(func(context.Context) error {
		return errors.New("db down")
	})
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected status %d, got %d", http.StatusServiceUnavailable, recorder.Code)
	}
	testutil.AssertErrorCode(t, recorder.Body.Bytes(), "not_ready")
}

func TestCaptureResponseWriterReadFromCopiesWithoutRecursing(t *testing.T) {
	t.Parallel()

	recorder := httptest.NewRecorder()
	writer := NewCaptureResponseWriter(recorder)

	n, err := io.Copy(writer, strings.NewReader("streamed response"))
	if err != nil {
		t.Fatalf("copy response body: %v", err)
	}
	if n != int64(len("streamed response")) {
		t.Fatalf("expected %d bytes copied, got %d", len("streamed response"), n)
	}
	if writer.StatusCode() != http.StatusOK {
		t.Fatalf("expected captured status %d, got %d", http.StatusOK, writer.StatusCode())
	}
	if got := string(writer.BodyBytes()); got != "streamed response" {
		t.Fatalf("expected captured body %q, got %q", "streamed response", got)
	}
	if got := recorder.Body.String(); got != "streamed response" {
		t.Fatalf("expected forwarded body %q, got %q", "streamed response", got)
	}
}

func TestWriteStoreError(t *testing.T) {
	t.Parallel()

	notFound := errors.New("not_found")
	recorder := httptest.NewRecorder()
	WriteStoreError(recorder, notFound, StoreErrors{NotFound: notFound})
	if recorder.Code != http.StatusNotFound {
		t.Fatalf("expected status %d, got %d", http.StatusNotFound, recorder.Code)
	}
	testutil.AssertErrorCode(t, recorder.Body.Bytes(), ErrorCodeNotFound)
}

func TestWriteStoreErrorPrefersMappedRuntimeCodeOverWrappedConnectError(t *testing.T) {
	t.Parallel()

	notFound := errors.New("not_found")
	err := connect.NewError(connect.CodeUnavailable, notFound)

	recorder := httptest.NewRecorder()
	WriteStoreError(recorder, err, StoreErrors{NotFound: notFound})

	if recorder.Code != http.StatusNotFound {
		t.Fatalf("expected status %d, got %d", http.StatusNotFound, recorder.Code)
	}
	testutil.AssertErrorCode(t, recorder.Body.Bytes(), ErrorCodeNotFound)
}

func TestWriteStoreErrorPreservesDefaultMessage(t *testing.T) {
	t.Parallel()

	recorder := httptest.NewRecorder()
	WriteStoreError(recorder, errors.New("db down"), StoreErrors{})

	if recorder.Code != http.StatusInternalServerError {
		t.Fatalf("expected status %d, got %d", http.StatusInternalServerError, recorder.Code)
	}
	testutil.AssertErrorCode(t, recorder.Body.Bytes(), ErrorCodeInternal)
	assertErrorMessage(t, recorder.Body.Bytes(), "db down")
}

func TestWriteMappedError(t *testing.T) {
	t.Parallel()

	recorder := httptest.NewRecorder()
	WriteMappedError(recorder, rterrors.E(rterrors.CodeRateLimited, "ingest", "too many requests", nil))

	if recorder.Code != http.StatusTooManyRequests {
		t.Fatalf("expected status %d, got %d", http.StatusTooManyRequests, recorder.Code)
	}
	testutil.AssertErrorCode(t, recorder.Body.Bytes(), string(rterrors.CodeRateLimited))
}

func TestRecoverMiddleware(t *testing.T) {
	t.Parallel()

	var output bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&output, nil))
	handler := RecoverMiddleware(logger)(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		panic("unexpected")
	}))

	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/panic", nil))

	if recorder.Code != http.StatusInternalServerError {
		t.Fatalf("expected status %d, got %d", http.StatusInternalServerError, recorder.Code)
	}
	testutil.AssertErrorCode(t, recorder.Body.Bytes(), string(rterrors.CodeInternal))
	if !strings.Contains(output.String(), "http panic recovered") {
		t.Fatalf("expected panic recovery log, got %q", output.String())
	}
}

func assertErrorMessage(t *testing.T, raw []byte, expected string) {
	t.Helper()

	var response ErrorResponse
	if err := json.Unmarshal(raw, &response); err != nil {
		t.Fatalf("decode error response: %v", err)
	}
	if response.Error.Message != expected {
		t.Fatalf("expected error message %q, got %q", expected, response.Error.Message)
	}
}

func installTelemetryTestTracerProvider(t *testing.T) *tracetest.SpanRecorder {
	t.Helper()

	// Callers must remain sequential because this helper swaps the global tracer provider.
	originalProvider := otel.GetTracerProvider()
	recorder := tracetest.NewSpanRecorder()
	tracerProvider := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(recorder))
	otel.SetTracerProvider(tracerProvider)
	t.Cleanup(func() {
		otel.SetTracerProvider(originalProvider)
		_ = tracerProvider.Shutdown(context.Background())
	})

	return recorder
}

type versionedValue struct {
	Version int64 `json:"version"`
}

func (value versionedValue) GetVersion() int64 {
	return value.Version
}
