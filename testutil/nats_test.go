package testutil

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/nats-io/nats.go/jetstream"
)

func TestNewTestNATSServerSupportsJetStreamRoundTrip(t *testing.T) {
	t.Parallel()

	conn, url := NewTestNATSServer(t)
	if !strings.HasPrefix(url, "nats://") {
		t.Fatalf("expected NATS URL, got %q", url)
	}

	js, err := jetstream.New(conn)
	if err != nil {
		t.Fatalf("create jetstream client: %v", err)
	}

	subject := fmt.Sprintf("test.events.%d", time.Now().UnixNano())
	streamName := fmt.Sprintf("TEST_%d", time.Now().UnixNano())
	_, err = js.CreateOrUpdateStream(context.Background(), jetstream.StreamConfig{
		Name:     streamName,
		Subjects: []string{subject},
	})
	if err != nil {
		t.Fatalf("create stream: %v", err)
	}

	sub, err := conn.SubscribeSync(subject)
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	if err := conn.Flush(); err != nil {
		t.Fatalf("flush subscription: %v", err)
	}

	if _, err := js.Publish(context.Background(), subject, []byte("hello")); err != nil {
		t.Fatalf("publish jetstream message: %v", err)
	}

	message := WaitForNATSMessage(t, sub, 5*time.Second)
	if got := string(message.Data); got != "hello" {
		t.Fatalf("expected hello payload, got %q", got)
	}
}
