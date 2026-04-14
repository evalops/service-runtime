package agenthook

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	approvalsv1 "github.com/evalops/proto/gen/go/approvals/v1"
	governancev1 "github.com/evalops/proto/gen/go/governance/v1"
)

type errReader struct{}

func (errReader) Read([]byte) (int, error) {
	return 0, errors.New("stdin exploded")
}

func TestExecuteFailsClosedOnStdinReadError(t *testing.T) {
	t.Setenv("EVALOPS_GOVERNANCE_URL", "https://governance.example")
	t.Setenv("EVALOPS_WORKSPACE_ID", "ws_123")

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	exitCode := Execute(context.Background(), []string{"governance-check"}, errReader{}, &stdout, &stderr)
	if exitCode != 2 {
		t.Fatalf("Execute() exit code = %d, want 2", exitCode)
	}
	if stderr.Len() != 0 {
		t.Fatalf("stderr = %q, want empty", stderr.String())
	}

	var decision PermissionDecision
	if err := json.Unmarshal(stdout.Bytes(), &decision); err != nil {
		t.Fatalf("decode deny decision: %v", err)
	}
	if decision.PermissionDecision != "deny" {
		t.Fatalf("permissionDecision = %q, want deny", decision.PermissionDecision)
	}
	if !strings.Contains(decision.PermissionDecisionReason, "read_stdin: stdin exploded") {
		t.Fatalf("permissionDecisionReason = %q", decision.PermissionDecisionReason)
	}
}

func TestGovernanceCheckAllowsAction(t *testing.T) {
	governance := &stubGovernanceClient{
		response: &governancev1.EvaluateActionResponse{
			Evaluation: &governancev1.ActionEvaluation{
				Decision: governancev1.ActionDecision_ACTION_DECISION_ALLOW,
			},
		},
	}
	runner := &Runner{
		Config: Config{
			GovernanceURL:        "https://governance.example",
			WorkspaceID:          "ws_123",
			AgentID:              "agent_env",
			ApprovalTimeout:      defaultApprovalTimeout,
			ApprovalPollInterval: defaultPollInterval,
			GovernanceTimeout:    defaultGovernanceTimeout,
			ApprovalsTimeout:     defaultApprovalsTimeout,
		},
		Governance: governance,
	}

	decision, err := runner.GovernanceCheck(context.Background(), []byte(`{"tool_name":"Bash","tool_input":{"command":"pwd"}}`))
	if err != nil {
		t.Fatalf("GovernanceCheck() error = %v", err)
	}
	if decision != nil {
		t.Fatalf("expected allow, got deny %#v", decision)
	}
	if governance.lastRequest.GetWorkspaceId() != "ws_123" {
		t.Fatalf("workspace_id = %q, want ws_123", governance.lastRequest.GetWorkspaceId())
	}
	if governance.lastRequest.GetAgentId() != "agent_env" {
		t.Fatalf("agent_id = %q, want agent_env", governance.lastRequest.GetAgentId())
	}
	if governance.lastRequest.GetActionType() != "Bash" {
		t.Fatalf("action_type = %q, want Bash", governance.lastRequest.GetActionType())
	}
}

func TestGovernanceCheckPrefersEventAgentIDOverFallback(t *testing.T) {
	governance := &stubGovernanceClient{
		response: &governancev1.EvaluateActionResponse{
			Evaluation: &governancev1.ActionEvaluation{
				Decision: governancev1.ActionDecision_ACTION_DECISION_ALLOW,
			},
		},
	}
	runner := &Runner{
		Config: Config{
			GovernanceURL:        "https://governance.example",
			WorkspaceID:          "ws_123",
			AgentID:              "agent_env",
			ApprovalTimeout:      defaultApprovalTimeout,
			ApprovalPollInterval: defaultPollInterval,
			GovernanceTimeout:    defaultGovernanceTimeout,
			ApprovalsTimeout:     defaultApprovalsTimeout,
		},
		Governance: governance,
	}

	decision, err := runner.GovernanceCheck(context.Background(), []byte(`{"tool_name":"Bash","tool_input":{"command":"pwd"},"agent_id":"agent_event","session_id":"sess_1"}`))
	if err != nil {
		t.Fatalf("GovernanceCheck() error = %v", err)
	}
	if decision != nil {
		t.Fatalf("expected allow, got deny %#v", decision)
	}
	if governance.lastRequest.GetAgentId() != "agent_event" {
		t.Fatalf("agent_id = %q, want agent_event", governance.lastRequest.GetAgentId())
	}
}

