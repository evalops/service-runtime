package testutil

import (
	"testing"
	"time"

	nserver "github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
)

const testNATSReadyTimeout = 10 * time.Second

// NewTestNATSServer starts an embedded NATS server with JetStream enabled,
// registers cleanup with the test, and returns a connected client plus server URL.
func NewTestNATSServer(t *testing.T) (*nats.Conn, string) {
	t.Helper()

	options := &nserver.Options{
		Host:      "127.0.0.1",
		Port:      -1,
		JetStream: true,
		StoreDir:  t.TempDir(),
		NoLog:     true,
		NoSigs:    true,
	}

	server, err := nserver.NewServer(options)
	if err != nil {
		t.Fatalf("create test nats server: %v", err)
	}

	go server.Start()
	if !server.ReadyForConnections(testNATSReadyTimeout) {
		server.Shutdown()
		server.WaitForShutdown()
		t.Fatal("test nats server was not ready in time")
	}

	url := server.ClientURL()
	conn, err := nats.Connect(url, nats.Name("service-runtime testutil"))
	if err != nil {
		server.Shutdown()
		server.WaitForShutdown()
		t.Fatalf("connect test nats client: %v", err)
	}

	t.Cleanup(func() {
		if err := conn.Drain(); err != nil && err != nats.ErrConnectionClosed {
			t.Errorf("drain test nats client: %v", err)
		}
		conn.Close()
		server.Shutdown()
		server.WaitForShutdown()
	})

	return conn, url
}

// WaitForNATSMessage waits for the next message on sub and fails the test on timeout or error.
func WaitForNATSMessage(t *testing.T, sub *nats.Subscription, timeout time.Duration) *nats.Msg {
	t.Helper()

	if sub == nil {
		t.Fatal("expected non-nil NATS subscription")
	}

	message, err := sub.NextMsg(timeout)
	if err != nil {
		t.Fatalf("wait for nats message: %v", err)
	}
	return message
}
