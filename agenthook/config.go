package agenthook

import (
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/evalops/service-runtime/mtls"
)

const (
	defaultSurface           = "codex"
	defaultApprovalTimeout   = 5 * time.Minute
	defaultPollInterval      = 3 * time.Second
	defaultGovernanceTimeout = 5 * time.Second
	defaultApprovalsTimeout  = 5 * time.Second
	defaultClientServerName  = ""
)

// Config controls the governance hook binary.
type Config struct {
	GovernanceURL        string
	ApprovalsURL         string
	AgentToken           string
	WorkspaceID          string
	AgentID              string
	Surface              string
	ApprovalTimeout      time.Duration
	ApprovalPollInterval time.Duration
	GovernanceTimeout    time.Duration
	ApprovalsTimeout     time.Duration
	TLS                  mtls.ClientConfig
}

// LoadConfigFromEnv reads hook configuration from environment variables.
func LoadConfigFromEnv() (Config, error) {
	cfg := Config{
		GovernanceURL:        trimEnv("EVALOPS_GOVERNANCE_URL"),
		ApprovalsURL:         trimEnv("EVALOPS_APPROVALS_URL"),
		AgentToken:           trimEnv("EVALOPS_AGENT_TOKEN"),
		WorkspaceID:          trimEnv("EVALOPS_WORKSPACE_ID"),
		AgentID:              trimEnv("EVALOPS_AGENT_ID"),
		Surface:              envOrDefault("EVALOPS_SURFACE", defaultSurface),
		ApprovalTimeout:      envDuration("EVALOPS_APPROVAL_TIMEOUT", defaultApprovalTimeout),
		ApprovalPollInterval: envDuration("EVALOPS_APPROVAL_POLL_INTERVAL", defaultPollInterval),
		GovernanceTimeout:    envDuration("EVALOPS_GOVERNANCE_TIMEOUT", defaultGovernanceTimeout),
		ApprovalsTimeout:     envDuration("EVALOPS_APPROVALS_TIMEOUT", defaultApprovalsTimeout),
		TLS: mtls.ClientConfig{
			CAFile:     trimEnv("EVALOPS_CA_FILE"),
			CertFile:   trimEnv("EVALOPS_CERT_FILE"),
			KeyFile:    trimEnv("EVALOPS_KEY_FILE"),
			ServerName: envOrDefault("EVALOPS_SERVER_NAME", defaultClientServerName),
		},
	}
	return cfg, cfg.Validate()
}

// Validate reports missing or invalid configuration.
func (cfg Config) Validate() error {
	switch {
	case strings.TrimSpace(cfg.GovernanceURL) == "":
		return fmt.Errorf("EVALOPS_GOVERNANCE_URL is required")
	case strings.TrimSpace(cfg.WorkspaceID) == "":
		return fmt.Errorf("EVALOPS_WORKSPACE_ID is required")
	case cfg.ApprovalTimeout <= 0:
		return fmt.Errorf("EVALOPS_APPROVAL_TIMEOUT must be greater than zero")
	case cfg.ApprovalPollInterval <= 0:
		return fmt.Errorf("EVALOPS_APPROVAL_POLL_INTERVAL must be greater than zero")
	case cfg.GovernanceTimeout <= 0:
		return fmt.Errorf("EVALOPS_GOVERNANCE_TIMEOUT must be greater than zero")
	case cfg.ApprovalsTimeout <= 0:
		return fmt.Errorf("EVALOPS_APPROVALS_TIMEOUT must be greater than zero")
	}
	return nil
}

// HTTPClient builds the shared outbound HTTP client for governance and approvals.
func (cfg Config) HTTPClient() (*http.Client, error) {
	return mtls.BuildHTTPClient(cfg.TLS)
}

func envOrDefault(key, fallback string) string {
	if value := trimEnv(key); value != "" {
		return value
	}
	return fallback
}

func envDuration(key string, fallback time.Duration) time.Duration {
	value := trimEnv(key)
	if value == "" {
		return fallback
	}
	if duration, err := time.ParseDuration(value); err == nil {
		return duration
	}
	if seconds, err := strconv.Atoi(value); err == nil {
		return time.Duration(seconds) * time.Second
	}
	return fallback
}

func trimEnv(key string) string {
	return strings.TrimSpace(os.Getenv(key))
}
