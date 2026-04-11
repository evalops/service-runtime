package authmw

import (
	"context"
	"net/http"
	"strings"

	"github.com/evalops/service-runtime/httpkit"
)

type contextKey string

const actorContextKey contextKey = "actor"

type Actor struct {
	Type           string            `json:"type"`
	ID             string            `json:"id"`
	OrganizationID string            `json:"organization_id"`
	Attributes     map[string]string `json:"attributes,omitempty"`
}

type VerifiedToken struct {
	Actor  Actor
	Scopes []string
}

type ValidatedAPIKey struct {
	ID             string
	OrganizationID string
	Scopes         []string
}

type TokenVerifier interface {
	VerifyToken(ctx context.Context, token string, requiredScopes []string) (VerifiedToken, error)
}

type APIKeyValidator interface {
	ValidateAPIKey(ctx context.Context, token string) (ValidatedAPIKey, error)
}

type Config struct {
	TokenVerifier    TokenVerifier
	APIKeyValidator  APIKeyValidator
	IsForbiddenError func(error) bool
}

type Middleware struct {
	tokenVerifier    TokenVerifier
	apiKeyValidator  APIKeyValidator
	isForbiddenError func(error) bool
}

func New(config Config) *Middleware {
	return &Middleware{
		tokenVerifier:    config.TokenVerifier,
		apiKeyValidator:  config.APIKeyValidator,
		isForbiddenError: config.IsForbiddenError,
	}
}

func (middleware *Middleware) WithAuth(scopes ...string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
			token, ok := BearerToken(request.Header.Get("Authorization"))
			if !ok {
				httpkit.WriteError(writer, http.StatusUnauthorized, "missing_authorization", "Authorization bearer token is required")
				return
			}

			actor, err := middleware.authenticate(request.Context(), token, scopes)
			if err != nil {
				status := http.StatusUnauthorized
				if middleware.isForbidden(err) {
					status = http.StatusForbidden
				}
				httpkit.WriteError(writer, status, "authorization_failed", err.Error())
				return
			}

			ctx := context.WithValue(request.Context(), actorContextKey, actor)
			next.ServeHTTP(writer, request.WithContext(ctx))
		})
	}
}

func ActorFromContext(ctx context.Context) (Actor, bool) {
	actor, ok := ctx.Value(actorContextKey).(Actor)
	return actor, ok
}

func HasAllScopes(available []string, required []string) bool {
	for _, requirement := range required {
		found := false
		for _, scope := range available {
			if scope == requirement {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

func BearerToken(header string) (string, bool) {
	parts := strings.SplitN(header, " ", 2)
	if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") {
		return "", false
	}
	token := strings.TrimSpace(parts[1])
	return token, token != ""
}

func (middleware *Middleware) authenticate(ctx context.Context, token string, scopes []string) (Actor, error) {
	if strings.HasPrefix(token, "pk_") {
		return middleware.authenticateAPIKey(ctx, token, scopes)
	}
	return middleware.authenticateToken(ctx, token, scopes)
}

func (middleware *Middleware) authenticateAPIKey(ctx context.Context, token string, scopes []string) (Actor, error) {
	if middleware == nil || middleware.apiKeyValidator == nil {
		return Actor{}, errAPIKeyValidatorUnavailable
	}
	key, err := middleware.apiKeyValidator.ValidateAPIKey(ctx, token)
	if err != nil {
		return Actor{}, err
	}
	if !HasAllScopes(key.Scopes, scopes) {
		return Actor{}, errMissingScopes
	}

	return Actor{
		Type:           "api_key",
		ID:             key.ID,
		OrganizationID: key.OrganizationID,
	}, nil
}

func (middleware *Middleware) authenticateToken(ctx context.Context, token string, scopes []string) (Actor, error) {
	if middleware == nil || middleware.tokenVerifier == nil {
		return Actor{}, errTokenVerifierUnavailable
	}
	verified, err := middleware.tokenVerifier.VerifyToken(ctx, token, scopes)
	if err != nil {
		return Actor{}, err
	}
	return verified.Actor, nil
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
