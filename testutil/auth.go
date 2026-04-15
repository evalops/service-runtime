package testutil

import (
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"context"
	"strings"
	"testing"
)

// NewTestToken builds an unsigned JWT-style token with the provided claims.
func NewTestToken(t *testing.T, claims map[string]any) string {
	t.Helper()

	if claims == nil {
		claims = map[string]any{}
	}

	header := map[string]string{
		"alg": "none",
		"typ": "JWT",
	}
	return encodeTokenSegment(t, header) + "." + encodeTokenSegment(t, claims) + "."
}

// NewAuthenticatedRequest returns an httptest request with a bearer token carrying test claims.
//
// When body is provided, the first body value becomes the request body and Content-Type is set to application/json.
func NewAuthenticatedRequest(t *testing.T, method, target, orgID, scope string, body ...string) *http.Request {
	t.Helper()

	var reader io.Reader
	if len(body) > 0 {
		reader = strings.NewReader(body[0])
	}

	request := httptest.NewRequestWithContext(context.Background(), method, target, reader)
	tokenClaims := map[string]any{
		"sub":             "test-subject",
		"organization_id": strings.TrimSpace(orgID),
	}
	if scope = strings.TrimSpace(scope); scope != "" {
		tokenClaims["scopes"] = []string{scope}
	}
	request.Header.Set("Authorization", "Bearer "+NewTestToken(t, tokenClaims))
	if len(body) > 0 {
		request.Header.Set("Content-Type", "application/json")
	}
	return request
}

func encodeTokenSegment(t *testing.T, value any) string {
	t.Helper()

	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("marshal token segment: %v", err)
	}
	return base64.RawURLEncoding.EncodeToString(raw)
}
