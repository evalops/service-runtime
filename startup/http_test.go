package startup

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"io"
	"log/slog"
	"math/big"
	"net"
	"net/http"
	"os"
	"strings"
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

func TestRunHTTPServerUsesConfiguredTLSFilesAndClientCA(t *testing.T) {
	t.Parallel()

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen test server: %v", err)
	}
	t.Cleanup(func() { _ = listener.Close() })

	caPath := writeTestCACertificate(t)
	certPath := t.TempDir() + "/missing-cert.pem"
	keyPath := t.TempDir() + "/missing-key.pem"

	server := &http.Server{
		Handler: http.NewServeMux(),
	}

	err = RunHTTPServer(context.Background(), HTTPServerConfig{
		ServiceName:     "test-service",
		Server:          server,
		Listener:        listener,
		Logger:          slog.New(slog.NewTextHandler(io.Discard, nil)),
		TLSCertFile:     certPath,
		TLSKeyFile:      keyPath,
		TLSClientCAFile: caPath,
	})
	if err == nil {
		t.Fatal("RunHTTPServer() error = nil, want missing certificate error")
	}
	if !strings.Contains(err.Error(), certPath) {
		t.Fatalf("RunHTTPServer() error = %v, want cert path %q in error", err, certPath)
	}
	if server.TLSConfig == nil {
		t.Fatal("server.TLSConfig = nil, want client CA configuration")
	}
	if server.TLSConfig.ClientAuth != tls.RequireAndVerifyClientCert {
		t.Fatalf("server.TLSConfig.ClientAuth = %v, want %v", server.TLSConfig.ClientAuth, tls.RequireAndVerifyClientCert)
	}
	if server.TLSConfig.ClientCAs == nil {
		t.Fatal("server.TLSConfig.ClientCAs = nil, want configured cert pool")
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

func writeTestCACertificate(t *testing.T) string {
	t.Helper()

	_, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate CA private key: %v", err)
	}

	template := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		NotBefore:             time.Now().Add(-time.Minute),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
	}

	der, err := x509.CreateCertificate(rand.Reader, template, template, privateKey.Public(), privateKey)
	if err != nil {
		t.Fatalf("create CA certificate: %v", err)
	}

	caPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	if caPEM == nil {
		t.Fatal("encode CA certificate PEM: nil")
	}

	path := t.TempDir() + "/client-ca.pem"
	if err := os.WriteFile(path, caPEM, 0o600); err != nil {
		t.Fatalf("write CA certificate: %v", err)
	}
	return path
}
