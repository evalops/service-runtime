package identityclient

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	identityv1 "github.com/evalops/proto/gen/go/identity/v1"
	"github.com/evalops/service-runtime/mtls"
	"github.com/evalops/service-runtime/testutil"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (fn roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return fn(req)
}

func TestNewMTLSClientUsesDefaultHTTPClientWhenTLSIsUnset(t *testing.T) {
	client, err := NewMTLSClient("https://identity.internal/v1/tokens/introspect", 2*time.Second, mtls.ClientConfig{})
	if err != nil {
		t.Fatalf("new mtls client: %v", err)
	}
	if client.httpClient != http.DefaultClient {
		t.Fatal("expected default client when tls is unset")
	}
}

func TestConfigured(t *testing.T) {
	if NewClient("", time.Second, nil).Configured() {
		t.Fatal("expected empty client to be unconfigured")
	}
	if !NewClient("https://identity.internal/v1/tokens/introspect", time.Second, nil).Configured() {
		t.Fatal("expected configured client")
	}
	if !New(Config{ServiceTokensURL: "https://identity.internal/v1/service-tokens", BootstrapKey: "bootstrap"}).ServiceTokensConfigured() { //nolint:gosec // G101: test credential
		t.Fatal("expected service tokens to be configured")
	}
	if !New(Config{ //nolint:gosec // G101: test credential
		ServiceTokensURL: "https://identity.internal/v1/service-tokens",
		HTTPClient: &http.Client{
			Transport: &http.Transport{
				TLSClientConfig: &tls.Config{Certificates: []tls.Certificate{{}}},
			},
		},
	}).ServiceTokensConfigured() {
		t.Fatal("expected mtls-authenticated service tokens to be configured")
	}
	if New(Config{ //nolint:gosec // G101: test credential
		ServiceTokensURL: "https://identity.internal/v1/service-tokens",
		BootstrapKey:     "   ",
	}).ServiceTokensConfigured() {
		t.Fatal("expected whitespace-only bootstrap key to be treated as unset")
	}
}

func TestIntrospectSuccess(t *testing.T) {
	server := testutil.NewTestServer(t, http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if got := request.Header.Get("Authorization"); got != "Bearer write-token" {
			t.Fatalf("unexpected authorization header: %q", got)
		}
		testutil.WriteJSON(t, writer, http.StatusOK, map[string]any{
			"active":          true,
			"organization_id": "org_123",
			"scopes":          []string{"audit:write"},
			"token_type":      "agent",
			"user_subject":    "user-123",
			"agent_type":      "claude-code",
			"capabilities":    []string{"bash", "git"},
			"surface":         "cli",
			"run_id":          "run_123",
		})
	}))

	client := NewClient(server.URL, time.Second, server.Client())
	result, err := client.Introspect(context.Background(), "write-token")
	if err != nil {
		t.Fatalf("introspect: %v", err)
	}
	if !result.Active {
		t.Fatal("expected active token")
	}
	if result.OrganizationID != "org_123" {
		t.Fatalf("unexpected org id %q", result.OrganizationID)
	}
	if result.TokenType != "agent" {
		t.Fatalf("unexpected token type %q", result.TokenType)
	}
	if result.UserSubject != "user-123" {
		t.Fatalf("unexpected user subject %q", result.UserSubject)
	}
	if result.AgentType != "claude-code" {
		t.Fatalf("unexpected agent type %q", result.AgentType)
	}
	if len(result.Capabilities) != 2 || result.Capabilities[0] != "bash" || result.Capabilities[1] != "git" {
		t.Fatalf("unexpected capabilities %#v", result.Capabilities)
	}
	if result.Surface != "cli" {
		t.Fatalf("unexpected surface %q", result.Surface)
	}
	if result.RunID != "run_123" {
		t.Fatalf("unexpected run id %q", result.RunID)
	}
}

func TestIntrospectProtoSuccess(t *testing.T) {
	server := testutil.NewTestServer(t, http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		testutil.WriteJSON(t, writer, http.StatusOK, map[string]any{
			"active":          true,
			"organization_id": "org_123",
			"scopes":          []string{"audit:write"},
			"token_type":      "agent",
			"user_subject":    "user-123",
			"agent_type":      "claude-code",
			"capabilities":    []string{"bash", "git"},
			"surface":         "cli",
			"run_id":          "run_123",
		})
	}))

	client := NewClient(server.URL, time.Second, server.Client())
	result, err := client.IntrospectProto(context.Background(), "write-token")
	if err != nil {
		t.Fatalf("introspect proto: %v", err)
	}
	if !result.GetActive() {
		t.Fatal("expected active token")
	}
	if result.GetOrganizationId() != "org_123" {
		t.Fatalf("unexpected org id %q", result.GetOrganizationId())
	}
	if result.GetRunId() != "run_123" {
		t.Fatalf("unexpected run id %q", result.GetRunId())
	}
}

