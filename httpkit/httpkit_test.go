package httpkit

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
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

	assertErrorCode(t, recorder.Body.Bytes(), ErrorCodeInvalidJSON)
}

func TestPathUUIDRejectsInvalidValue(t *testing.T) {
	t.Parallel()

	recorder := httptest.NewRecorder()
	if _, ok := PathUUID(recorder, "bad", "contactID"); ok {
		t.Fatal("expected parse failure")
	}

	assertErrorCode(t, recorder.Body.Bytes(), "invalid_contactID")
}

func TestRequireIfMatchVersion(t *testing.T) {
	t.Parallel()

	t.Run("missing header", func(t *testing.T) {
		request := httptest.NewRequest(http.MethodPatch, "/", nil)
		recorder := httptest.NewRecorder()
		if _, ok := RequireIfMatchVersion(recorder, request); ok {
			t.Fatal("expected missing If-Match to fail")
		}
		assertErrorCode(t, recorder.Body.Bytes(), "missing_if_match")
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
	assertErrorCode(t, recorder.Body.Bytes(), "not_ready")
}

func TestWriteStoreError(t *testing.T) {
	t.Parallel()

	notFound := errors.New("not_found")
	recorder := httptest.NewRecorder()
	WriteStoreError(recorder, notFound, StoreErrors{NotFound: notFound})
	if recorder.Code != http.StatusNotFound {
		t.Fatalf("expected status %d, got %d", http.StatusNotFound, recorder.Code)
	}
	assertErrorCode(t, recorder.Body.Bytes(), ErrorCodeNotFound)
}

func assertErrorCode(t *testing.T, raw []byte, expected string) {
	t.Helper()

	var response ErrorResponse
	if err := json.Unmarshal(raw, &response); err != nil {
		t.Fatalf("decode error response: %v", err)
	}
	if response.Error.Code != expected {
		t.Fatalf("expected error code %q, got %q", expected, response.Error.Code)
	}
}

type versionedValue struct {
	Version int64 `json:"version"`
}

func (value versionedValue) GetVersion() int64 {
	return value.Version
}
