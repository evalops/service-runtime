package natscheck

import (
	"context"
	"errors"
	"fmt"

	"github.com/evalops/service-runtime/health"
	"github.com/nats-io/nats.go"
)

type statusConn interface {
	Status() nats.Status
}

// Check returns a health.CheckFunc that verifies a NATS connection is active.
func Check(conn statusConn) health.CheckFunc {
	return func(context.Context) error {
		if conn == nil {
			return errors.New("nats_not_configured")
		}
		if status := conn.Status(); status != nats.CONNECTED {
			return fmt.Errorf("nats_not_ready: %s", status.String())
		}
		return nil
	}
}
