package natsbus

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"google.golang.org/protobuf/types/known/wrapperspb"
)

func TestConnectWithOptionsCreatesConfiguredStream(t *testing.T) {
	t.Parallel()

	originalConnect := connectNATS
	originalNewJetStream := newJetStream
	t.Cleanup(func() {
		connectNATS = originalConnect
		newJetStream = originalNewJetStream
	})

	fakeJS := &fakeJetStream{}
	connectNATS = func(string, ...nats.Option) (*nats.Conn, error) {
		return &nats.Conn{}, nil
	}
	newJetStream = func(*nats.Conn) (jetStreamClient, error) {
		return fakeJS, nil
	}

	_, err := ConnectWithOptions(context.Background(), "nats://example", "pipeline_changes", "pipeline.changes", Options{
		Retention: jetstream.WorkQueuePolicy,
		MaxAge:    48 * time.Hour,
		Storage:   jetstream.MemoryStorage,
	})
	if err != nil {
		t.Fatalf("connect with options: %v", err)
	}

	if fakeJS.streamConfig.Name != "pipeline_changes" {
		t.Fatalf("expected stream name, got %#v", fakeJS.streamConfig.Name)
	}
	if fakeJS.streamConfig.Subjects[0] != "pipeline.changes.>" {
		t.Fatalf("unexpected subjects %#v", fakeJS.streamConfig.Subjects)
	}
	if fakeJS.streamConfig.Retention != jetstream.WorkQueuePolicy {
		t.Fatalf("unexpected retention %#v", fakeJS.streamConfig.Retention)
	}
	if fakeJS.streamConfig.MaxAge != 48*time.Hour {
		t.Fatalf("unexpected max age %#v", fakeJS.streamConfig.MaxAge)
	}
	if fakeJS.streamConfig.Storage != jetstream.MemoryStorage {
		t.Fatalf("unexpected storage %#v", fakeJS.streamConfig.Storage)
	}

}

func TestConnectWithOptionsRejectsMissingConfig(t *testing.T) {
	t.Parallel()

	if _, err := ConnectWithOptions(context.Background(), "", "stream", "prefix", Options{}); !errors.Is(err, errNATSURLRequired) {
		t.Fatalf("expected errNATSURLRequired, got %v", err)
	}
	if _, err := ConnectWithOptions(context.Background(), "nats://example", "", "prefix", Options{}); !errors.Is(err, errStreamNameRequired) {
		t.Fatalf("expected errStreamNameRequired, got %v", err)
	}
	if _, err := ConnectWithOptions(context.Background(), "nats://example", "stream", "", Options{}); !errors.Is(err, errSubjectPrefixRequired) {
		t.Fatalf("expected errSubjectPrefixRequired, got %v", err)
	}
}

func TestPublishChangeWrapsCloudEvent(t *testing.T) {
	t.Parallel()

	var logs bytes.Buffer
	publisher := &Publisher{
		js:            &fakeJetStream{},
		logger:        slog.New(slog.NewTextHandler(&logs, nil)),
		subjectPrefix: "pipeline.changes",
		source:        "pipeline",
	}

	change := Change{
		Sequence:      42,
		TenantID:      "org-123",
		AggregateType: "deal",
		Operation:     "create",
		RecordedAt:    time.Date(2026, 4, 11, 18, 0, 0, 0, time.UTC),
		Payload:       MustPayload(wrapperspb.String("d-1")),
	}

	fake := publisher.js.(*fakeJetStream)
	publisher.PublishChange(context.Background(), change)

	if fake.subject != "pipeline.changes.deal.create" {
		t.Fatalf("unexpected subject %q", fake.subject)
	}

	var event CloudEvent
	if err := json.Unmarshal(fake.payload, &event); err != nil {
		t.Fatalf("decode event payload: %v", err)
	}
	if event.Type != "pipeline.changes.deal.create" {
		t.Fatalf("unexpected event type %q", event.Type)
	}
	if event.Source != "pipeline" {
		t.Fatalf("unexpected source %q", event.Source)
	}
	if event.TenantID != "org-123" {
		t.Fatalf("unexpected tenant %q", event.TenantID)
	}
	var data map[string]any
	if err := json.Unmarshal(event.Data, &data); err != nil {
		t.Fatalf("decode event data: %v", err)
	}
	if data["@type"] != "type.googleapis.com/google.protobuf.StringValue" {
		t.Fatalf("unexpected event data type %#v", data["@type"])
	}
	if data["value"] != "d-1" {
		t.Fatalf("unexpected event data value %#v", data["value"])
	}
}

func TestPublishChangeLogsPublishErrors(t *testing.T) {
	t.Parallel()

	var logs bytes.Buffer
	publisher := &Publisher{
		js:            &fakeJetStream{publishErr: errors.New("nats unavailable")},
		logger:        slog.New(slog.NewTextHandler(&logs, nil)),
		subjectPrefix: "pipeline.changes",
		source:        "pipeline",
	}

	publisher.PublishChange(context.Background(), Change{
		Sequence:      7,
		TenantID:      "org-123",
		AggregateType: "deal",
		Operation:     "update",
		Payload:       MustPayload(wrapperspb.String("d-1")),
	})

	if !bytes.Contains(logs.Bytes(), []byte("failed to publish change event")) {
		t.Fatalf("expected publish error log, got %q", logs.String())
	}
}

func TestNoopPublisher(t *testing.T) {
	t.Parallel()

	var publisher ChangePublisher = NoopPublisher{}
	publisher.PublishChange(context.Background(), Change{})
}

func TestNewPayloadAndUnmarshalPayload(t *testing.T) {
	t.Parallel()

	payload, err := NewPayload(wrapperspb.String("d-1"))
	if err != nil {
		t.Fatalf("new payload: %v", err)
	}

	target := &wrapperspb.StringValue{}
	if err := UnmarshalPayload(payload, target); err != nil {
		t.Fatalf("unmarshal payload: %v", err)
	}
	if target.Value != "d-1" {
		t.Fatalf("unexpected target value %q", target.Value)
	}
}

func TestNewPayloadRejectsNilMessage(t *testing.T) {
	t.Parallel()

	if _, err := NewPayload(nil); !errors.Is(err, errPayloadMessageNil) {
		t.Fatalf("expected errPayloadMessageNil, got %v", err)
	}
}

func TestUnmarshalPayloadRejectsNilTarget(t *testing.T) {
	t.Parallel()

	if err := UnmarshalPayload(MustPayload(wrapperspb.String("d-1")), nil); !errors.Is(err, errPayloadTargetNil) {
		t.Fatalf("expected errPayloadTargetNil, got %v", err)
	}
}

type fakeJetStream struct {
	streamConfig jetstream.StreamConfig
	subject      string
	payload      []byte
	publishErr   error
}

func (fake *fakeJetStream) CreateOrUpdateStream(_ context.Context, cfg jetstream.StreamConfig) (jetstream.Stream, error) {
	fake.streamConfig = cfg
	return nil, nil
}

func (fake *fakeJetStream) Publish(_ context.Context, subject string, data []byte, _ ...jetstream.PublishOpt) (*jetstream.PubAck, error) {
	fake.subject = subject
	fake.payload = append([]byte(nil), data...)
	if fake.publishErr != nil {
		return nil, fake.publishErr
	}
	return &jetstream.PubAck{}, nil
}
