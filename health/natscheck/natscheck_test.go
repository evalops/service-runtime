package natscheck_test

import (
	"context"
	"testing"

	"github.com/evalops/service-runtime/health/natscheck"
	"github.com/nats-io/nats.go"
)

type mockConn struct{ status nats.Status }

func (m mockConn) Status() nats.Status { return m.status }

func TestCheck(t *testing.T) {
	check := natscheck.Check(mockConn{status: nats.CONNECTED})
	if err := check(context.Background()); err != nil {
		t.Fatalf("connected nats check returned error: %v", err)
	}

	check = natscheck.Check(mockConn{status: nats.CLOSED})
	if err := check(context.Background()); err == nil {
		t.Fatal("expected nats check to report closed connection")
	}
}
