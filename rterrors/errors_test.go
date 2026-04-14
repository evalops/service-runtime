package rterrors

import (
	"bytes"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"connectrpc.com/connect"
)

func TestErrorsIsMatchesByCode(t *testing.T) {
	t.Parallel()

	err := Wrap(CodeNotFound, "load_policy", errors.New("row missing"))
	if !errors.Is(err, &Error{Code: CodeNotFound}) {
		t.Fatal("expected errors.Is to match by runtime error code")
	}
}

func TestCodeAndMessageOfStructuredError(t *testing.T) {
	t.Parallel()

	err := E(CodeConflict, "upsert_budget", "budget already exists", errors.New("duplicate key"))
	if got := CodeOf(err); got != CodeConflict {
		t.Fatalf("CodeOf() = %q, want %q", got, CodeConflict)
	}
	if got := MessageOf(err); got != "budget already exists" {
		t.Fatalf("MessageOf() = %q, want %q", got, "budget already exists")
	}
}

func TestWrapUsesDefaultInternalMessage(t *testing.T) {
	t.Parallel()

	err := Wrap(CodeInternal, "query_usage", errors.New("db timeout"))
	if got := MessageOf(err); got != "internal error" {
		t.Fatalf("MessageOf() = %q, want %q", got, "internal error")
	}
}

func TestConnectCodeAndHTTPStatus(t *testing.T) {
	t.Parallel()

	err := Wrap(CodeRateLimited, "ingest", errors.New("burst exceeded"))
	if got := ConnectCode(err); got != connect.CodeResourceExhausted {
		t.Fatalf("ConnectCode() = %v, want %v", got, connect.CodeResourceExhausted)
	}
	if got := HTTPStatus(err); got != http.StatusTooManyRequests {
		t.Fatalf("HTTPStatus() = %d, want %d", got, http.StatusTooManyRequests)
	}
}

func TestConnectCodePrefersRuntimeCodeOverWrappedConnectError(t *testing.T) {
	t.Parallel()

	err := E(
		CodeNotFound,
		"load_policy",
		"policy missing",
		connect.NewError(connect.CodeUnavailable, errors.New("dial timeout")),
	)
	if got := ConnectCode(err); got != connect.CodeNotFound {
		t.Fatalf("ConnectCode() = %v, want %v", got, connect.CodeNotFound)
	}
	if got := HTTPStatus(err); got != http.StatusNotFound {
		t.Fatalf("HTTPStatus() = %d, want %d", got, http.StatusNotFound)
	}
}

func TestToConnectErrorPreservesExistingConnectErrors(t *testing.T) {
	t.Parallel()

	original := connect.NewError(connect.CodeUnavailable, errors.New("upstream unavailable"))
	if got := ToConnectError(original); got != original {
		t.Fatal("expected ToConnectError to preserve existing connect errors")
	}
}

func TestWriteError(t *testing.T) {
	t.Parallel()

	recorder := httptest.NewRecorder()
	WriteError(recorder, E(CodeUnauthorized, "auth.check", "token missing", nil))

	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("expected status %d, got %d", http.StatusUnauthorized, recorder.Code)
	}

	var response ErrorResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode error response: %v", err)
	}
	if response.Error.Code != string(CodeUnauthorized) {
		t.Fatalf("expected code %q, got %q", CodeUnauthorized, response.Error.Code)
	}
	if response.Error.Message != "token missing" {
		t.Fatalf("expected message %q, got %q", "token missing", response.Error.Message)
	}
}

func TestWriteErrorIgnoresNilError(t *testing.T) {
	t.Parallel()

	recorder := httptest.NewRecorder()
	WriteError(recorder, nil)

	if recorder.Code != http.StatusOK {
		t.Fatalf("expected untouched recorder status %d, got %d", http.StatusOK, recorder.Code)
	}
	if recorder.Body.Len() != 0 {
		t.Fatalf("expected no response body for nil error, got %q", recorder.Body.String())
	}
}

func TestRecoverMiddleware(t *testing.T) {
	t.Parallel()

	var output bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&output, nil))
	handler := RecoverMiddleware(logger)(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		panic("boom")
	}))

	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/panic", nil))

	if recorder.Code != http.StatusInternalServerError {
		t.Fatalf("expected status %d, got %d", http.StatusInternalServerError, recorder.Code)
	}

	var response ErrorResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode panic response: %v", err)
	}
	if response.Error.Code != string(CodeInternal) {
		t.Fatalf("expected code %q, got %q", CodeInternal, response.Error.Code)
	}
	if !strings.Contains(output.String(), "http panic recovered") {
		t.Fatalf("expected panic log entry, got %q", output.String())
	}
}
