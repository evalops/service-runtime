package natsbus

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/evalops/service-runtime/resilience"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	"google.golang.org/protobuf/types/known/wrapperspb"
)

func TestReliablePublisherDeadLettersAfterRetryExhausted(t *testing.T) {
	t.Parallel()

	js := &reliableFakeJetStream{
		publishErrs: []error{
			errors.New("nats unavailable"),
			errors.New("nats unavailable"),
			errors.New("nats unavailable"),
		},
	}
	reliable := newTestReliablePublisher(t, js, ReliableOptions{
		Retry: resilience.RetryConfig{
			MaxAttempts:  3,
			InitialDelay: time.Nanosecond,
			MaxDelay:     time.Nanosecond,
			Multiplier:   1,
		},
		Breaker: resilience.BreakerConfig{
			FailureThreshold: 5,
			ResetTimeout:     time.Hour,
		},
	})

	change := testReliableChange(1)
	err := reliable.Publish(context.Background(), change)
	if err == nil || !strings.Contains(err.Error(), "dead-lettered") {
		t.Fatalf("expected dead-letter error, got %v", err)
	}
	if got := js.PublishCount(); got != 3 {
		t.Fatalf("expected 3 publish attempts, got %d", got)
	}
	expectedMessage := js.Messages()[0]

	records, err := reliable.deadLetters.List(10)
	if err != nil {
		t.Fatalf("list dead letters: %v", err)
	}
	if len(records) != 1 {
		t.Fatalf("expected 1 dead letter, got %d", len(records))
	}

	gotMessage := records[0].message()
	if gotMessage.Subject != expectedMessage.Subject {
		t.Fatalf("expected subject %q, got %q", expectedMessage.Subject, gotMessage.Subject)
	}
	if !reflect.DeepEqual(gotMessage.Header, expectedMessage.Header) {
		t.Fatalf("expected header %#v, got %#v", expectedMessage.Header, gotMessage.Header)
	}
	if string(gotMessage.Data) != string(expectedMessage.Data) {
		t.Fatalf("expected payload %q, got %q", string(expectedMessage.Data), string(gotMessage.Data))
	}

	if got := readCounterValue(t, reliable.metrics.publishFailuresTotal); got != 1 {
		t.Fatalf("expected publish failure metric 1, got %v", got)
	}
	if got := readGaugeValue(t, reliable.metrics.deadLetterSize); got != 1 {
		t.Fatalf("expected dead-letter gauge 1, got %v", got)
	}
}

func TestReliablePublisherReplayPendingPublishesAndClearsDeadLetters(t *testing.T) {
	t.Parallel()

	js := &reliableFakeJetStream{
		publishErrs: []error{
			errors.New("nats unavailable"),
			errors.New("nats unavailable"),
			errors.New("nats unavailable"),
		},
	}
	reliable := newTestReliablePublisher(t, js, ReliableOptions{
		Retry: resilience.RetryConfig{
			MaxAttempts:  3,
			InitialDelay: time.Nanosecond,
			MaxDelay:     time.Nanosecond,
			Multiplier:   1,
		},
		Breaker: resilience.BreakerConfig{
			FailureThreshold: 5,
			ResetTimeout:     time.Hour,
		},
	})

	if err := reliable.Publish(context.Background(), testReliableChange(1)); err == nil {
		t.Fatal("expected publish to dead-letter after retries")
	}
	if got := reliable.deadLetters.Count(); got != 1 {
		t.Fatalf("expected 1 dead letter before replay, got %d", got)
	}

	if err := reliable.ReplayPending(context.Background()); err != nil {
		t.Fatalf("replay pending: %v", err)
	}
	if got := js.PublishCount(); got != 4 {
		t.Fatalf("expected 4 total publish attempts including replay, got %d", got)
	}
	if got := reliable.deadLetters.Count(); got != 0 {
		t.Fatalf("expected dead letters to drain after replay, got %d", got)
	}
	if got := readCounterValue(t, reliable.metrics.publishFailuresTotal); got != 1 {
		t.Fatalf("expected publish failure metric to remain 1 after replay, got %v", got)
	}
	if got := readGaugeValue(t, reliable.metrics.deadLetterSize); got != 0 {
		t.Fatalf("expected dead-letter gauge 0 after replay, got %v", got)
	}
}

