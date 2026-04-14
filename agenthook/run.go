package agenthook

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"connectrpc.com/connect"
	approvalsv1 "github.com/evalops/proto/gen/go/approvals/v1"
	"github.com/evalops/proto/gen/go/approvals/v1/approvalsv1connect"
	governancev1 "github.com/evalops/proto/gen/go/governance/v1"
	"github.com/evalops/proto/gen/go/governance/v1/governancev1connect"
)

// GovernanceClient is the subset of governance needed by the hook.
type GovernanceClient interface {
	EvaluateAction(context.Context, *connect.Request[governancev1.EvaluateActionRequest]) (*connect.Response[governancev1.EvaluateActionResponse], error)
}

// ApprovalClient is the subset of approvals needed by the hook.
type ApprovalClient interface {
	RequestApproval(context.Context, *connect.Request[approvalsv1.RequestApprovalRequest]) (*connect.Response[approvalsv1.RequestApprovalResponse], error)
	GetApproval(context.Context, *connect.Request[approvalsv1.GetApprovalRequest]) (*connect.Response[approvalsv1.GetApprovalResponse], error)
}

// Runner executes governance checks for hook payloads.
type Runner struct {
	Config     Config
	Governance GovernanceClient
	Approvals  ApprovalClient
	Sleep      func(context.Context, time.Duration) error
}

// NewRunner builds a runner with Connect clients.
func NewRunner(cfg Config) (*Runner, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	httpClient, err := cfg.HTTPClient()
	if err != nil {
		return nil, fmt.Errorf("build_http_client: %w", err)
	}

	runner := &Runner{
		Config:     cfg,
		Governance: governancev1connect.NewGovernanceServiceClient(httpClient, cfg.GovernanceURL),
		Sleep:      sleepWithContext,
	}
	if strings.TrimSpace(cfg.ApprovalsURL) != "" {
		runner.Approvals = approvalsv1connect.NewApprovalServiceClient(httpClient, cfg.ApprovalsURL)
	}
	return runner, nil
}

// Execute runs the CLI entrypoint and returns the process exit code.
func Execute(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(args) != 1 || args[0] != "governance-check" {
		_, _ = fmt.Fprintln(stderr, "usage: evalops-agent-hook governance-check")
		return 1
	}

	cfg, err := LoadConfigFromEnv()
	if err != nil {
		_, _ = fmt.Fprintln(stderr, err)
		return 1
	}
	runner, err := NewRunner(cfg)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, err)
		return 1
	}

	payload, err := io.ReadAll(stdin)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, fmt.Errorf("read_stdin: %w", err))
		return 1
	}

	decision, err := runner.GovernanceCheck(ctx, payload)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, err)
		return 1
	}
	if decision == nil {
		return 0
	}

	encoder := json.NewEncoder(stdout)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(decision); err != nil {
		_, _ = fmt.Fprintln(stderr, fmt.Errorf("encode_hook_response: %w", err))
		return 1
	}
	return 2
}

// GovernanceCheck evaluates the incoming tool use and optionally returns a deny response.
func (runner *Runner) GovernanceCheck(ctx context.Context, input []byte) (*PermissionDecision, error) {
	if runner == nil || runner.Governance == nil {
		return nil, errors.New("governance client is required")
	}

	event, err := ParseToolUseEvent(input)
	if err != nil {
		return nil, err
	}

	agentID := event.resolvedAgentID(runner.Config.AgentID)
	request := connect.NewRequest(&governancev1.EvaluateActionRequest{
		WorkspaceId:   runner.Config.WorkspaceID,
		AgentId:       agentID,
		ActionType:    event.ToolName,
		ActionPayload: event.actionPayload(input),
	})
	runner.addAuthorization(request.Header())

	requestCtx, cancel := context.WithTimeout(ctx, runner.Config.GovernanceTimeout)
	defer cancel()

	response, err := runner.Governance.EvaluateAction(requestCtx, request)
	if err != nil {
		return denyDecision(fmt.Sprintf("governance check failed: %v", err)), nil
	}
	evaluation := response.Msg.GetEvaluation()
	if evaluation == nil {
		return denyDecision("governance returned no evaluation"), nil
	}

	switch evaluation.GetDecision() {
	case governancev1.ActionDecision_ACTION_DECISION_ALLOW:
		return nil, nil
	case governancev1.ActionDecision_ACTION_DECISION_DENY:
		return denyDecision(joinReasons(evaluation.GetReasons(), "governance denied this action")), nil
	case governancev1.ActionDecision_ACTION_DECISION_REQUIRE_APPROVAL:
		return runner.awaitApproval(ctx, event, input, agentID, evaluation)
	default:
		return denyDecision("governance returned an unknown decision"), nil
	}
}

