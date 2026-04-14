package testutil

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
)

// NewTestServer starts an httptest server and closes it during test cleanup.
func NewTestServer(t *testing.T, handler http.Handler) *httptest.Server {
	t.Helper()

	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	return server
}

// WriteJSON writes a JSON response body for test HTTP handlers.
func WriteJSON(t *testing.T, writer http.ResponseWriter, status int, payload any) {
	t.Helper()

	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(status)
	if err := json.NewEncoder(writer).Encode(payload); err != nil {
		t.Fatalf("encode json response: %v", err)
	}
}

// AssertJSONResponse verifies the response status and JSON body.
func AssertJSONResponse(t *testing.T, response *http.Response, status int, body any) {
	t.Helper()

	if response == nil {
		t.Fatal("expected non-nil response")
	}
	defer func() { _ = response.Body.Close() }()

	if response.StatusCode != status {
		t.Fatalf("expected status %d, got %d", status, response.StatusCode)
	}

	raw, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("read response body: %v", err)
	}

	if body == nil {
		if len(bytes.TrimSpace(raw)) != 0 {
			t.Fatalf("expected empty body, got %s", formatJSON(raw))
		}
		return
	}

	var got any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("decode response body: %v\nbody: %s", err, string(raw))
	}

	wantRaw, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal expected body: %v", err)
	}
	var want any
	if err := json.Unmarshal(wantRaw, &want); err != nil {
		t.Fatalf("decode expected body: %v", err)
	}

	if !reflect.DeepEqual(got, want) {
		t.Fatalf("unexpected json response\nwant: %s\ngot:  %s", formatJSON(wantRaw), formatJSON(raw))
	}
}

// AssertErrorCode verifies the standard JSON error envelope contains the expected code.
func AssertErrorCode(t *testing.T, raw []byte, expected string) {
	t.Helper()

	var response struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(raw, &response); err != nil {
		t.Fatalf("decode error response: %v", err)
	}
	if response.Error.Code != expected {
		t.Fatalf("expected error code %q, got %q", expected, response.Error.Code)
	}
}

func formatJSON(raw []byte) string {
	var formatted bytes.Buffer
	if err := json.Indent(&formatted, raw, "", "  "); err != nil {
		return string(raw)
	}
	return formatted.String()
}