func TestIntrospectFallsBackToCachedResultOnIdentityOutage(t *testing.T) {
	t.Parallel()

	var calls int
	httpClient := &http.Client{
		Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			calls++
			if calls == 1 {
				return &http.Response{
					StatusCode: http.StatusOK,
					Body: io.NopCloser(strings.NewReader(
						`{"active":true,"organization_id":"org-123","scopes":["llm_gateway:invoke"]}`,
					)),
					Header: make(http.Header),
				}, nil
			}
			return nil, context.DeadlineExceeded
		}),
	}

	client := New(Config{
		IntrospectURL:  "https://identity.test/v1/tokens/introspect",
		RequestTimeout: time.Second,
		CacheTTL:       time.Minute,
		HTTPClient:     httpClient,
	})

	first, err := client.Introspect(context.Background(), "user-token")
	if err != nil {
		t.Fatalf("first introspection: %v", err)
	}
	second, err := client.Introspect(context.Background(), "user-token")
	if err != nil {
		t.Fatalf("second introspection: %v", err)
	}

	if first.OrganizationID != "org-123" || second.OrganizationID != "org-123" {
		t.Fatalf("expected cached organization scope, got %q and %q", first.OrganizationID, second.OrganizationID)
	}
	if calls != 2 {
		t.Fatalf("expected two identity attempts, got %d", calls)
	}
}

func TestIntrospectReturnsInvalidTokenForUnauthorized(t *testing.T) {
	server := testutil.NewTestServer(t, http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.WriteHeader(http.StatusUnauthorized)
	}))

	client := NewClient(server.URL, time.Second, server.Client())
	_, err := client.Introspect(context.Background(), "bad-token")
	if !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("expected ErrInvalidToken, got %v", err)
	}
}

func TestIntrospectReturnsInactiveToken(t *testing.T) {
	server := testutil.NewTestServer(t, http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		testutil.WriteJSON(t, writer, http.StatusOK, map[string]any{"active": false})
	}))

	client := NewClient(server.URL, time.Second, server.Client())
	_, err := client.Introspect(context.Background(), "inactive-token")
	if !errors.Is(err, ErrInactiveToken) {
		t.Fatalf("expected ErrInactiveToken, got %v", err)
	}
}

func TestIntrospectReturnsIdentityUnavailableForTransportErrors(t *testing.T) {
	client := NewClient("http://127.0.0.1:1/v1/tokens/introspect", 100*time.Millisecond, http.DefaultClient)
	_, err := client.Introspect(context.Background(), "write-token")
	if err == nil || !errors.Is(err, ErrIdentityUnavailable) {
		t.Fatalf("expected ErrIdentityUnavailable, got %v", err)
	}
}

func TestIntrospectReturnsIdentityUnavailableForBadJSON(t *testing.T) {
	server := testutil.NewTestServer(t, http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(`{not-json}`))
	}))

	client := NewClient(server.URL, time.Second, server.Client())
	_, err := client.Introspect(context.Background(), "write-token")
	if err == nil || !errors.Is(err, ErrIdentityUnavailable) {
		t.Fatalf("expected ErrIdentityUnavailable, got %v", err)
	}
	if !strings.Contains(err.Error(), "decode_response") {
		t.Fatalf("expected decode_response in error, got %v", err)
	}
}

func TestIntrospectDoesNotUseCacheForInvalidTokenResponses(t *testing.T) {
	t.Parallel()

	var calls int
	httpClient := &http.Client{
		Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			calls++
			switch calls {
			case 1:
				return &http.Response{
					StatusCode: http.StatusOK,
					Body: io.NopCloser(strings.NewReader(
						`{"active":true,"organization_id":"org-123","scopes":["llm_gateway:invoke"]}`,
					)),
					Header: make(http.Header),
				}, nil
			case 2:
				return &http.Response{
					StatusCode: http.StatusUnauthorized,
					Body:       io.NopCloser(strings.NewReader(`{"error":"invalid_token"}`)),
					Header:     make(http.Header),
				}, nil
			default:
				return nil, context.DeadlineExceeded
			}
		}),
	}

	client := New(Config{
		IntrospectURL:  "https://identity.test/v1/tokens/introspect",
		RequestTimeout: time.Second,
		CacheTTL:       time.Minute,
		HTTPClient:     httpClient,
	})

	if _, err := client.Introspect(context.Background(), "user-token"); err != nil {
		t.Fatalf("seed introspection: %v", err)
	}
	if _, err := client.Introspect(context.Background(), "user-token"); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("expected invalid token error, got %v", err)
	}
	if _, err := client.Introspect(context.Background(), "user-token"); !errors.Is(err, ErrIdentityUnavailable) {
		t.Fatalf("expected cache eviction after invalid token, got %v", err)
	}
}