func TestGovernanceCheckDeniesOnGovernanceDecision(t *testing.T) {
	runner := &Runner{
		Config: Config{
			GovernanceURL:        "https://governance.example",
			WorkspaceID:          "ws_123",
			AgentID:              "agent_env",
			ApprovalTimeout:      defaultApprovalTimeout,
			ApprovalPollInterval: defaultPollInterval,
			GovernanceTimeout:    defaultGovernanceTimeout,
			ApprovalsTimeout:     defaultApprovalsTimeout,
		},
		Governance: &stubGovernanceClient{
			response: &governancev1.EvaluateActionResponse{
				Evaluation: &governancev1.ActionEvaluation{
					Decision: governancev1.ActionDecision_ACTION_DECISION_DENY,
					Reasons:  []string{"destructive command", "policy violation"},
				},
			},
		},
	}

	decision, err := runner.GovernanceCheck(context.Background(), []byte(`{"tool_name":"Bash","tool_input":{"command":"rm -rf /"}}`))
	if err != nil {
		t.Fatalf("GovernanceCheck() error = %v", err)
	}
	if decision == nil {
		t.Fatal("expected deny decision")
	}
	if decision.PermissionDecision != "deny" {
		t.Fatalf("permissionDecision = %q, want deny", decision.PermissionDecision)
	}
	if decision.PermissionDecisionReason != "destructive command; policy violation" {
		t.Fatalf("permissionDecisionReason = %q", decision.PermissionDecisionReason)
	}
}

func TestGovernanceCheckFailsClosedOnGovernanceError(t *testing.T) {
	runner := &Runner{
		Config: Config{
			GovernanceURL:        "https://governance.example",
			WorkspaceID:          "ws_123",
			AgentID:              "agent_env",
			ApprovalTimeout:      defaultApprovalTimeout,
			ApprovalPollInterval: defaultPollInterval,
			GovernanceTimeout:    defaultGovernanceTimeout,
			ApprovalsTimeout:     defaultApprovalsTimeout,
		},
		Governance: &stubGovernanceClient{err: errors.New("dial tcp timeout")},
	}

	decision, err := runner.GovernanceCheck(context.Background(), []byte(`{"tool_name":"Edit","tool_input":{"path":"a.txt"}}`))
	if err != nil {
		t.Fatalf("GovernanceCheck() error = %v", err)
	}
	if decision == nil || decision.PermissionDecision != "deny" {
		t.Fatalf("expected fail-closed deny, got %#v", decision)
	}
}

func TestGovernanceCheckFailsClosedOnUnspecifiedDecision(t *testing.T) {
	runner := &Runner{
		Config: Config{
			GovernanceURL:        "https://governance.example",
			WorkspaceID:          "ws_123",
			AgentID:              "agent_env",
			ApprovalTimeout:      defaultApprovalTimeout,
			ApprovalPollInterval: defaultPollInterval,
			GovernanceTimeout:    defaultGovernanceTimeout,
			ApprovalsTimeout:     defaultApprovalsTimeout,
		},
		Governance: &stubGovernanceClient{
			response: &governancev1.EvaluateActionResponse{
				Evaluation: &governancev1.ActionEvaluation{
					Decision: governancev1.ActionDecision_ACTION_DECISION_UNSPECIFIED,
				},
			},
		},
	}

	decision, err := runner.GovernanceCheck(context.Background(), []byte(`{"tool_name":"Edit","tool_input":{"path":"a.txt"}}`))
	if err != nil {
		t.Fatalf("GovernanceCheck() error = %v", err)
	}
	if decision == nil || decision.PermissionDecision != "deny" {
		t.Fatalf("expected fail-closed deny, got %#v", decision)
	}
	if decision.PermissionDecisionReason != "governance returned an unknown decision" {
		t.Fatalf("permissionDecisionReason = %q", decision.PermissionDecisionReason)
	}
}

