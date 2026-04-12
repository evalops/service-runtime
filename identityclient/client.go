package identityclient

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	identityv1 "github.com/evalops/identity/gen/proto/go/identity/v1"
	"github.com/evalops/service-runtime/mtls"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

var (
	ErrIdentityNotConfigured      = errors.New("identity_not_configured")
	ErrIdentityUnavailable        = errors.New("identity_unavailable")
	ErrInvalidToken               = errors.New("invalid_token")
	ErrInactiveToken              = errors.New("inactive_token")
	ErrServiceTokensNotConfigured = errors.New("identity_service_tokens_not_configured")
)

type Config struct {
	IntrospectURL    string
	ServiceTokensURL string
	BootstrapKey     string
	RequestTimeout   time.Duration
	CacheTTL         time.Duration
	HTTPClient       *http.Client
}

type IntrospectionResult struct {
	Active         bool     `json:"active"`
	AgentType      string   `json:"agent_type,omitempty"`
	Capabilities   []string `json:"capabilities,omitempty"`
	OrganizationID string   `json:"organization_id,omitempty"`
	RunID          string   `json:"run_id,omitempty"`
	Scopes         []string `json:"scopes,omitempty"`
	Service        string   `json:"service,omitempty"`
	Subject        string   `json:"subject,omitempty"`
	Surface        string   `json:"surface,omitempty"`
	TokenType      string   `json:"token_type,omitempty"`
	UserSubject    string   `json:"user_subject,omitempty"`
}

type cachedIntrospection struct {
	expiresAt time.Time
	result    *identityv1.IntrospectionResult
}

type serviceTokenCacheKey struct {
	organizationID string
	service        string
	scopes         string
}

type cachedServiceToken struct {
	expiresAt time.Time
	token     string
}

type Client struct {
	httpClient       *http.Client
	introspectURL    string
	serviceTokensURL string
	bootstrapKey     string
	requestTimeout   time.Duration
	cacheTTL         time.Duration

	cacheLock     sync.Mutex
	introspection map[string]cachedIntrospection

	serviceTokenLock sync.Mutex
	serviceTokens    map[serviceTokenCacheKey]cachedServiceToken
}

func New(config Config) *Client {
	httpClient := config.HTTPClient
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	return &Client{
		httpClient:       httpClient,
		introspectURL:    config.IntrospectURL,
		serviceTokensURL: config.ServiceTokensURL,
		bootstrapKey:     config.BootstrapKey,
		requestTimeout:   config.RequestTimeout,
		cacheTTL:         config.CacheTTL,
		introspection:    make(map[string]cachedIntrospection),
		serviceTokens:    make(map[serviceTokenCacheKey]cachedServiceToken),
	}
}

func NewClient(introspectURL string, requestTimeout time.Duration, httpClient *http.Client) *Client {
	return New(Config{
		IntrospectURL:  introspectURL,
		RequestTimeout: requestTimeout,
		HTTPClient:     httpClient,
	})
}

func NewMTLSClient(introspectURL string, requestTimeout time.Duration, tlsConfig mtls.ClientConfig) (*Client, error) {
	httpClient, err := mtls.BuildHTTPClient(tlsConfig)
	if err != nil {
		return nil, err
	}
	return New(Config{
		IntrospectURL:  introspectURL,
		RequestTimeout: requestTimeout,
		HTTPClient:     httpClient,
	}), nil
}

func (c *Client) Configured() bool {
	return c != nil && c.introspectURL != ""
}

func (c *Client) ServiceTokensConfigured() bool {
	return c != nil && c.serviceTokensURL != "" && c.bootstrapKey != ""
}

func (c *Client) Introspect(ctx context.Context, bearerToken string) (IntrospectionResult, error) {
	result, err := c.IntrospectProto(ctx, bearerToken)
	if err != nil {
		return IntrospectionResult{}, err
	}
	return introspectionResultFromProto(result), nil
}

