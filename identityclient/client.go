package identityclient

import (
	"bytes"
	"container/list"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	identityv1 "github.com/evalops/proto/gen/go/identity/v1"
	"github.com/evalops/service-runtime/mtls"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// ErrIdentityNotConfigured and related errors are returned by Client methods.
var (
	ErrIdentityNotConfigured      = errors.New("identity_not_configured")
	ErrIdentityUnavailable        = errors.New("identity_unavailable")
	ErrInvalidToken               = errors.New("invalid_token")
	ErrInactiveToken              = errors.New("inactive_token")
	ErrServiceTokensNotConfigured = errors.New("identity_service_tokens_not_configured")
)

const defaultMaxCacheSize = 10000

// Config holds the settings for constructing an identity client.
type Config struct {
	IntrospectURL    string
	ServiceTokensURL string
	BootstrapKey     string
	RequestTimeout   time.Duration
	CacheTTL         time.Duration
	MaxCacheSize     int // max entries per cache (default: 10000)
	HTTPClient       *http.Client
}

// IntrospectionResult is the decoded response from a token introspection call.
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
	result    *identityv1.IntrospectResponse
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

// ServiceTokenClaims holds expiry metadata embedded in a service token response.
type ServiceTokenClaims struct {
	ExpiresAt time.Time `json:"expires_at,omitempty"`
}

// GetExpiresAt returns the expiry as a protobuf Timestamp.
func (c *ServiceTokenClaims) GetExpiresAt() *timestamppb.Timestamp {
	if c == nil || c.ExpiresAt.IsZero() {
		return nil
	}
	return timestamppb.New(c.ExpiresAt)
}

// ServiceTokenResponse is the decoded response from a service token issuance call.
type ServiceTokenResponse struct {
	Token     string              `json:"token,omitempty"`
	TokenType string              `json:"token_type,omitempty"`
	ExpiresAt time.Time           `json:"expires_at,omitempty"`
	Claims    *ServiceTokenClaims `json:"claims,omitempty"`
}

// GetToken returns the issued token string.
func (r *ServiceTokenResponse) GetToken() string {
	if r == nil {
		return ""
	}
	return r.Token
}

// GetExpiresAt returns the token expiry as a protobuf Timestamp.
func (r *ServiceTokenResponse) GetExpiresAt() *timestamppb.Timestamp {
	if r == nil || r.ExpiresAt.IsZero() {
		return nil
	}
	return timestamppb.New(r.ExpiresAt)
}

// GetClaims returns the embedded claims from the service token response.
func (r *ServiceTokenResponse) GetClaims() *ServiceTokenClaims {
	if r == nil {
		return nil
	}
	return r.Claims
}

func (r *ServiceTokenResponse) expiryTime() (time.Time, bool) {
	if r == nil {
		return time.Time{}, false
	}
	if !r.ExpiresAt.IsZero() {
		return r.ExpiresAt, true
	}
	if r.Claims != nil && !r.Claims.ExpiresAt.IsZero() {
		return r.Claims.ExpiresAt, true
	}
	return time.Time{}, false
}

// Client is an identity service client that introspects tokens and issues service tokens.
type Client struct {
	httpClient       *http.Client
	introspectURL    string
	serviceTokensURL string
	bootstrapKey     string
	requestTimeout   time.Duration
	cacheTTL         time.Duration

	cacheLock     sync.Mutex
	introspection *lruCache[string, cachedIntrospection]

	serviceTokenLock sync.Mutex
	serviceTokens    *lruCache[serviceTokenCacheKey, cachedServiceToken]
}

// New creates a Client from the given Config.
func New(config Config) *Client {
	httpClient := config.HTTPClient
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	maxSize := config.MaxCacheSize
	if maxSize <= 0 {
		maxSize = defaultMaxCacheSize
	}
	return &Client{
		httpClient:       httpClient,
		introspectURL:    config.IntrospectURL,
		serviceTokensURL: config.ServiceTokensURL,
		bootstrapKey:     config.BootstrapKey,
		requestTimeout:   config.RequestTimeout,
		cacheTTL:         config.CacheTTL,
		introspection:    newLRUCache[string, cachedIntrospection](maxSize),
		serviceTokens:    newLRUCache[serviceTokenCacheKey, cachedServiceToken](maxSize),
	}
}

// NewClient creates a Client that introspects tokens at the given URL.
func NewClient(introspectURL string, requestTimeout time.Duration, httpClient *http.Client) *Client {
	return New(Config{
		IntrospectURL:  introspectURL,
		RequestTimeout: requestTimeout,
		HTTPClient:     httpClient,
	})
}

// NewMTLSClient creates a Client that uses mTLS for its HTTP transport.
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

// Configured reports whether the client has an introspect URL set.
func (c *Client) Configured() bool {
	return c != nil && c.introspectURL != ""
}

// ServiceTokensConfigured reports whether the client can issue service tokens.
func (c *Client) ServiceTokensConfigured() bool {
	return c != nil && c.serviceTokensURL != "" && (strings.TrimSpace(c.bootstrapKey) != "" || c.usesMTLSClientCertificate())
}

// Introspect verifies a bearer token and returns the decoded result.
func (c *Client) Introspect(ctx context.Context, bearerToken string) (IntrospectionResult, error) {
	result, err := c.IntrospectProto(ctx, bearerToken)
	if err != nil {
		return IntrospectionResult{}, err
	}
	return introspectionResultFromProto(result), nil
}

// IntrospectProto is like Introspect but returns the raw proto response.
func (c *Client) IntrospectProto(ctx context.Context, bearerToken string) (*identityv1.IntrospectResponse, error) {
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
	defer func() { _ = response.Body.Close() }()

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

	var result identityv1.IntrospectResponse
	if err := (protojson.UnmarshalOptions{DiscardUnknown: true}).Unmarshal(body, &result); err != nil {
		if hasCached {
			return cached, nil
		}
		return nil, fmt.Errorf("%w: decode_response: %v", ErrIdentityUnavailable, err)
	}
	if !result.GetActive() {
		c.evictIntrospection(bearerToken)
		return nil, ErrInactiveToken
	}

	c.storeIntrospection(bearerToken, &result)
	return cloneIntrospectionResult(&result), nil
}

// IssueServiceToken requests a new service token from the identity service.
func (c *Client) IssueServiceToken(
	ctx context.Context,
	organizationID string,
	service string,
	scopes []string,
	ttl time.Duration,
) (*ServiceTokenResponse, error) {
	if !c.ServiceTokensConfigured() {
		return nil, ErrServiceTokensNotConfigured
	}

	payload := &identityv1.IssueServiceTokenRequest{
		Service:        service,
		OrganizationId: organizationID,
		Scopes:         scopes,
		TtlSeconds:     clampToInt32(max(int64(ttl.Seconds()), 1)),
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
	if strings.TrimSpace(c.bootstrapKey) != "" {
		request.Header.Set("X-Identity-Bootstrap-Key", c.bootstrapKey)
	}

	response, err := c.httpClient.Do(request)
	if err != nil {
		return nil, fmt.Errorf("identity_request: %w", err)
	}
	defer func() { _ = response.Body.Close() }()

	if response.StatusCode != http.StatusCreated {
		return nil, fmt.Errorf("issue_service_token: unexpected_status_%d", response.StatusCode)
	}

	body, err = io.ReadAll(response.Body)
	if err != nil {
		return nil, fmt.Errorf("read_response: %w", err)
	}

	var result ServiceTokenResponse
	if err := json.Unmarshal(body, &result); err != nil {
		return nil, fmt.Errorf("decode_response: %w", err)
	}
	if strings.TrimSpace(result.GetToken()) == "" {
		return nil, errors.New("missing_service_token")
	}
	return &result, nil
}

// ResolveServiceToken returns a cached service token or issues a new one.
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
	cached, ok := c.serviceTokens.Get(key)
	if ok && !cached.expiringSoon() {
		c.serviceTokenLock.Unlock()
		return cached.token, nil
	}
	c.serviceTokenLock.Unlock()

	issued, err := c.IssueServiceToken(ctx, key.organizationID, key.service, scopes, ttl)
	if err != nil {
		return "", err
	}
	expiresAt, ok := issued.expiryTime()
	if !ok {
		return "", errors.New("missing_service_token_expiry")
	}

	c.serviceTokenLock.Lock()
	c.serviceTokens.Put(key, cachedServiceToken{
		token:     issued.GetToken(),
		expiresAt: expiresAt,
	})
	c.serviceTokenLock.Unlock()

	return issued.GetToken(), nil
}

func (c *Client) usesMTLSClientCertificate() bool {
	if c == nil || c.httpClient == nil {
		return false
	}

	transport, ok := c.httpClient.Transport.(*http.Transport)
	if !ok || transport == nil {
		return false
	}
	return tlsConfigHasClientCertificate(transport.TLSClientConfig)
}

func tlsConfigHasClientCertificate(cfg *tls.Config) bool {
	if cfg == nil {
		return false
	}
	return len(cfg.Certificates) > 0 || cfg.GetClientCertificate != nil
}

func introspectionResultFromProto(result *identityv1.IntrospectResponse) IntrospectionResult {
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

func (c *Client) cachedIntrospection(bearerToken string) (*identityv1.IntrospectResponse, bool) {
	if c == nil || c.cacheTTL <= 0 || bearerToken == "" {
		return nil, false
	}

	c.cacheLock.Lock()
	defer c.cacheLock.Unlock()

	cached, ok := c.introspection.Get(bearerToken)
	if !ok {
		return nil, false
	}
	if time.Now().After(cached.expiresAt) {
		c.introspection.Delete(bearerToken)
		return nil, false
	}
	return cloneIntrospectionResult(cached.result), true
}

func (c *Client) storeIntrospection(bearerToken string, result *identityv1.IntrospectResponse) {
	if c == nil || c.cacheTTL <= 0 || bearerToken == "" || result == nil {
		return
	}

	now := time.Now()
	c.cacheLock.Lock()
	c.pruneExpiredLocked(now)
	c.introspection.Put(bearerToken, cachedIntrospection{
		expiresAt: now.Add(c.cacheTTL),
		result:    cloneIntrospectionResult(result),
	})
	c.cacheLock.Unlock()
}

func (c *Client) evictIntrospection(bearerToken string) {
	if c == nil || bearerToken == "" {
		return
	}

	c.cacheLock.Lock()
	c.introspection.Delete(bearerToken)
	c.cacheLock.Unlock()
}

func (c *Client) pruneExpiredLocked(now time.Time) {
	c.introspection.Range(func(bearerToken string, cached cachedIntrospection) bool {
		if now.After(cached.expiresAt) {
			c.introspection.Delete(bearerToken)
		}
		return true
	})
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

func cloneIntrospectionResult(result *identityv1.IntrospectResponse) *identityv1.IntrospectResponse {
	if result == nil {
		return nil
	}
	cloned, ok := proto.Clone(result).(*identityv1.IntrospectResponse)
	if !ok {
		return nil
	}
	return cloned
}

// clampToInt32 converts v to int32, clamping at math.MaxInt32 to avoid overflow.
func clampToInt32(v int64) int32 {
	const maxInt32 int64 = math.MaxInt32
	if v > maxInt32 {
		return math.MaxInt32
	}
	if v < 0 {
		return 0
	}
	return int32(v) //nolint:gosec // G115: bounds checked above
}

func (c cachedServiceToken) expiringSoon() bool {
	return time.Now().Add(10 * time.Second).After(c.expiresAt)
}

// lruCache is a fixed-size LRU cache backed by container/list and a map.
type lruCache[K comparable, V any] struct {
	maxSize int
	items   map[K]*list.Element
	order   *list.List // front = most recently used, back = least recently used
}

type lruEntry[K comparable, V any] struct {
	key   K
	value V
}

func newLRUCache[K comparable, V any](maxSize int) *lruCache[K, V] {
	return &lruCache[K, V]{
		maxSize: maxSize,
		items:   make(map[K]*list.Element),
		order:   list.New(),
	}
}

// Get retrieves a value and promotes it to the front (most recently used).
func (c *lruCache[K, V]) Get(key K) (V, bool) {
	elem, ok := c.items[key]
	if !ok {
		var zero V
		return zero, false
	}
	c.order.MoveToFront(elem)
	entry, ok := elem.Value.(*lruEntry[K, V])
	if !ok {
		var zero V
		return zero, false
	}
	return entry.value, true
}

// Put adds or updates a value, promoting it to the front. If the cache exceeds
// maxSize, the least recently used entry is evicted.
func (c *lruCache[K, V]) Put(key K, value V) {
	if elem, ok := c.items[key]; ok {
		entry, ok := elem.Value.(*lruEntry[K, V])
		if !ok {
			return
		}
		entry.value = value
		c.order.MoveToFront(elem)
		return
	}
	entry := &lruEntry[K, V]{key: key, value: value}
	elem := c.order.PushFront(entry)
	c.items[key] = elem

	for c.order.Len() > c.maxSize {
		back := c.order.Back()
		if back == nil {
			break
		}
		evicted, ok := c.order.Remove(back).(*lruEntry[K, V])
		if !ok {
			continue
		}
		delete(c.items, evicted.key)
	}
}

// Delete removes a key from the cache.
func (c *lruCache[K, V]) Delete(key K) {
	elem, ok := c.items[key]
	if !ok {
		return
	}
	c.order.Remove(elem)
	delete(c.items, key)
}

// Len returns the number of entries in the cache.
func (c *lruCache[K, V]) Len() int {
	return c.order.Len()
}

// MaxSize returns the configured maximum size of the cache.
func (c *lruCache[K, V]) MaxSize() int {
	return c.maxSize
}

// Range iterates over all entries from most to least recently used.
// If fn returns false, iteration stops. It is safe for fn to call Delete.
func (c *lruCache[K, V]) Range(fn func(K, V) bool) {
	// Collect keys first to allow safe deletion during iteration.
	type kv struct {
		key   K
		value V
	}
	entries := make([]kv, 0, c.order.Len())
	for elem := c.order.Front(); elem != nil; elem = elem.Next() {
		entry, ok := elem.Value.(*lruEntry[K, V])
		if !ok {
			continue
		}
		entries = append(entries, kv{key: entry.key, value: entry.value})
	}
	for _, e := range entries {
		if !fn(e.key, e.value) {
			return
		}
	}
}
