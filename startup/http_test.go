package startup

import (
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"testing"
	"time"
)

func TestRunHTTPServerAppliesHTTPDefenses(t *testing.T) {
	t.Parallel()

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen test server: %v", err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	addr := listener.Addr().String()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	mux := http.NewServeMux()
	mux.HandleFunc("/", func(writer http.ResponseWriter, request *http.Request) {
		writer.WriteHeader(http.StatusNoContent)
	})
	server := &http.Server{
		Addr:    addr,
		Handler: mux,
	}

	errCh := make(chan error, 1)
	go func() {
		errCh <- RunHTTPServer(ctx, HTTPServerConfig{
			ServiceName: "test-service",
			Addr:        addr,
			Server:      server,
			Listener:    listener,
			Logger:      slog.New(slog.NewTextHandler(io.Discard, nil)),
		})
	}()
	waitForServer(t, addr, errCh)

	client := &http.Client{Timeout: time.Second}
	response, err := client.Get("http://" + addr + "/")
	if err != nil {
		t.Fatalf("GET server: %v", err)
	}
	_ = response.Body.Close()
	if response.StatusCode != http.StatusNoContent {
		t.Fatalf("GET status = %d, want %d", response.StatusCode, http.StatusNoContent)
	}
	assertResponseHeader(t, response.Header, "X-Content-Type-Options", "nosniff")
	assertResponseHeader(t, response.Header, "X-Frame-Options", "DENY")
	assertResponseHeader(t, response.Header, "Referrer-Policy", "no-referrer")

	cancel()
	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("RunHTTPServer() error = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("RunHTTPServer() did not return after shutdown")
	}
}

func waitForServer(t *testing.T, addr string, errCh <-chan error) {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		select {
		case err := <-errCh:
			t.Fatalf("server exited before accepting connections: %v", err)
		case <-deadline:
			t.Fatal("timed out waiting for server")
		default:
			conn, err := net.DialTimeout("tcp", addr, 50*time.Millisecond)
			if err == nil {
				_ = conn.Close()
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
}

func assertResponseHeader(t *testing.T, header http.Header, key string, want string) {
	t.Helper()
	if got := header.Get(key); got != want {
		t.Fatalf("%s = %q, want %q", key, got, want)
	}
}