func (c *Client) IntrospectProto(ctx context.Context, bearerToken string) (*identityv1.IntrospectionResult, error) {
	if !c.Configured() {
		return nil, ErrIdentityNotConfigured
	}
	cached, hasCached := c.cachedIntrospection(bearerToken)

	requestCtx, cancel := context.WithTimeout(ctx, c.requestTimeout)
	defer cancel()

	request, err := http.NewRequestWithContext(
		requestCtx,
		http.MethodPost,
		c.introspectURL,
		bytes.NewReader([]byte(`{}`)),
	)
	if err != nil {
		return nil, fmt.Errorf("build_request: %w", err)
	}
	request.Header.Set("Authorization", "Bearer "+bearerToken)
	request.Header.Set("Content-Type", "application/json")

	response, err := c.httpClient.Do(request)
	if err != nil {
		if hasCached {
			return cached, nil
		}
		return nil, fmt.Errorf("%w: identity_request: %v", ErrIdentityUnavailable, err)
	}
	defer response.Body.Close()

	switch response.StatusCode {
	case http.StatusOK:
	case http.StatusRequestTimeout, http.StatusTooManyRequests:
		if hasCached {
			return cached, nil
		}
		return nil, fmt.Errorf("%w: status_%d", ErrIdentityUnavailable, response.StatusCode)
	default:
		if response.StatusCode >= http.StatusInternalServerError {
			if hasCached {
				return cached, nil
			}
			return nil, fmt.Errorf("%w: status_%d", ErrIdentityUnavailable, response.StatusCode)
		}
		c.evictIntrospection(bearerToken)
		return nil, ErrInvalidToken
	}

	body, err := io.ReadAll(response.Body)
	if err != nil {
		if hasCached {
			return cached, nil
		}
		return nil, fmt.Errorf("%w: read_response: %v", ErrIdentityUnavailable, err)
	}

	var result identityv1.IntrospectionResult
	if err := (protojson.UnmarshalOptions{DiscardUnknown: true}).Unmarshal(body, &result); err != nil {
		if hasCached {
			return cached, nil
		}
		return nil, fmt.Errorf("%w: decode_response: %v", ErrIdentityUnavailable, err)
	}
	if result.Active == nil || !result.GetActive() {
		c.evictIntrospection(bearerToken)
		return nil, ErrInactiveToken
	}

	c.storeIntrospection(bearerToken, &result)
	return cloneIntrospectionResult(&result), nil
}

func (c *Client) IssueServiceToken(
	ctx context.Context,
	organizationID string,
	service string,
	scopes []string,
	ttl time.Duration,
) (*identityv1.ServiceTokenResponse, error) {
	if !c.ServiceTokensConfigured() {
		return nil, ErrServiceTokensNotConfigured
	}

	payload := &identityv1.ServiceTokenRequest{
		Service:        service,
		OrganizationId: organizationID,
		Scopes:         scopes,
		TtlSeconds:     int32(max(int(ttl.Seconds()), 1)),
	}
	body, err := protojson.MarshalOptions{UseProtoNames: true}.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("encode_request: %w", err)
	}

	requestCtx, cancel := context.WithTimeout(ctx, c.requestTimeout)
	defer cancel()

	request, err := http.NewRequestWithContext(
		requestCtx,
		http.MethodPost,
		c.serviceTokensURL,
		bytes.NewReader(body),
	)
	if err != nil {
		return nil, fmt.Errorf("build_request: %w", err)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-Identity-Bootstrap-Key", c.bootstrapKey)

	response, err := c.httpClient.Do(request)
	if err != nil {
		return nil, fmt.Errorf("identity_request: %w", err)
	}
	defer response.Body.Close()

	if response.StatusCode != http.StatusCreated {
		return nil, fmt.Errorf("issue_service_token: unexpected_status_%d", response.StatusCode)
	}

	body, err = io.ReadAll(response.Body)
	if err != nil {
		return nil, fmt.Errorf("read_response: %w", err)
	}

	var result identityv1.ServiceTokenResponse
	if err := (protojson.UnmarshalOptions{DiscardUnknown: true}).Unmarshal(body, &result); err != nil {
		return nil, fmt.Errorf("decode_response: %w", err)
	}
	if strings.TrimSpace(result.GetToken()) == "" {
		return nil, errors.New("missing_service_token")
	}
	return &result, nil
}

