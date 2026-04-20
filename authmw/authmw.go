package authmw

import (
	"context"
	"net/http"
	"slices"
	"strings"

	"github.com/evalops/service-runtime/httpkit"
)

type contextKey string

const actorContextKey contextKey = "actor"

// Actor represents an authenticated principal performing an action.
type Actor struct {
	Type           string            `json:"type"`
	ID             string            `json:"id"`
	OrganizationID string            `json:"organization_id"`
	Attributes     map[string]string `json:"attributes,omitempty"`
}

// VerifiedToken is the result of a successful token verification.
type VerifiedToken struct {
	Actor  Actor
	Scopes []string
}

// ValidatedAPIKey is the result of a successful API key validation.
type ValidatedAPIKey struct {
	ID             string
	OrganizationID string
	Scopes         []string
}

// TokenVerifier verifies bearer tokens against the identity service.
type TokenVerifier interface {
	VerifyToken(ctx context.Context, token string, requiredScopes []string) (VerifiedToken, error)
}

// APIKeyValidator validates API keys against the identity service.
type APIKeyValidator interface {
	ValidateAPIKey(ctx context.Context, token string) (ValidatedAPIKey, error)
}

// Config holds the dependencies for a Middleware.
type Config struct {
	TokenVerifier    TokenVerifier
	APIKeyValidator  APIKeyValidator
	IsForbiddenError func(error) bool
}

// Middleware enforces authentication on HTTP handlers.
type Middleware struct {
	tokenVerifier    TokenVerifier
	apiKeyValidator  APIKeyValidator
	isForbiddenError func(error) bool
}

// New creates a Middleware from the given Config.
func New(config Config) *Middleware {
	return &Middleware{
		tokenVerifier:    config.TokenVerifier,
		apiKeyValidator:  config.APIKeyValidator,
		isForbiddenError: config.IsForbiddenError,
	}
}

// WithAuth returns an HTTP middleware that requires a valid bearer token with the given scopes.
func (middleware *Middleware) WithAuth(scopes ...string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
			token, ok := BearerToken(request.Header.Get("Authorization"))
			if !ok {
				httpkit.WriteError(writer, http.StatusUnauthorized, "missing_authorization", "Authorization bearer token is required")
				return
			}

			actor, availableScopes, err := middleware.authenticate(request.Context(), token, scopes)
			if err != nil {
				status := http.StatusUnauthorized
				if middleware.isForbidden(err) {
					status = http.StatusForbidden
				}
				httpkit.WriteError(writer, status, "authorization_failed", err.Error())
				return
			}

			ctx := context.WithValue(request.Context(), actorContextKey, actor)
			ctx = ContextWithPrincipal(ctx, PrincipalFromActor(actor, availableScopes))
			next.ServeHTTP(writer, request.WithContext(ctx))
		})
	}
}

// ActorFromContext retrieves the authenticated Actor from context.
func ActorFromContext(ctx context.Context) (Actor, bool) {
	actor, ok := ctx.Value(actorContextKey).(Actor)
	return actor, ok
}

// HasAllScopes reports whether all required scopes are present in available.
func HasAllScopes(available []string, required []string) bool {
	for _, requirement := range required {
		if !HasScope(available, requirement) {
			return false
		}
	}
	return true
}

// HasScope reports whether the required scope is present in available scopes.
func HasScope(available []string, required string) bool {
	return required != "" && slices.Contains(available, required)
}

// BearerToken extracts the token from an Authorization: Bearer <token> header.
func BearerToken(header string) (string, bool) {
	parts := strings.SplitN(header, " ", 2)
	if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") {
		return "", false
	}
	token := strings.TrimSpace(parts[1])
	return token, token != ""
}

func (middleware *Middleware) authenticate(ctx context.Context, token string, scopes []string) (Actor, []string, error) {
	if strings.HasPrefix(token, "pk_") {
		return middleware.authenticateAPIKey(ctx, token, scopes)
	}
	return middleware.authenticateToken(ctx, token, scopes)
}

func (middleware *Middleware) authenticateAPIKey(ctx context.Context, token string, scopes []string) (Actor, []string, error) {
	if middleware == nil || middleware.apiKeyValidator == nil {
		return Actor{}, nil, errAPIKeyValidatorUnavailable
	}
	key, err := middleware.apiKeyValidator.ValidateAPIKey(ctx, token)
	if err != nil {
		return Actor{}, nil, err
	}
	if !HasAllScopes(key.Scopes, scopes) {
		return Actor{}, nil, errMissingScopes
	}

	return Actor{
		Type:           "api_key",
		ID:             key.ID,
		OrganizationID: key.OrganizationID,
	}, append([]string(nil), key.Scopes...), nil
}

func (middleware *Middleware) authenticateToken(ctx context.Context, token string, scopes []string) (Actor, []string, error) {
	if middleware == nil || middleware.tokenVerifier == nil {
		return Actor{}, nil, errTokenVerifierUnavailable
	}
	verified, err := middleware.tokenVerifier.VerifyToken(ctx, token, scopes)
	if err != nil {
		return Actor{}, nil, err
	}
	return verified.Actor, append([]string(nil), verified.Scopes...), nil
}

func (middleware *Middleware) isForbidden(err error) bool {
	if err == nil {
		return false
	}
	if middleware != nil && middleware.isForbiddenError != nil {
		return middleware.isForbiddenError(err)
	}
	return err == errMissingScopes
}
