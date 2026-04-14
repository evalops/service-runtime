package agenthook

import "testing"

func TestLoadConfigFromEnvNormalizesServiceURLsAndSurfaceAlias(t *testing.T) {
	t.Setenv("EVALOPS_GOVERNANCE_URL", "https://governance.example/governance.v1.GovernanceService/EvaluateAction")
	t.Setenv("EVALOPS_APPROVALS_URL", "https://approvals.example/approvals.v1.ApprovalService/GetApproval")
	t.Setenv("EVALOPS_WORKSPACE_ID", "ws_123")
	t.Setenv("EVALOPS_HOOK_SURFACE", "claude-code")

	cfg, err := LoadConfigFromEnv()
	if err != nil {
		t.Fatalf("LoadConfigFromEnv() error = %v", err)
	}

	if cfg.GovernanceURL != "https://governance.example" {
		t.Fatalf("GovernanceURL = %q", cfg.GovernanceURL)
	}
	if cfg.ApprovalsURL != "https://approvals.example" {
		t.Fatalf("ApprovalsURL = %q", cfg.ApprovalsURL)
	}
	if cfg.Surface != "claude-code" {
		t.Fatalf("Surface = %q", cfg.Surface)
	}
}

func TestLoadConfigFromEnvUsesLegacySurfaceFallback(t *testing.T) {
	t.Setenv("EVALOPS_GOVERNANCE_URL", "https://governance.example")
	t.Setenv("EVALOPS_WORKSPACE_ID", "ws_123")
	t.Setenv("EVALOPS_SURFACE", "codex-legacy")

	cfg, err := LoadConfigFromEnv()
	if err != nil {
		t.Fatalf("LoadConfigFromEnv() error = %v", err)
	}
	if cfg.Surface != "codex-legacy" {
		t.Fatalf("Surface = %q", cfg.Surface)
	}
}
