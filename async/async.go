// Package async provides fire-and-forget helpers for background work that
// should not block the caller. It standardises the pattern used across
// identity, chat, and agent-mcp: detach from the request context, clone
// data to prevent races, warn on errors, and recover panics.
package async

import (
	"context"
	"log/slog"
	"runtime/debug"

	"google.golang.org/protobuf/proto"
)

// Task is a unit of background work. The context passed to a Task is detached
// from the originating request so it survives handler return.
type Task func(ctx context.Context) error

// Runner bounds fire-and-forget work so degraded downstreams cannot create
// unbounded goroutines under load.
type Runner struct {
	sem    chan struct{}
	logger *slog.Logger
}

// NewRunner creates a bounded background task runner.
func NewRunner(maxInFlight int, logger *slog.Logger) *Runner {
	if maxInFlight <= 0 {
		maxInFlight = 1
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &Runner{sem: make(chan struct{}, maxInFlight), logger: logger}
}

// TryGo launches task with a context detached from request cancellation when
// capacity is available. It returns false without launching a goroutine when
// the runner is full.
func (r *Runner) TryGo(ctx context.Context, op string, task Task) bool {
	if r == nil || task == nil {
		return false
	}
	select {
	case r.sem <- struct{}{}:
	default:
		r.logger.Warn("background task rejected: capacity exhausted", "op", op)
		return false
	}
	bgCtx := context.WithoutCancel(ctx)
	go func() {
		defer func() {
			if recovered := recover(); recovered != nil {
				r.logger.Error("background task panicked", "op", op, "panic", recovered, "stack", string(debug.Stack()))
			}
			<-r.sem
		}()
		if err := task(bgCtx); err != nil {
			r.logger.Warn("background task failed", "op", op, "error", err)
		}
	}()
	return true
}

// FireAndForget launches task in a background goroutine with a context
// detached from the parent (via context.WithoutCancel). Errors are logged
// at WARN level and panics are recovered and logged at ERROR level with the
// given operation name.
//
// Use this for best-effort side effects that the caller does not need to
// wait for: audit delivery, usage metering, registry heartbeats, event
// publishing.
func FireAndForget(ctx context.Context, logger *slog.Logger, op string, task Task) {
	bgCtx := context.WithoutCancel(ctx)
	go func() {
		defer func() {
			if recovered := recover(); recovered != nil {
				logger.Error("background task panicked", "op", op, "panic", recovered, "stack", string(debug.Stack()))
			}
		}()
		if err := task(bgCtx); err != nil {
			logger.Warn("background task failed", "op", op, "error", err)
		}
	}()
}

// CloneProto returns a deep copy of msg. Use this before launching a
// goroutine to prevent data races when the caller mutates the message
// after return.
//
//	cloned := async.CloneProto(req.Msg)
//	async.FireAndForget(ctx, logger, "meter.record_usage", func(ctx context.Context) error {
//	    _, err := meter.RecordUsage(ctx, connect.NewRequest(cloned))
//	    return err
//	})
func CloneProto[T proto.Message](msg T) T {
	cloned, ok := proto.Clone(msg).(T)
	if !ok {
		var zero T
		return zero
	}
	return cloned
}
