package identityclient

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	identityv1 "github.com/evalops/identity/gen/proto/go/identity/v1"
	"github.com/evalops/service-runtime/mtls"
	"google.golang.org/protobuf/encoding/protojson"
)

var (
	ErrIdentityNotConfigured = errors.New("identity_not_configured")
	ErrIdentityUnavailable   = errors.New("identity_unavailable")
	ErrInvalidToken          = errors.New("invalid_token")
	ErrInactiveToken         = errors.New("inactive_token")
)

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
		return nil, fmt.Errorf("%w: %v", ErrIdentityUnavailable, err)
	}
	defer response.Body.Close()

	if response.StatusCode != http.StatusOK {
		if response.StatusCode == http.StatusBadRequest ||
			response.StatusCode == http.StatusUnauthorized ||
			response.StatusCode == http.StatusForbidden {
			return nil, ErrInvalidToken
		}
		return nil, fmt.Errorf("%w: status_%d", ErrIdentityUnavailable, response.StatusCode)
	}

	body, err := io.ReadAll(response.Body)
	if err != nil {
		return nil, fmt.Errorf("%w: read_response: %v", ErrIdentityUnavailable, err)
	}
	var result identityv1.IntrospectionResult
	if err := (protojson.UnmarshalOptions{DiscardUnknown: true}).Unmarshal(body, &result); err != nil {
		return nil, fmt.Errorf("%w: decode_response: %v", ErrIdentityUnavailable, err)
	}
	if result.Active == nil || !result.GetActive() {
		return nil, ErrInactiveToken
	}
	return &result, nil
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
