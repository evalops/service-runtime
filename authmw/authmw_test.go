package authmw

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/evalops/service-runtime/httpkit"
)

func TestWithAuthMissingHeader(t *testing.T) {
	t.Parallel()

	middleware := New(Config{})
	recorder := httptest.NewRecorder()
	handler := middleware.WithAuth("scope:read")(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Fatal("next handler should not run")
	}))

	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/", nil))

	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("expected status %d, got %d", http.StatusUnauthorized, recorder.Code)
	}
	assertErrorCode(t, recorder.Body.Bytes(), "missing_authorization")
}

func TestWithAuthAPIKey(t *testing.T) {
	t.Parallel()

	validator := &stubAPIKeyValidator{
		result: ValidatedAPIKey{
			ID:             "integration-key",
			OrganizationID: "org-123",
			Scopes:         []string{"scope:read"},
		},
	}
	middleware := New(Config{APIKeyValidator: validator})

	var seenActor Actor
	handler := middleware.WithAuth("scope:read")(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		var ok bool
		seenActor, ok = ActorFromContext(request.Context())
		if !ok {
			t.Fatal("expected actor in context")
		}
		writer.WriteHeader(http.StatusNoContent)
	}))

	request := httptest.NewRequest(http.MethodGet, "/", nil)
	request.Header.Set("Authorization", "Bearer pk_live")
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusNoContent {
		t.Fatalf("expected status %d, got %d", http.StatusNoContent, recorder.Code)
	}
	if seenActor.Type != "api_key" || seenActor.OrganizationID != "org-123" {
		t.Fatalf("unexpected actor %#v", seenActor)
	}
}

func TestWithAuthAPIKeyMissingScopes(t *testing.T) {
	t.Parallel()

	middleware := New(Config{
		APIKeyValidator: &stubAPIKeyValidator{
			result: ValidatedAPIKey{
				ID:             "integration-key",
				OrganizationID: "org-123",
				Scopes:         []string{"scope:read"},
			},
		},
	})

	request := httptest.NewRequest(http.MethodGet, "/", nil)
	request.Header.Set("Authorization", "Bearer pk_live")
	recorder := httptest.NewRecorder()

	handler := middleware.WithAuth("scope:write")(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Fatal("next handler should not run")
	}))
	handler.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusForbidden {
		t.Fatalf("expected status %d, got %d", http.StatusForbidden, recorder.Code)
	}
	assertErrorCode(t, recorder.Body.Bytes(), "authorization_failed")
}

func TestWithAuthAPIKeyWithoutValidator(t *testing.T) {
	t.Parallel()

	request := httptest.NewRequest(http.MethodGet, "/", nil)
	request.Header.Set("Authorization", "Bearer pk_live")
	recorder := httptest.NewRecorder()

	handler := New(Config{}).WithAuth("scope:read")(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Fatal("next handler should not run")
	}))
	handler.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("expected status %d, got %d", http.StatusUnauthorized, recorder.Code)
	}
}

func TestWithAuthServiceToken(t *testing.T) {
	t.Parallel()

	middleware := New(Config{
		TokenVerifier: &stubTokenVerifier{
			result: VerifiedToken{
				Actor: Actor{
					Type:           "service",
					ID:             "pipeline",
					OrganizationID: "org-123",
				},
			},
		},
	})

	var seenActor Actor
	handler := middleware.WithAuth("scope:write")(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		var ok bool
		seenActor, ok = ActorFromContext(request.Context())
		if !ok {
			t.Fatal("expected actor in context")
		}
		writer.WriteHeader(http.StatusAccepted)
	}))

	request := httptest.NewRequest(http.MethodPost, "/", nil)
	request.Header.Set("Authorization", "Bearer svc-token")
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusAccepted {
		t.Fatalf("expected status %d, got %d", http.StatusAccepted, recorder.Code)
	}
	if seenActor.Type != "service" || seenActor.ID != "pipeline" {
		t.Fatalf("unexpected actor %#v", seenActor)
	}
}

func TestWithAuthServiceTokenWithoutVerifier(t *testing.T) {
	t.Parallel()

	request := httptest.NewRequest(http.MethodGet, "/", nil)
	request.Header.Set("Authorization", "Bearer service-token")
	recorder := httptest.NewRecorder()

	handler := New(Config{}).WithAuth("scope:read")(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Fatal("next handler should not run")
	}))
	handler.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("expected status %d, got %d", http.StatusUnauthorized, recorder.Code)
	}
}

func TestWithAuthForbiddenServiceTokenError(t *testing.T) {
	t.Parallel()

	forbidden := errors.New("missing scopes")
	middleware := New(Config{
		TokenVerifier: &stubTokenVerifier{err: forbidden},
		IsForbiddenError: func(err error) bool {
			return errors.Is(err, forbidden)
		},
	})

	request := httptest.NewRequest(http.MethodPost, "/", nil)
	request.Header.Set("Authorization", "Bearer svc-token")
	recorder := httptest.NewRecorder()

	handler := middleware.WithAuth("scope:write")(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Fatal("next handler should not run")
	}))
	handler.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusForbidden {
		t.Fatalf("expected status %d, got %d", http.StatusForbidden, recorder.Code)
	}
}

func TestBearerToken(t *testing.T) {
	t.Parallel()

	token, ok := BearerToken("Bearer token-value")
	if !ok || token != "token-value" {
		t.Fatalf("expected bearer token, got ok=%v token=%q", ok, token)
	}
}

func TestHasAllScopes(t *testing.T) {
	t.Parallel()

	if !HasAllScopes([]string{"a", "b"}, []string{"a"}) {
		t.Fatal("expected scope check to pass")
	}
	if HasAllScopes([]string{"a"}, []string{"a", "b"}) {
		t.Fatal("expected scope check to fail")
	}
}

func assertErrorCode(t *testing.T, raw []byte, expected string) {
	t.Helper()

	var response httpkit.ErrorResponse
	if err := json.Unmarshal(raw, &response); err != nil {
		t.Fatalf("decode error response: %v", err)
	}
	if response.Error.Code != expected {
		t.Fatalf("expected error code %q, got %q", expected, response.Error.Code)
	}
}

type stubTokenVerifier struct {
	result VerifiedToken
	err    error
}

func (verifier *stubTokenVerifier) VerifyToken(context.Context, string, []string) (VerifiedToken, error) {
	return verifier.result, verifier.err
}

type stubAPIKeyValidator struct {
	result ValidatedAPIKey
	err    error
}

func (validator *stubAPIKeyValidator) ValidateAPIKey(context.Context, string) (ValidatedAPIKey, error) {
	return validator.result, validator.err
}
