package testutil

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

func TestNewTestTokenCarriesClaims(t *testing.T) {
	t.Parallel()

	token := NewTestToken(t, map[string]any{
		"organization_id": "org-123",
		"scopes":          []string{"scope:write"},
	})
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		t.Fatalf("expected three token segments, got %d", len(parts))
	}

	payloadRaw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		t.Fatalf("decode token payload: %v", err)
	}

	var payload map[string]any
	if err := json.Unmarshal(payloadRaw, &payload); err != nil {
		t.Fatalf("decode payload json: %v", err)
	}
	if payload["organization_id"] != "org-123" {
		t.Fatalf("unexpected org id %#v", payload["organization_id"])
	}
}

func TestNewAuthenticatedRequestSetsAuthorizationAndBody(t *testing.T) {
	t.Parallel()

	request := NewAuthenticatedRequest(t, http.MethodPost, "/v1/items", "org-123", "items:write", `{"ok":true}`)
	if !strings.HasPrefix(request.Header.Get("Authorization"), "Bearer ") {
		t.Fatalf("expected bearer token header, got %q", request.Header.Get("Authorization"))
	}
	if request.Header.Get("Content-Type") != "application/json" {
		t.Fatalf("expected json content type, got %q", request.Header.Get("Content-Type"))
	}
}

func TestAssertJSONResponse(t *testing.T) {
	t.Parallel()

	server := NewTestServer(t, http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		WriteJSON(t, writer, http.StatusCreated, map[string]any{"id": "1"})
	}))

	response, err := server.Client().Get(server.URL)
	if err != nil {
		t.Fatalf("get test server response: %v", err)
	}

	AssertJSONResponse(t, response, http.StatusCreated, map[string]any{"id": "1"})
}

func TestScopedConnConfigSetsSearchPath(t *testing.T) {
	t.Parallel()

	config := scopedConnConfig(t, "postgres://user:pass@localhost:5432/app", "test_schema")
	if got := config.RuntimeParams["search_path"]; got != "test_schema" {
		t.Fatalf("expected search_path test_schema, got %q", got)
	}
}