func TestIntrospectDoesNotUseCacheForInactiveTokenResponses(t *testing.T) {
	t.Parallel()

	var calls int
	httpClient := &http.Client{
		Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			calls++
			switch calls {
			case 1:
				return &http.Response{
					StatusCode: http.StatusOK,
					Body: io.NopCloser(strings.NewReader(
						`{"active":true,"organization_id":"org-123","scopes":["llm_gateway:invoke"]}`,
					)),
					Header: make(http.Header),
				}, nil
			case 2:
				return &http.Response{
					StatusCode: http.StatusOK,
					Body:       io.NopCloser(strings.NewReader(`{"active":false}`)),
					Header:     make(http.Header),
				}, nil
			default:
				return nil, context.DeadlineExceeded
			}
		}),
	}

	client := New(Config{
		IntrospectURL:  "https://identity.test/v1/tokens/introspect",
		RequestTimeout: time.Second,
		CacheTTL:       time.Minute,
		HTTPClient:     httpClient,
	})

	if _, err := client.Introspect(context.Background(), "user-token"); err != nil {
		t.Fatalf("seed introspection: %v", err)
	}
	if _, err := client.Introspect(context.Background(), "user-token"); !errors.Is(err, ErrInactiveToken) {
		t.Fatalf("expected inactive token error, got %v", err)
	}
	if _, err := client.Introspect(context.Background(), "user-token"); !errors.Is(err, ErrIdentityUnavailable) {
		t.Fatalf("expected cache eviction after inactive token, got %v", err)
	}
}

func TestStoreIntrospectionPrunesExpiredEntries(t *testing.T) {
	t.Parallel()

	client := New(Config{CacheTTL: time.Minute})
	client.introspection.Put("expired-token", cachedIntrospection{
		expiresAt: time.Now().Add(-time.Minute),
		result:    protoIntrospectionResult(true, "expired-org"),
	})

	client.storeIntrospection("fresh-token", protoIntrospectionResult(true, "fresh-org"))

	if _, ok := client.introspection.Get("expired-token"); ok {
		t.Fatal("expected expired cache entry to be pruned on store")
	}
	fresh, ok := client.introspection.Get("fresh-token")
	if !ok {
		t.Fatal("expected fresh cache entry to be stored")
	}
	if fresh.result.GetOrganizationId() != "fresh-org" {
		t.Fatalf("expected fresh cache entry to round-trip, got %q", fresh.result.GetOrganizationId())
	}
}

func TestResolveServiceTokenIssuesAndCachesByScopeSet(t *testing.T) {
	t.Parallel()

	var identityCalls atomic.Int32
	identityServer := testutil.NewTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		identityCalls.Add(1)
		testutil.WriteJSON(t, w, http.StatusCreated, map[string]any{
			"token":      "identity-service-token",
			"token_type": "Bearer",
			"claims": map[string]any{
				"expires_at": time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
			},
		})
	}))

	client := New(Config{
		ServiceTokensURL: identityServer.URL + "/v1/service-tokens",
		BootstrapKey:     "identity-bootstrap-key",
		RequestTimeout:   time.Second,
		CacheTTL:         time.Minute,
		HTTPClient:       http.DefaultClient,
	})

	first, err := client.ResolveServiceToken(
		context.Background(),
		"org-123",
		"llm-gateway",
		[]string{"provider_refs:read", "provider_refs:read", " keys:read "},
		time.Minute,
	)
	if err != nil {
		t.Fatalf("first resolve service token: %v", err)
	}
	second, err := client.ResolveServiceToken(
		context.Background(),
		"org-123",
		"llm-gateway",
		[]string{"keys:read", "provider_refs:read"},
		time.Minute,
	)
	if err != nil {
		t.Fatalf("second resolve service token: %v", err)
	}

	if first != "identity-service-token" || second != "identity-service-token" {
		t.Fatalf("expected cached service token, got %q and %q", first, second)
	}
	if identityCalls.Load() != 1 {
		t.Fatalf("expected one identity token issuance, got %d", identityCalls.Load())
	}
}

