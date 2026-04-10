package mtls

import (
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestBuildClientTLSConfigReturnsNilWhenUnset(t *testing.T) {
	tlsConfig, err := BuildClientTLSConfig(ClientConfig{})
	if err != nil {
		t.Fatalf("build client tls config: %v", err)
	}
	if tlsConfig != nil {
		t.Fatalf("expected nil tls config, got %#v", tlsConfig)
	}
}

func TestBuildClientTLSConfigRequiresMatchingKeypair(t *testing.T) {
	_, err := BuildClientTLSConfig(ClientConfig{CertFile: "client.pem"})
	if err == nil || err.Error() != "tls_key_file_required" {
		t.Fatalf("expected tls_key_file_required, got %v", err)
	}

	_, err = BuildClientTLSConfig(ClientConfig{KeyFile: "client.key"})
	if err == nil || err.Error() != "tls_cert_file_required" {
		t.Fatalf("expected tls_cert_file_required, got %v", err)
	}
}

func TestBuildServerTLSConfigRequiresCertAndKeyTogether(t *testing.T) {
	_, err := BuildServerTLSConfig(ServerConfig{CertFile: "server.pem"})
	if err == nil || err.Error() != "tls_cert_and_key_must_both_be_set" {
		t.Fatalf("expected tls_cert_and_key_must_both_be_set, got %v", err)
	}
}

func TestBuildHTTPClientReturnsDefaultClientWhenUnset(t *testing.T) {
	client, err := BuildHTTPClient(ClientConfig{})
	if err != nil {
		t.Fatalf("build http client: %v", err)
	}
	if client != http.DefaultClient {
		t.Fatal("expected default client")
	}
}

func TestVerifiedClientCertificateIdentitiesDedupes(t *testing.T) {
	request := httptest.NewRequest(http.MethodGet, "/", nil)
	request.TLS = &tls.ConnectionState{
		VerifiedChains: [][]*x509.Certificate{{
			{
				Subject:        pkix.Name{CommonName: "svc"},
				DNSNames:       []string{"svc.internal", "svc.internal"},
				EmailAddresses: []string{"svc@example.com"},
			},
		}},
	}

	identities := VerifiedClientCertificateIdentities(request)
	if len(identities) != 3 {
		t.Fatalf("expected 3 unique identities, got %v", identities)
	}
	if strings.Join(identities, ",") != "svc,svc.internal,svc@example.com" {
		t.Fatalf("unexpected identities: %v", identities)
	}
}

func TestRequireVerifiedClientCertificateRejectsMissingCertificate(t *testing.T) {
	handler := RequireVerifiedClientCertificate(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.WriteHeader(http.StatusNoContent)
	}))

	request := httptest.NewRequest(http.MethodGet, "/", nil)
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", recorder.Code)
	}
	if !strings.Contains(recorder.Body.String(), "client_certificate_required") {
		t.Fatalf("unexpected response: %q", recorder.Body.String())
	}
}

func TestRequireVerifiedClientCertificateForIdentitiesHonorsAllowedList(t *testing.T) {
	handler := RequireVerifiedClientCertificateForIdentities(
		[]string{"svc.internal"},
		http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
			writer.WriteHeader(http.StatusNoContent)
		}),
	)

	request := httptest.NewRequest(http.MethodGet, "/", nil)
	request.TLS = &tls.ConnectionState{
		VerifiedChains: [][]*x509.Certificate{{
			{
				Subject:  pkix.Name{CommonName: "svc"},
				DNSNames: []string{"svc.internal"},
			},
		}},
	}
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusNoContent {
		t.Fatalf("expected 204, got %d", recorder.Code)
	}
}