func TestGovernanceCheckFailsClosedOnInvalidPayload(t *testing.T) {
	runner := &Runner{
		Config: Config{
			GovernanceURL:        "https://governance.example",
			WorkspaceID:          "ws_123",
			ApprovalTimeout:      defaultApprovalTimeout,
			ApprovalPollInterval: defaultPollInterval,
			GovernanceTimeout:    defaultGovernanceTimeout,
			ApprovalsTimeout:     defaultApprovalsTimeout,
		},
		Governance: &stubGovernanceClient{},
	}

	decision, err := runner.GovernanceCheck(context.Background(), []byte(`{"tool_name":`))
	if err != nil {
		t.Fatalf("GovernanceCheck() error = %v", err)
	}
	if decision == nil || decision.PermissionDecision != "deny" {
		t.Fatalf("expected deny decision, got %#v", decision)
	}
	if !strings.Contains(decision.PermissionDecisionReason, "decode_tool_use_payload") {
		t.Fatalf("permissionDecisionReason = %q", decision.PermissionDecisionReason)
	}
}

func TestGovernanceCheckRequiresAgentIdentity(t *testing.T) {
	governance := &stubGovernanceClient{
		response: &governancev1.EvaluateActionResponse{
			Evaluation: &governancev1.ActionEvaluation{
				Decision: governancev1.ActionDecision_ACTION_DECISION_ALLOW,
			},
		},
	}
	runner := &Runner{
		Config: Config{
			GovernanceURL:        "https://governance.example",
			WorkspaceID:          "ws_123",
			ApprovalTimeout:      defaultApprovalTimeout,
			ApprovalPollInterval: defaultPollInterval,
			GovernanceTimeout:    defaultGovernanceTimeout,
			ApprovalsTimeout:     defaultApprovalsTimeout,
		},
		Governance: governance,
	}

	decision, err := runner.GovernanceCheck(context.Background(), []byte(`{"tool_name":"Bash","tool_input":{"command":"pwd"}}`))
	if err != nil {
		t.Fatalf("GovernanceCheck() error = %v", err)
	}
	if decision == nil || decision.PermissionDecision != "deny" {
		t.Fatalf("expected deny decision, got %#v", decision)
	}
	if governance.lastRequest != nil {
		t.Fatal("governance should not be called without an agent identity")
	}
}

func TestGovernanceCheckApprovesAfterHumanApproval(t *testing.T) {
	governance := &stubGovernanceClient{
		response: &governancev1.EvaluateActionResponse{
			Evaluation: &governancev1.ActionEvaluation{
				Decision:     governancev1.ActionDecision_ACTION_DECISION_REQUIRE_APPROVAL,
				RiskLevel:    governancev1.RiskLevel_RISK_LEVEL_HIGH,
				Reasons:      []string{"touches protected branch"},
				MatchedRules: []string{"branch-protection"},
			},
		},
	}
	approvals := &stubApprovalClient{
		requestResponse: &approvalsv1.RequestApprovalResponse{
			ApprovalRequest: &approvalsv1.ApprovalRequest{Id: "apr_123"},
		},
		getResponses: []*approvalsv1.GetApprovalResponse{
			{State: "pending"},
			{
				State: "resolved",
				Decisions: []*approvalsv1.ApprovalDecision{
					{ApprovalRequestId: "apr_123", Decision: approvalsv1.DecisionType_DECISION_TYPE_APPROVED},
				},
			},
		},
	}
	runner := &Runner{
		Config: Config{
			GovernanceURL:        "https://governance.example",
			ApprovalsURL:         "https://approvals.example",
			WorkspaceID:          "ws_123",
			Surface:              "codex",
			ApprovalTimeout:      defaultApprovalTimeout,
			ApprovalPollInterval: 10 * time.Millisecond,
			GovernanceTimeout:    defaultGovernanceTimeout,
			ApprovalsTimeout:     defaultApprovalsTimeout,
		},
		Governance: governance,
		Approvals:  approvals,
		Sleep:      func(context.Context, time.Duration) error { return nil },
	}

	decision, err := runner.GovernanceCheck(context.Background(), []byte(`{"tool_name":"Bash","tool_input":{"command":"git push --force"},"session_id":"sess_1"}`))
	if err != nil {
		t.Fatalf("GovernanceCheck() error = %v", err)
	}
	if decision != nil {
		t.Fatalf("expected approval to allow execution, got %#v", decision)
	}
	if approvals.requestRequest.GetAgentId() != "sess_1" {
		t.Fatalf("request approval agent_id = %q, want sess_1", approvals.requestRequest.GetAgentId())
	}
	if approvals.getCalls != 2 {
		t.Fatalf("GetApproval call count = %d, want 2", approvals.getCalls)
	}
}