func TestResolveServiceTokenRefreshesExpiringTokens(t *testing.T) {
	t.Parallel()

	var identityCalls atomic.Int32
	identityServer := testutil.NewTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		call := identityCalls.Add(1)
		testutil.WriteJSON(t, w, http.StatusCreated, map[string]any{
			"token":      fmt.Sprintf("identity-service-token-%d", call),
			"token_type": "Bearer",
			"claims": map[string]any{
				"expires_at": time.Now().Add(5 * time.Second).UTC().Format(time.RFC3339),
			},
		})
	}))

	client := New(Config{
		ServiceTokensURL: identityServer.URL + "/v1/service-tokens",
		BootstrapKey:     "identity-bootstrap-key",
		RequestTimeout:   time.Second,
		CacheTTL:         time.Minute,
		HTTPClient:       http.DefaultClient,
	})

	first, err := client.ResolveServiceToken(
		context.Background(),
		"org-123",
		"llm-gateway",
		[]string{"provider_refs:read"},
		time.Minute,
	)
	if err != nil {
		t.Fatalf("first resolve service token: %v", err)
	}
	second, err := client.ResolveServiceToken(
		context.Background(),
		"org-123",
		"llm-gateway",
		[]string{"provider_refs:read"},
		time.Minute,
	)
	if err != nil {
		t.Fatalf("second resolve service token: %v", err)
	}

	if first == second {
		t.Fatalf("expected expiring token to be refreshed, got %q twice", first)
	}
	if identityCalls.Load() != 2 {
		t.Fatalf("expected two identity token issuances, got %d", identityCalls.Load())
	}
}

func TestIssueServiceTokenAllowsMTLSWithoutBootstrapKey(t *testing.T) {
	t.Parallel()

	identityServer := testutil.NewTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("X-Identity-Bootstrap-Key"); got != "" {
			t.Fatalf("expected no bootstrap key header, got %q", got)
		}
		testutil.WriteJSON(t, w, http.StatusCreated, map[string]any{
			"token":      "identity-service-token",
			"token_type": "Bearer",
			"claims": map[string]any{
				"expires_at": time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
			},
		})
	}))

	client := New(Config{
		ServiceTokensURL: identityServer.URL + "/v1/service-tokens",
		RequestTimeout:   time.Second,
		HTTPClient: &http.Client{
			Transport: &http.Transport{
				TLSClientConfig: &tls.Config{Certificates: []tls.Certificate{{}}},
			},
		},
	})

	issued, err := client.IssueServiceToken(
		context.Background(),
		"org-123",
		"llm-gateway",
		[]string{"provider_refs:read"},
		time.Minute,
	)
	if err != nil {
		t.Fatalf("issue service token: %v", err)
	}
	if issued.GetToken() != "identity-service-token" {
		t.Fatalf("unexpected issued token %q", issued.GetToken())
	}
}

func TestIntrospectionCacheEvictsLeastRecentlyUsedWhenFull(t *testing.T) {
	t.Parallel()

	callCount := 0
	httpClient := &http.Client{
		Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			callCount++
			token := strings.TrimPrefix(req.Header.Get("Authorization"), "Bearer ")
			return &http.Response{
				StatusCode: http.StatusOK,
				Body: io.NopCloser(strings.NewReader(
					fmt.Sprintf(`{"active":true,"organization_id":"org-%s"}`, token),
				)),
				Header: make(http.Header),
			}, nil
		}),
	}

	client := New(Config{
		IntrospectURL:  "https://identity.test/v1/tokens/introspect",
		RequestTimeout: time.Second,
		CacheTTL:       time.Minute,
		MaxCacheSize:   2,
		HTTPClient:     httpClient,
	})

	// Introspect 3 tokens: A, B, C
	for _, token := range []string{"token-A", "token-B", "token-C"} {
		if _, err := client.IntrospectProto(context.Background(), token); err != nil {
			t.Fatalf("introspect %s: %v", token, err)
		}
	}
	if callCount != 3 {
		t.Fatalf("expected 3 calls, got %d", callCount)
	}

	// Token A should have been evicted (LRU), so cache miss on fallback lookup
	client.cacheLock.Lock()
	size := client.introspection.Len()
	_, hasA := client.introspection.Get("token-A")
	_, hasB := client.introspection.Get("token-B")
	_, hasC := client.introspection.Get("token-C")
	client.cacheLock.Unlock()

	if size != 2 {
		t.Fatalf("expected cache size 2, got %d", size)
	}
	if hasA {
		t.Fatal("expected token-A to be evicted")
	}
	if !hasB {
		t.Fatal("expected token-B to be cached")
	}
	if !hasC {
		t.Fatal("expected token-C to be cached")
	}
}