func TestReliablePublisherBreakerOpensAndSkipsNATSAfterThreshold(t *testing.T) {
	t.Parallel()

	js := &reliableFakeJetStream{
		publishErrs: []error{errors.New("nats unavailable")},
	}
	reliable := newTestReliablePublisher(t, js, ReliableOptions{
		Retry: resilience.RetryConfig{
			MaxAttempts:  1,
			InitialDelay: time.Nanosecond,
			MaxDelay:     time.Nanosecond,
			Multiplier:   1,
		},
		Breaker: resilience.BreakerConfig{
			FailureThreshold: 1,
			ResetTimeout:     time.Hour,
		},
	})

	if err := reliable.Publish(context.Background(), testReliableChange(1)); err == nil {
		t.Fatal("expected initial publish failure")
	}

	err := reliable.Publish(context.Background(), testReliableChange(2))
	if err == nil || !strings.Contains(err.Error(), resilience.ErrCircuitOpen.Error()) {
		t.Fatalf("expected circuit_open error on second publish, got %v", err)
	}
	if got := js.PublishCount(); got != 1 {
		t.Fatalf("expected breaker to skip second NATS publish, got %d attempts", got)
	}
	if got := reliable.deadLetters.Count(); got != 2 {
		t.Fatalf("expected both failed publishes to be dead-lettered, got %d", got)
	}
	if got := readCounterValue(t, reliable.metrics.publishFailuresTotal); got != 2 {
		t.Fatalf("expected publish failure metric 2, got %v", got)
	}
	if got := readGaugeValue(t, reliable.metrics.deadLetterSize); got != 2 {
		t.Fatalf("expected dead-letter gauge 2, got %v", got)
	}
}

func TestConnectReliableRequiresDeadLetterDir(t *testing.T) {
	t.Parallel()

	_, err := ConnectReliable(context.Background(), "nats://example", "stream", "audit.events", ReliableOptions{})
	if !errors.Is(err, errDeadLetterDirRequired) {
		t.Fatalf("expected errDeadLetterDirRequired, got %v", err)
	}
}

func newTestReliablePublisher(t *testing.T, js jetStreamClient, opts ReliableOptions) *ReliablePublisher {
	t.Helper()

	opts.Registerer = prometheus.NewRegistry()
	opts.DeadLetterDir = t.TempDir()
	if opts.ReplayInterval == 0 {
		opts.ReplayInterval = time.Hour
	}

	reliable, err := NewReliablePublisher(&Publisher{
		js:            js,
		subjectPrefix: "audit.events",
		source:        eventSource("audit.events"),
	}, opts)
	if err != nil {
		t.Fatalf("new reliable publisher: %v", err)
	}
	t.Cleanup(reliable.Close)
	return reliable
}

func testReliableChange(sequence int64) Change {
	return Change{
		Sequence:      sequence,
		TenantID:      "org-123",
		AggregateType: "approval",
		AggregateID:   "approval-1",
		Operation:     "decide",
		RecordedAt:    time.Date(2026, 4, 15, 12, 0, 0, 0, time.UTC),
		Payload:       MustPayload(wrapperspb.String("payload")),
	}
}

func readCounterValue(t *testing.T, counter prometheus.Counter) float64 {
	t.Helper()

	metric := &dto.Metric{}
	if err := counter.Write(metric); err != nil {
		t.Fatalf("read counter metric: %v", err)
	}
	return metric.GetCounter().GetValue()
}

func readGaugeValue(t *testing.T, gauge prometheus.Gauge) float64 {
	t.Helper()

	metric := &dto.Metric{}
	if err := gauge.Write(metric); err != nil {
		t.Fatalf("read gauge metric: %v", err)
	}
	return metric.GetGauge().GetValue()
}

type reliableFakeJetStream struct {
	mu           sync.Mutex
	streamConfig jetstream.StreamConfig
	publishErrs  []error
	publishCount int
	messages     []*nats.Msg
}

func (fake *reliableFakeJetStream) CreateOrUpdateStream(_ context.Context, cfg jetstream.StreamConfig) (jetstream.Stream, error) {
	fake.mu.Lock()
	defer fake.mu.Unlock()

	fake.streamConfig = cfg
	return nil, nil
}

func (fake *reliableFakeJetStream) PublishMsg(_ context.Context, message *nats.Msg, _ ...jetstream.PublishOpt) (*jetstream.PubAck, error) {
	fake.mu.Lock()
	defer fake.mu.Unlock()

	fake.publishCount++
	fake.messages = append(fake.messages, cloneMessage(message))
	if len(fake.publishErrs) == 0 {
		return &jetstream.PubAck{}, nil
	}

	err := fake.publishErrs[0]
	fake.publishErrs = fake.publishErrs[1:]
	if err != nil {
		return nil, err
	}
	return &jetstream.PubAck{}, nil
}

func (fake *reliableFakeJetStream) PublishCount() int {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	return fake.publishCount
}

func (fake *reliableFakeJetStream) Messages() []*nats.Msg {
	fake.mu.Lock()
	defer fake.mu.Unlock()

	messages := make([]*nats.Msg, 0, len(fake.messages))
	for _, message := range fake.messages {
		messages = append(messages, cloneMessage(message))
	}
	return messages
}
