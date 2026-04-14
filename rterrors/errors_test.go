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

func TestConnectCodePreservesStandaloneConnectError(t *testing.T) {
	t.Parallel()

	// Regression: before the fix the empty-string runtime code match
	// swallowed all errors without a runtime Code and returned CodeInternal,
	// making the connect.CodeOf fallback dead code.
	tests := []struct {
		name string
		code connect.Code
	}{
		{"Unavailable", connect.CodeUnavailable},
		{"Canceled", connect.CodeCanceled},
		{"DataLoss", connect.CodeDataLoss},
		{"Aborted", connect.CodeAborted},
		{"DeadlineExceeded", connect.CodeDeadlineExceeded},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			err := connect.NewError(tt.code, errors.New("connect error"))
			if got := ConnectCode(err); got != tt.code {
				t.Fatalf("ConnectCode(*connect.Error{%v}) = %v, want %v", tt.code, got, tt.code)
			}
		})
	}
}

func TestConnectCodePlainErrorFallsToDefault(t *testing.T) {
	t.Parallel()

	// A plain error with no runtime Code and no *connect.Error wrapping
	// must fall through the switch to the default CodeInternal return.
	err := errors.New("something broke")
	if got := ConnectCode(err); got != connect.CodeInternal {
		t.Fatalf("ConnectCode(plain error) = %v, want %v", got, connect.CodeInternal)
	}
}

func TestToConnectErrorPreservesExistingConnectErrors(t *testing.T) {
	t.Parallel()

	original := connect.NewError(connect.CodeUnavailable, errors.New("upstream unavailable"))
	if got := ToConnectError(original); got != original {
		t.Fatal("expected ToConnectError to preserve existing connect errors")
	}
}

func TestToConnectErrorPrefersRuntimeCodeOverWrappedConnectError(t *testing.T) {
	t.Parallel()

	err := E(
		CodeNotFound,
		"load_policy",
		"policy missing",
		connect.NewError(connect.CodeUnavailable, errors.New("dial timeout")),
	)
	if got := connect.CodeOf(ToConnectError(err)); got != connect.CodeNotFound {
		t.Fatalf("connect.CodeOf(ToConnectError(err)) = %v, want %v", got, connect.CodeNotFound)
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