func TestIntrospectionCachePromotesOnHit(t *testing.T) {
	t.Parallel()

	callCount := 0
	httpClient := &http.Client{
		Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			callCount++
			token := strings.TrimPrefix(req.Header.Get("Authorization"), "Bearer ")
			if callCount <= 2 {
				return &http.Response{
					StatusCode: http.StatusOK,
					Body: io.NopCloser(strings.NewReader(
						fmt.Sprintf(`{"active":true,"organization_id":"org-%s"}`, token),
					)),
					Header: make(http.Header),
				}, nil
			}
			// After the first two, simulate an outage so cache fallback is used for A
			if callCount == 3 {
				return nil, context.DeadlineExceeded
			}
			// Fourth call (token-C) succeeds
			return &http.Response{
				StatusCode: http.StatusOK,
				Body: io.NopCloser(strings.NewReader(
					fmt.Sprintf(`{"active":true,"organization_id":"org-%s"}`, token),
				)),
				Header: make(http.Header),
			}, nil
		}),
	}

	client := New(Config{
		IntrospectURL:  "https://identity.test/v1/tokens/introspect",
		RequestTimeout: time.Second,
		CacheTTL:       time.Minute,
		MaxCacheSize:   2,
		HTTPClient:     httpClient,
	})

	// Introspect A then B
	if _, err := client.IntrospectProto(context.Background(), "token-A"); err != nil {
		t.Fatalf("introspect A: %v", err)
	}
	if _, err := client.IntrospectProto(context.Background(), "token-B"); err != nil {
		t.Fatalf("introspect B: %v", err)
	}

	// Introspect A again (outage -> cache hit -> promotes A)
	result, err := client.IntrospectProto(context.Background(), "token-A")
	if err != nil {
		t.Fatalf("introspect A (cache fallback): %v", err)
	}
	if result.GetOrganizationId() != "org-token-A" {
		t.Fatalf("expected cached org for A, got %q", result.GetOrganizationId())
	}

	// Now introspect C — this should evict B (not A, because A was recently accessed)
	if _, err := client.IntrospectProto(context.Background(), "token-C"); err != nil {
		t.Fatalf("introspect C: %v", err)
	}

	client.cacheLock.Lock()
	_, hasA := client.introspection.Get("token-A")
	_, hasB := client.introspection.Get("token-B")
	_, hasC := client.introspection.Get("token-C")
	client.cacheLock.Unlock()

	if !hasA {
		t.Fatal("expected token-A to be retained (promoted)")
	}
	if hasB {
		t.Fatal("expected token-B to be evicted")
	}
	if !hasC {
		t.Fatal("expected token-C to be cached")
	}
}

func TestIntrospectionCacheRespectsDefaultMaxSize(t *testing.T) {
	t.Parallel()

	client := New(Config{
		IntrospectURL:  "https://identity.test/v1/tokens/introspect",
		RequestTimeout: time.Second,
		CacheTTL:       time.Minute,
	})

	client.cacheLock.Lock()
	maxSize := client.introspection.MaxSize()
	client.cacheLock.Unlock()

	if maxSize != 10000 {
		t.Fatalf("expected default max size 10000, got %d", maxSize)
	}
}

func TestServiceTokenCacheEvictsWhenFull(t *testing.T) {
	t.Parallel()

	callCount := 0
	identityServer := testutil.NewTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount++
		testutil.WriteJSON(t, w, http.StatusCreated, map[string]any{
			"token":      fmt.Sprintf("token-%d", callCount),
			"token_type": "Bearer",
			"claims": map[string]any{
				"expires_at": time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
			},
		})
	}))

	client := New(Config{
		ServiceTokensURL: identityServer.URL + "/v1/service-tokens",
		BootstrapKey:     "identity-bootstrap-key",
		RequestTimeout:   time.Second,
		CacheTTL:         time.Minute,
		MaxCacheSize:     2,
		HTTPClient:       http.DefaultClient,
	})

	// Issue tokens for 3 different org/service/scope combinations
	for _, org := range []string{"org-1", "org-2", "org-3"} {
		if _, err := client.ResolveServiceToken(
			context.Background(), org, "svc", []string{"read"}, time.Minute,
		); err != nil {
			t.Fatalf("resolve %s: %v", org, err)
		}
	}

	client.serviceTokenLock.Lock()
	size := client.serviceTokens.Len()
	client.serviceTokenLock.Unlock()

	if size != 2 {
		t.Fatalf("expected service token cache size 2, got %d", size)
	}
}

func protoIntrospectionResult(active bool, organizationID string) *identityv1.IntrospectResponse {
	return &identityv1.IntrospectResponse{
		Active:         active,
		OrganizationId: organizationID,
	}
}
