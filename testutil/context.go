package testutil

import (
	"context"
	"testing"
)

// Context returns a cancellable background context and registers cleanup with the test.
func Context(t *testing.T) context.Context {
	t.Helper()

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	return ctx
}
