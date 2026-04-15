package startup

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
)

// ShutdownFunc closes a startup-managed dependency.
type ShutdownFunc func(context.Context) error

type shutdownHook struct {
	name string
	fn   ShutdownFunc
}

// Lifecycle tracks shutdown hooks for startup-managed dependencies.
type Lifecycle struct {
	mu    sync.Mutex
	hooks []shutdownHook
}

// NewLifecycle creates an empty startup lifecycle.
func NewLifecycle() *Lifecycle {
	return &Lifecycle{}
}

// OnShutdown registers fn to run when Shutdown is called.
func (l *Lifecycle) OnShutdown(name string, fn ShutdownFunc) {
	if l == nil || fn == nil {
		return
	}

	name = strings.TrimSpace(name)
	if name == "" {
		name = "shutdown hook"
	}

	l.mu.Lock()
	defer l.mu.Unlock()
	l.hooks = append(l.hooks, shutdownHook{name: name, fn: fn})
}

// Shutdown runs registered hooks in reverse order and joins any returned errors.
func (l *Lifecycle) Shutdown(ctx context.Context) error {
	if l == nil {
		return nil
	}

	l.mu.Lock()
	hooks := append([]shutdownHook(nil), l.hooks...)
	l.hooks = nil
	l.mu.Unlock()

	var errs []error
	for index := len(hooks) - 1; index >= 0; index-- {
		hook := hooks[index]
		if err := hook.fn(ctx); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", hook.name, err))
		}
	}

	if len(errs) == 0 {
		return nil
	}
	return errors.Join(errs...)
}
