package mtls

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net/http"
	"os"
)

// ClientConfig holds TLS settings for an outbound mTLS client.
type ClientConfig struct {
	CAFile     string
	CertFile   string
	KeyFile    string
	ServerName string
}

// ServerConfig holds TLS settings for an inbound mTLS server.
type ServerConfig struct {
	CertFile     string
	KeyFile      string
	ClientCAFile string
}

// BuildServerTLSConfig returns a *tls.Config for an mTLS server, or nil when all fields are empty.
func BuildServerTLSConfig(cfg ServerConfig) (*tls.Config, error) {
	if cfg.CertFile == "" && cfg.KeyFile == "" && cfg.ClientCAFile == "" {
		return nil, nil
	}
	if cfg.CertFile == "" || cfg.KeyFile == "" {
		return nil, errors.New("tls_cert_and_key_must_both_be_set")
	}

	certificate, err := tls.LoadX509KeyPair(cfg.CertFile, cfg.KeyFile)
	if err != nil {
		return nil, fmt.Errorf("load_server_keypair: %w", err)
	}

	tlsConfig := &tls.Config{
		MinVersion:   tls.VersionTLS13,
		Certificates: []tls.Certificate{certificate},
	}

	if cfg.ClientCAFile != "" {
		clientCA, err := os.ReadFile(cfg.ClientCAFile)
		if err != nil {
			return nil, fmt.Errorf("read_client_ca: %w", err)
		}
		clientPool := x509.NewCertPool()
		if !clientPool.AppendCertsFromPEM(clientCA) {
			return nil, errors.New("parse_client_ca")
		}
		tlsConfig.ClientAuth = tls.VerifyClientCertIfGiven
		tlsConfig.ClientCAs = clientPool
	}

	return tlsConfig, nil
}

// BuildHTTPClient returns an *http.Client configured with the given mTLS settings,
// or http.DefaultClient when the config is empty.
func BuildHTTPClient(cfg ClientConfig) (*http.Client, error) {
	tlsConfig, err := BuildClientTLSConfig(cfg)
	if err != nil {
		return nil, err
	}
	if tlsConfig == nil {
		return http.DefaultClient, nil
	}

	transport, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		return nil, errors.New("default transport is not *http.Transport")
	}
	clone := transport.Clone()
	clone.TLSClientConfig = tlsConfig
	return &http.Client{Transport: clone}, nil
}

// BuildClientTLSConfig returns a *tls.Config for an outbound mTLS client, or nil when all fields are empty.
func BuildClientTLSConfig(cfg ClientConfig) (*tls.Config, error) {
	if cfg.CAFile == "" && cfg.CertFile == "" && cfg.KeyFile == "" && cfg.ServerName == "" {
		return nil, nil
	}
	if cfg.CertFile == "" && cfg.KeyFile != "" {
		return nil, errors.New("tls_cert_file_required")
	}
	if cfg.CertFile != "" && cfg.KeyFile == "" {
		return nil, errors.New("tls_key_file_required")
	}

	tlsConfig := &tls.Config{
		MinVersion: tls.VersionTLS13,
		ServerName: cfg.ServerName,
	}

	if cfg.CAFile != "" {
		serverCA, err := os.ReadFile(cfg.CAFile)
		if err != nil {
			return nil, fmt.Errorf("read_server_ca: %w", err)
		}
		rootPool := x509.NewCertPool()
		if !rootPool.AppendCertsFromPEM(serverCA) {
			return nil, errors.New("parse_server_ca")
		}
		tlsConfig.RootCAs = rootPool
	}

	if cfg.CertFile != "" {
		certificate, err := tls.LoadX509KeyPair(cfg.CertFile, cfg.KeyFile)
		if err != nil {
			return nil, fmt.Errorf("load_client_keypair: %w", err)
		}
		tlsConfig.Certificates = []tls.Certificate{certificate}
	}

	return tlsConfig, nil
}

// HasVerifiedClientCertificate reports whether the request contains a verified TLS client certificate.
func HasVerifiedClientCertificate(request *http.Request) bool {
	return request != nil &&
		request.TLS != nil &&
		len(request.TLS.VerifiedChains) > 0
}

// VerifiedClientCertificateIdentities returns deduplicated identities from the verified client certificate.
func VerifiedClientCertificateIdentities(request *http.Request) []string {
	if !HasVerifiedClientCertificate(request) {
		return nil
	}

	seen := make(map[string]struct{})
	identities := make([]string, 0)
	appendIdentity := func(value string) {
		if value == "" {
			return
		}
		if _, ok := seen[value]; ok {
			return
		}
		seen[value] = struct{}{}
		identities = append(identities, value)
	}

	for _, chain := range request.TLS.VerifiedChains {
		if len(chain) == 0 {
			continue
		}
		certificate := chain[0]
		appendIdentity(certificate.Subject.CommonName)
		for _, dnsName := range certificate.DNSNames {
			appendIdentity(dnsName)
		}
		for _, uri := range certificate.URIs {
			if uri != nil {
				appendIdentity(uri.String())
			}
		}
		for _, emailAddress := range certificate.EmailAddresses {
			appendIdentity(emailAddress)
		}
	}

	return identities
}

// HasAllowedVerifiedClientCertificate reports whether the request has a verified client certificate
// whose identity is in allowedIdentities (or allowedIdentities is empty).
func HasAllowedVerifiedClientCertificate(request *http.Request, allowedIdentities []string) bool {
	if !HasVerifiedClientCertificate(request) {
		return false
	}
	if len(allowedIdentities) == 0 {
		return true
	}

	for _, identity := range VerifiedClientCertificateIdentities(request) {
		for _, allowedIdentity := range allowedIdentities {
			if identity == allowedIdentity {
				return true
			}
		}
	}

	return false
}

// RequireVerifiedClientCertificate is an HTTP middleware that rejects requests without a verified client certificate.
func RequireVerifiedClientCertificate(next http.Handler) http.Handler {
	return RequireVerifiedClientCertificateForIdentities(nil, next)
}

// RequireVerifiedClientCertificateForIdentities is an HTTP middleware that rejects requests whose
// client certificate identity is not in allowedIdentities (pass nil to allow any verified certificate).
func RequireVerifiedClientCertificateForIdentities(allowedIdentities []string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if !HasVerifiedClientCertificate(request) {
			writer.Header().Set("Content-Type", "application/json")
			writer.WriteHeader(http.StatusUnauthorized)
			_, _ = writer.Write([]byte("{\n  \"error\": \"client_certificate_required\"\n}\n"))
			return
		}
		if !HasAllowedVerifiedClientCertificate(request, allowedIdentities) {
			writer.Header().Set("Content-Type", "application/json")
			writer.WriteHeader(http.StatusUnauthorized)
			_, _ = writer.Write([]byte("{\n  \"error\": \"client_certificate_identity_not_allowed\"\n}\n"))
			return
		}
		next.ServeHTTP(writer, request)
	})
}
