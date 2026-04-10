package identityclient

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/evalops/service-runtime/mtls"
)

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
}

func TestIntrospectSuccess(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if got := request.Header.Get("Authorization"); got != "Bearer write-token" {
			t.Fatalf("unexpected authorization header: %q", got)
		}
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(`{"active":true,"organization_id":"org_123","scopes":["audit:write"]}`))
	}))
	defer server.Close()

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
}

func TestIntrospectReturnsInvalidTokenForUnauthorized(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.WriteHeader(http.StatusUnauthorized)
	}))
	defer server.Close()

	client := NewClient(server.URL, time.Second, server.Client())
	_, err := client.Introspect(context.Background(), "bad-token")
	if !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("expected ErrInvalidToken, got %v", err)
	}
}

func TestIntrospectReturnsInactiveToken(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(`{"active":false}`))
	}))
	defer server.Close()

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
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(`{not-json}`))
	}))
	defer server.Close()

	client := NewClient(server.URL, time.Second, server.Client())
	_, err := client.Introspect(context.Background(), "write-token")
	if err == nil || !errors.Is(err, ErrIdentityUnavailable) {
		t.Fatalf("expected ErrIdentityUnavailable, got %v", err)
	}
	if !strings.Contains(err.Error(), "decode_response") {
		t.Fatalf("expected decode_response in error, got %v", err)
	}
}
