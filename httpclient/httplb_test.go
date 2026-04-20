package httpclient

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestNewLoadBalancedUsesDefaults(t *testing.T) {
	t.Parallel()

	client := NewLoadBalanced(LoadBalancedOptions{})
	t.Cleanup(func() {
		if err := client.Close(); err != nil {
			t.Fatalf("Close() error = %v", err)
		}
	})

	if client.Client == nil {
		t.Fatal("expected embedded http client")
	}
	if client.Transport == nil {
		t.Fatal("expected httplb transport")
	}
	if client.Timeout != 0 {
		t.Fatalf("Timeout = %s, want httplb request timeout only", client.Timeout)
	}
}

func TestNewLoadBalancedHonorsCustomOptions(t *testing.T) {
	t.Parallel()

	client := NewLoadBalanced(LoadBalancedOptions{
		RequestTimeout:      2 * time.Second,
		IdleConnTimeout:     time.Second,
		MaxIdleConns:        8,
		MaxIdleConnsPerHost: 4,
		MaxConnsPerHost:     6,
	})
	t.Cleanup(func() {
		if err := client.Close(); err != nil {
			t.Fatalf("Close() error = %v", err)
		}
	})

	if client.Timeout != 0 {
		t.Fatalf("Timeout = %s, want httplb request timeout only", client.Timeout)
	}
}

func TestLoadBalancedClientSendsRequests(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Path; got != "/healthz" {
			t.Fatalf("path = %q, want /healthz", got)
		}
		_, _ = fmt.Fprint(w, "ok")
	}))
	defer server.Close()

	client := NewLoadBalanced(LoadBalancedOptions{RequestTimeout: time.Second})
	t.Cleanup(func() {
		if err := client.Close(); err != nil {
			t.Fatalf("Close() error = %v", err)
		}
	})

	response, err := client.Get(server.URL + "/healthz")
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	defer func() {
		_ = response.Body.Close()
	}()

	if response.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want %d", response.StatusCode, http.StatusOK)
	}
}