func TestGovernanceCheckDeniesRejectedApproval(t *testing.T) {
	runner := &Runner{
		Config: Config{
			GovernanceURL:        "https://governance.example",
			ApprovalsURL:         "https://approvals.example",
			WorkspaceID:          "ws_123",
			AgentID:              "agent_env",
			Surface:              "claude-code",
			ApprovalTimeout:      defaultApprovalTimeout,
			ApprovalPollInterval: 10 * time.Millisecond,
			GovernanceTimeout:    defaultGovernanceTimeout,
			ApprovalsTimeout:     defaultApprovalsTimeout,
		},
		Governance: &stubGovernanceClient{
			response: &governancev1.EvaluateActionResponse{
				Evaluation: &governancev1.ActionEvaluation{
					Decision:  governancev1.ActionDecision_ACTION_DECISION_REQUIRE_APPROVAL,
					RiskLevel: governancev1.RiskLevel_RISK_LEVEL_CRITICAL,
				},
			},
		},
		Approvals: &stubApprovalClient{
			requestResponse: &approvalsv1.RequestApprovalResponse{
				ApprovalRequest: &approvalsv1.ApprovalRequest{Id: "apr_456"},
			},
			getResponses: []*approvalsv1.GetApprovalResponse{
				{
					State: "resolved",
					Decisions: []*approvalsv1.ApprovalDecision{
						{ApprovalRequestId: "apr_456", Decision: approvalsv1.DecisionType_DECISION_TYPE_DENIED, Reason: "manager denied force push"},
					},
				},
			},
		},
		Sleep: func(context.Context, time.Duration) error { return nil },
	}

	decision, err := runner.GovernanceCheck(context.Background(), []byte(`{"tool_name":"Bash","tool_input":{"command":"git push --force"}}`))
	if err != nil {
		t.Fatalf("GovernanceCheck() error = %v", err)
	}
	if decision == nil {
		t.Fatal("expected deny decision")
	}
	if decision.PermissionDecisionReason != "manager denied force push" {
		t.Fatalf("permissionDecisionReason = %q, want manager denied force push", decision.PermissionDecisionReason)
	}
}

type stubGovernanceClient struct {
	response    *governancev1.EvaluateActionResponse
	err         error
	lastRequest *governancev1.EvaluateActionRequest
}

func (stub *stubGovernanceClient) EvaluateAction(_ context.Context, request *connect.Request[governancev1.EvaluateActionRequest]) (*connect.Response[governancev1.EvaluateActionResponse], error) {
	if request != nil {
		stub.lastRequest = request.Msg
	}
	if stub.err != nil {
		return nil, stub.err
	}
	return connect.NewResponse(stub.response), nil
}

type stubApprovalClient struct {
	requestResponse *approvalsv1.RequestApprovalResponse
	requestErr      error
	requestRequest  *approvalsv1.RequestApprovalRequest
	getResponses    []*approvalsv1.GetApprovalResponse
	getErr          error
	getRequest      *approvalsv1.GetApprovalRequest
	getCalls        int
}

func (stub *stubApprovalClient) RequestApproval(_ context.Context, request *connect.Request[approvalsv1.RequestApprovalRequest]) (*connect.Response[approvalsv1.RequestApprovalResponse], error) {
	if request != nil {
		stub.requestRequest = request.Msg
	}
	if stub.requestErr != nil {
		return nil, stub.requestErr
	}
	return connect.NewResponse(stub.requestResponse), nil
}

func (stub *stubApprovalClient) GetApproval(_ context.Context, request *connect.Request[approvalsv1.GetApprovalRequest]) (*connect.Response[approvalsv1.GetApprovalResponse], error) {
	stub.getCalls++
	if request != nil {
		stub.getRequest = request.Msg
	}
	if stub.getErr != nil {
		return nil, stub.getErr
	}
	index := stub.getCalls - 1
	if index >= len(stub.getResponses) {
		index = len(stub.getResponses) - 1
	}
	return connect.NewResponse(stub.getResponses[index]), nil
}
