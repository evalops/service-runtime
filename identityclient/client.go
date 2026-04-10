package identityclient

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/evalops/service-runtime/mtls"
)

var (
	ErrIdentityNotConfigured = errors.New("identity_not_configured")
	ErrIdentityUnavailable   = errors.New("identity_unavailable")
	ErrInvalidToken          = errors.New("invalid_token")
	ErrInactiveToken         = errors.New("inactive_token")
)

type IntrospectionResult struct {
	Active         bool     `json:"active"`
	OrganizationID string   `json:"organization_id,omitempty"`
	Scopes         []string `json:"scopes,omitempty"`
	Service        string   `json:"service,omitempty"`
	Subject        string   `json:"subject,omitempty"`
	TokenType      string   `json:"token_type,omitempty"`
}

type Client struct {
	httpClient     *http.Client
	introspectURL  string
	requestTimeout time.Duration
}

func NewClient(introspectURL string, requestTimeout time.Duration, httpClient *http.Client) *Client {
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	return &Client{
		httpClient:     httpClient,
		introspectURL:  introspectURL,
		requestTimeout: requestTimeout,
	}
}

func NewMTLSClient(introspectURL string, requestTimeout time.Duration, tlsConfig mtls.ClientConfig) (*Client, error) {
	httpClient, err := mtls.BuildHTTPClient(tlsConfig)
	if err != nil {
		return nil, err
	}
	return NewClient(introspectURL, requestTimeout, httpClient), nil
}

func (c *Client) Configured() bool {
	return c != nil && c.introspectURL != ""
}

func (c *Client) Introspect(ctx context.Context, bearerToken string) (IntrospectionResult, error) {
	if !c.Configured() {
		return IntrospectionResult{}, ErrIdentityNotConfigured
	}
	requestCtx, cancel := context.WithTimeout(ctx, c.requestTimeout)
	defer cancel()

	request, err := http.NewRequestWithContext(
		requestCtx,
		http.MethodPost,
		c.introspectURL,
		bytes.NewReader([]byte(`{}`)),
	)
	if err != nil {
		return IntrospectionResult{}, fmt.Errorf("build_request: %w", err)
	}
	request.Header.Set("Authorization", "Bearer "+bearerToken)
	request.Header.Set("Content-Type", "application/json")

	response, err := c.httpClient.Do(request)
	if err != nil {
		return IntrospectionResult{}, fmt.Errorf("%w: %v", ErrIdentityUnavailable, err)
	}
	defer response.Body.Close()

	if response.StatusCode != http.StatusOK {
		if response.StatusCode == http.StatusBadRequest ||
			response.StatusCode == http.StatusUnauthorized ||
			response.StatusCode == http.StatusForbidden {
			return IntrospectionResult{}, ErrInvalidToken
		}
		return IntrospectionResult{}, fmt.Errorf("%w: status_%d", ErrIdentityUnavailable, response.StatusCode)
	}

	var result IntrospectionResult
	if err := json.NewDecoder(response.Body).Decode(&result); err != nil {
		return IntrospectionResult{}, fmt.Errorf("%w: decode_response: %v", ErrIdentityUnavailable, err)
	}
	if !result.Active {
		return IntrospectionResult{}, ErrInactiveToken
	}
	return result, nil
}
