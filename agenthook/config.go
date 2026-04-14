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

func LoadConfigFromEnv() (Config, error) {
	cfg := Config{
		GovernanceURL:        normalizeServiceURL(trimEnv("EVALOPS_GOVERNANCE_URL"), governanceServiceSuffixes),
		ApprovalsURL:         normalizeServiceURL(trimEnv("EVALOPS_APPROVALS_URL"), approvalsServiceSuffixes),
		AgentToken:           trimEnv("EVALOPS_AGENT_TOKEN"),
		WorkspaceID:          trimEnv("EVALOPS_WORKSPACE_ID"),
		AgentID:              trimEnv("EVALOPS_AGENT_ID"),
		Surface:              firstNonEmpty(trimEnv("EVALOPS_HOOK_SURFACE"), trimEnv("EVALOPS_SURFACE"), defaultSurface),
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

func (cfg Config) HTTPClient() (*http.Client, error) {
	return mtls.BuildHTTPClient(cfg.TLS)
}

var governanceServiceSuffixes = []string{
	"/governance.v1.GovernanceService/EvaluateAction",
	"/governance.v1.GovernanceService",
}

var approvalsServiceSuffixes = []string{
	"/approvals.v1.ApprovalService/RequestApproval",
	"/approvals.v1.ApprovalService/GetApproval",
	"/approvals.v1.ApprovalService",
}

func normalizeServiceURL(value string, suffixes []string) string {
	normalized := strings.TrimSpace(value)
	for _, suffix := range suffixes {
		normalized = strings.TrimSuffix(normalized, suffix)
	}
	return strings.TrimRight(normalized, "/")
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

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if trimmed := strings.TrimSpace(value); trimmed != "" {
			return trimmed
		}
	}
	return ""
}