func (c *Client) ResolveServiceToken(
	ctx context.Context,
	organizationID string,
	service string,
	scopes []string,
	ttl time.Duration,
) (string, error) {
	if !c.ServiceTokensConfigured() {
		return "", ErrServiceTokensNotConfigured
	}

	key := serviceTokenCacheKey{
		organizationID: strings.TrimSpace(organizationID),
		service:        strings.TrimSpace(service),
		scopes:         cacheScopesKey(scopes),
	}

	c.serviceTokenLock.Lock()
	cached, ok := c.serviceTokens[key]
	if ok && !cached.expiringSoon() {
		c.serviceTokenLock.Unlock()
		return cached.token, nil
	}
	c.serviceTokenLock.Unlock()

	issued, err := c.IssueServiceToken(ctx, key.organizationID, key.service, scopes, ttl)
	if err != nil {
		return "", err
	}
	claims := issued.GetClaims()
	if claims == nil || claims.GetExpiresAt() == nil {
		return "", errors.New("missing_service_token_expiry")
	}

	c.serviceTokenLock.Lock()
	c.serviceTokens[key] = cachedServiceToken{
		token:     issued.GetToken(),
		expiresAt: claims.GetExpiresAt().AsTime(),
	}
	c.serviceTokenLock.Unlock()

	return issued.GetToken(), nil
}

func introspectionResultFromProto(result *identityv1.IntrospectionResult) IntrospectionResult {
	if result == nil {
		return IntrospectionResult{}
	}
	return IntrospectionResult{
		Active:         result.GetActive(),
		AgentType:      result.GetAgentType(),
		Capabilities:   append([]string(nil), result.GetCapabilities()...),
		OrganizationID: result.GetOrganizationId(),
		RunID:          result.GetRunId(),
		Scopes:         append([]string(nil), result.GetScopes()...),
		Service:        result.GetService(),
		Subject:        result.GetSubject(),
		Surface:        result.GetSurface(),
		TokenType:      result.GetTokenType(),
		UserSubject:    result.GetUserSubject(),
	}
}

func (c *Client) cachedIntrospection(bearerToken string) (*identityv1.IntrospectionResult, bool) {
	if c == nil || c.cacheTTL <= 0 || bearerToken == "" {
		return nil, false
	}

	c.cacheLock.Lock()
	defer c.cacheLock.Unlock()

	cached, ok := c.introspection[bearerToken]
	if !ok {
		return nil, false
	}
	if time.Now().After(cached.expiresAt) {
		delete(c.introspection, bearerToken)
		return nil, false
	}
	return cloneIntrospectionResult(cached.result), true
}

func (c *Client) storeIntrospection(bearerToken string, result *identityv1.IntrospectionResult) {
	if c == nil || c.cacheTTL <= 0 || bearerToken == "" || result == nil {
		return
	}

	now := time.Now()
	c.cacheLock.Lock()
	c.pruneExpiredLocked(now)
	c.introspection[bearerToken] = cachedIntrospection{
		expiresAt: now.Add(c.cacheTTL),
		result:    cloneIntrospectionResult(result),
	}
	c.cacheLock.Unlock()
}

func (c *Client) evictIntrospection(bearerToken string) {
	if c == nil || bearerToken == "" {
		return
	}

	c.cacheLock.Lock()
	delete(c.introspection, bearerToken)
	c.cacheLock.Unlock()
}

func (c *Client) pruneExpiredLocked(now time.Time) {
	for bearerToken, cached := range c.introspection {
		if now.After(cached.expiresAt) {
			delete(c.introspection, bearerToken)
		}
	}
}

func cacheScopesKey(scopes []string) string {
	normalized := make([]string, 0, len(scopes))
	for _, scope := range scopes {
		trimmed := strings.TrimSpace(scope)
		if trimmed == "" {
			continue
		}
		normalized = append(normalized, trimmed)
	}
	slices.Sort(normalized)
	deduped := normalized[:0]
	for _, scope := range normalized {
		if len(deduped) != 0 && deduped[len(deduped)-1] == scope {
			continue
		}
		deduped = append(deduped, scope)
	}
	return strings.Join(deduped, "\x00")
}

func cloneIntrospectionResult(result *identityv1.IntrospectionResult) *identityv1.IntrospectionResult {
	if result == nil {
		return nil
	}
	cloned, ok := proto.Clone(result).(*identityv1.IntrospectionResult)
	if !ok {
		return nil
	}
	return cloned
}

func (c cachedServiceToken) expiringSoon() bool {
	return time.Now().Add(10 * time.Second).After(c.expiresAt)
}
