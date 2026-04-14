package async_test

import (
	"context"
	"errors"
	"log/slog"
	"sync/atomic"
	"testing"
	"time"

	approvalsv1 "github.com/evalops/proto/gen/go/approvals/v1"
	"github.com/evalops/service-runtime/async"
)

func TestFireAndForgetRunsInBackground(t *testing.T) {
	var called atomic.Bool
	logger := slog.Default()

	async.FireAndForget(context.Background(), logger, "test.op", func(_ context.Context) error {
		called.Store(true)
		return nil
	})

	waitFor(t, &called, "background task to run")
}

func TestFireAndForgetSurvivesCancelledParent(t *testing.T) {
	var called atomic.Bool
	var wasCancelled atomic.Bool
	logger := slog.Default()

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel before launch

	async.FireAndForget(ctx, logger, "test.cancelled", func(bgCtx context.Context) error {
		if bgCtx.Err() != nil {
			wasCancelled.Store(true)
		}
		called.Store(true)
		return nil
	})

	waitFor(t, &called, "background task to run despite cancelled parent")

	if wasCancelled.Load() {
		t.Fatal("background context was cancelled — WithoutCancel should have detached it")
	}
}

func TestFireAndForgetLogsErrorsWithoutPanicking(t *testing.T) {
	var called atomic.Bool
	logger := slog.Default()

	async.FireAndForget(context.Background(), logger, "test.fail", func(_ context.Context) error {
		called.Store(true)
		return errors.New("simulated failure")
	})

	waitFor(t, &called, "failing background task to complete")
}

func TestFireAndForgetRecoversPanics(t *testing.T) {
	logs := make(chan slog.Record, 1)
	logger := slog.New(recordingHandler{records: logs})

	async.FireAndForget(context.Background(), logger, "test.panic", func(_ context.Context) error {
		panic("simulated panic")
	})

	select {
	case record := <-logs:
		if record.Level != slog.LevelError {
			t.Fatalf("log level = %s, want %s", record.Level, slog.LevelError)
		}
		if record.Message != "background task panicked" {
			t.Fatalf("log message = %q, want %q", record.Message, "background task panicked")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for panic recovery log")
	}
}

func TestCloneProtoPreventsMutationRace(t *testing.T) {
	original := &approvalsv1.ApprovalRequest{
		Id:          "req-1",
		WorkspaceId: "ws-1",
		AgentId:     "agent-1",
		Surface:     "chat",
		ActionType:  "crm_update",
	}

	cloned := async.CloneProto(original)

	// Mutate the original after cloning.
	original.Id = "req-2"
	original.AgentId = "agent-2"
	original.WorkspaceId = "ws-2"

	// Clone must be unaffected.
	if cloned.GetId() != "req-1" {
		t.Fatalf("clone was mutated: id = %s", cloned.GetId())
	}
	if cloned.GetAgentId() != "agent-1" {
		t.Fatalf("clone was mutated: agent_id = %s", cloned.GetAgentId())
	}
	if cloned.GetWorkspaceId() != "ws-1" {
		t.Fatalf("clone was mutated: workspace_id = %s", cloned.GetWorkspaceId())
	}
}

func TestCloneProtoReturnsCorrectType(t *testing.T) {
	original := &approvalsv1.ApprovalHabit{
		Pattern:               "chat:crm_update",
		AutoApproveConfidence: 0.97,
		ObservationCount:      50,
	}

	// CloneProto should return *approvalsv1.ApprovalHabit, not proto.Message.
	cloned := async.CloneProto(original)

	// This line proves the return type is correct at compile time.
	if cloned.GetPattern() != "chat:crm_update" {
		t.Fatalf("pattern = %s, want chat:crm_update", cloned.GetPattern())
	}
	if cloned.GetAutoApproveConfidence() != 0.97 {
		t.Fatalf("confidence = %f, want 0.97", cloned.GetAutoApproveConfidence())
	}
}

// waitFor polls an atomic.Bool with a 2-second deadline.
func waitFor(t *testing.T, flag *atomic.Bool, what string) {
	t.Helper()
	deadline := time.After(2 * time.Second)
	for !flag.Load() {
		select {
		case <-deadline:
			t.Fatalf("timed out waiting for %s", what)
		default:
			time.Sleep(5 * time.Millisecond)
		}
	}
}

type recordingHandler struct {
	records chan slog.Record
}

func (handler recordingHandler) Enabled(context.Context, slog.Level) bool {
	return true
}

func (handler recordingHandler) Handle(_ context.Context, record slog.Record) error {
	handler.records <- record.Clone()
	return nil
}

func (handler recordingHandler) WithAttrs([]slog.Attr) slog.Handler {
	return handler
}

func (handler recordingHandler) WithGroup(string) slog.Handler {
	return handler
}