func (runner *Runner) awaitApproval(
	ctx context.Context,
	event ToolUseEvent,
	rawInput []byte,
	agentID string,
	evaluation *governancev1.ActionEvaluation,
) (*PermissionDecision, error) {
	if runner.Approvals == nil {
		return denyDecision("approval required but EVALOPS_APPROVALS_URL is not configured"), nil
	}

	request := connect.NewRequest(&approvalsv1.RequestApprovalRequest{
		WorkspaceId:   runner.Config.WorkspaceID,
		AgentId:       agentID,
		Surface:       event.surface(runner.Config.Surface),
		ActionType:    event.ToolName,
		ActionPayload: event.actionPayload(rawInput),
		RiskLevel:     mapApprovalRisk(evaluation.GetRiskLevel()),
		ContextJson:   event.approvalContextJSON(rawInput, evaluation.GetReasons(), evaluation.GetMatchedRules()),
	})
	runner.addAuthorization(request.Header())

	requestCtx, cancel := context.WithTimeout(ctx, runner.Config.ApprovalsTimeout)
	defer cancel()

	response, err := runner.Approvals.RequestApproval(requestCtx, request)
	if err != nil {
		return denyDecision(fmt.Sprintf("approval request failed: %v", err)), nil
	}

	approvalID := strings.TrimSpace(response.Msg.GetApprovalRequest().GetId())
	if approvalID == "" {
		return denyDecision("approval request failed: missing approval id"), nil
	}

	deadline := time.Now().Add(runner.Config.ApprovalTimeout)
	for {
		if err := ctx.Err(); err != nil {
			return denyDecision(fmt.Sprintf("approval wait canceled: %v", err)), nil
		}
		if time.Now().After(deadline) {
			return denyDecision("approval timed out"), nil
		}

		state, decision, err := runner.getApproval(ctx, approvalID)
		if err != nil {
			return denyDecision(fmt.Sprintf("approval lookup failed: %v", err)), nil
		}
		switch state {
		case "", "pending":
			if err := runner.sleep(ctx, runner.Config.ApprovalPollInterval); err != nil {
				return denyDecision(fmt.Sprintf("approval wait canceled: %v", err)), nil
			}
			continue
		default:
			return decisionFromApproval(state, decision), nil
		}
	}
}

func (runner *Runner) getApproval(ctx context.Context, approvalID string) (string, *approvalsv1.ApprovalDecision, error) {
	request := connect.NewRequest(&approvalsv1.GetApprovalRequest{
		ApprovalRequestId: approvalID,
		WorkspaceId:       runner.Config.WorkspaceID,
	})
	runner.addAuthorization(request.Header())

	requestCtx, cancel := context.WithTimeout(ctx, runner.Config.ApprovalsTimeout)
	defer cancel()

	response, err := runner.Approvals.GetApproval(requestCtx, request)
	if err != nil {
		return "", nil, err
	}
	state := strings.TrimSpace(response.Msg.GetState())
	return state, latestDecision(response.Msg.GetDecisions()), nil
}

func (runner *Runner) addAuthorization(headers http.Header) {
	if headers == nil {
		return
	}
	if token := strings.TrimSpace(runner.Config.AgentToken); token != "" {
		headers.Set("Authorization", "Bearer "+token)
	}
}

func (runner *Runner) sleep(ctx context.Context, duration time.Duration) error {
	if runner != nil && runner.Sleep != nil {
		return runner.Sleep(ctx, duration)
	}
	return sleepWithContext(ctx, duration)
}

func sleepWithContext(ctx context.Context, duration time.Duration) error {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func latestDecision(decisions []*approvalsv1.ApprovalDecision) *approvalsv1.ApprovalDecision {
	if len(decisions) == 0 {
		return nil
	}
	return decisions[len(decisions)-1]
}

func decisionFromApproval(state string, decision *approvalsv1.ApprovalDecision) *PermissionDecision {
	if decision == nil {
		switch state {
		case "resolved":
			return denyDecision("approval resolved without a final decision")
		case "expired":
			return denyDecision("approval expired")
		default:
			return denyDecision(fmt.Sprintf("approval ended in unexpected state %q", state))
		}
	}

	switch decision.GetDecision() {
	case approvalsv1.DecisionType_DECISION_TYPE_APPROVED:
		return nil
	case approvalsv1.DecisionType_DECISION_TYPE_DENIED:
		return denyDecision(firstNonEmpty(decision.GetReason(), "approval denied"))
	case approvalsv1.DecisionType_DECISION_TYPE_EXPIRED:
		return denyDecision(firstNonEmpty(decision.GetReason(), "approval expired"))
	case approvalsv1.DecisionType_DECISION_TYPE_ESCALATED:
		return denyDecision(firstNonEmpty(decision.GetReason(), "approval escalated"))
	default:
		return denyDecision(firstNonEmpty(decision.GetReason(), "approval did not resolve to allow"))
	}
}

func joinReasons(reasons []string, fallback string) string {
	filtered := make([]string, 0, len(reasons))
	for _, reason := range reasons {
		if trimmed := strings.TrimSpace(reason); trimmed != "" {
			filtered = append(filtered, trimmed)
		}
	}
	if len(filtered) == 0 {
		return fallback
	}
	return strings.Join(filtered, "; ")
}

func mapApprovalRisk(level governancev1.RiskLevel) approvalsv1.RiskLevel {
	switch level {
	case governancev1.RiskLevel_RISK_LEVEL_LOW:
		return approvalsv1.RiskLevel_RISK_LEVEL_LOW
	case governancev1.RiskLevel_RISK_LEVEL_MEDIUM:
		return approvalsv1.RiskLevel_RISK_LEVEL_MEDIUM
	case governancev1.RiskLevel_RISK_LEVEL_HIGH:
		return approvalsv1.RiskLevel_RISK_LEVEL_HIGH
	case governancev1.RiskLevel_RISK_LEVEL_CRITICAL:
		return approvalsv1.RiskLevel_RISK_LEVEL_CRITICAL
	default:
		return approvalsv1.RiskLevel_RISK_LEVEL_UNSPECIFIED
	}
}
