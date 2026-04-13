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
	"google.golang.org/protobuf/proto"
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

	fake, ok := publisher.js.(*fakeJetStream)
	if !ok { t.Fatal("unexpected type") }
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
	if event.DataContentType != "application/json" {
		t.Fatalf("unexpected data content type %q", event.DataContentType)
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

func TestPublishChangeWrapsProtoEnvelopeWhenConfigured(t *testing.T) {
	t.Parallel()

	var logs bytes.Buffer
	publisher := &Publisher{
		js:            &fakeJetStream{},
		logger:        slog.New(slog.NewTextHandler(&logs, nil)),
		subjectPrefix: "pipeline.changes",
		source:        "pipeline",
		wireFormat:    WireFormatProto,
	}

	change := Change{
		Sequence:      42,
		TenantID:      "org-123",
		AggregateType: "deal",
		Operation:     "create",
		RecordedAt:    time.Date(2026, 4, 11, 18, 0, 0, 0, time.UTC),
		Payload:       MustPayload(wrapperspb.String("d-1")),
	}

	fake, ok := publisher.js.(*fakeJetStream)
	if !ok { t.Fatal("unexpected type") }
	publisher.PublishChange(context.Background(), change)

	event, err := UnmarshalEnvelope(fake.payload)
	if err != nil {
		t.Fatalf("unmarshal envelope: %v", err)
	}
	if event.WireFormat != WireFormatProto {
		t.Fatalf("unexpected wire format %q", event.WireFormat)
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
	if event.DataContentType != "application/protobuf" {
		t.Fatalf("unexpected data content type %q", event.DataContentType)
	}
	target := &wrapperspb.StringValue{}
	if err := UnmarshalPayload(event.Payload, target); err != nil {
		t.Fatalf("unmarshal payload: %v", err)
	}
	if target.Value != "d-1" {
		t.Fatalf("unexpected payload value %q", target.Value)
	}
}

func TestPublishChangeWrapsProtoHeadersWhenConfigured(t *testing.T) {
	t.Parallel()

	var logs bytes.Buffer
	publisher := &Publisher{
		js:            &fakeJetStream{},
		logger:        slog.New(slog.NewTextHandler(&logs, nil)),
		subjectPrefix: "pipeline.changes",
		source:        "pipeline",
		wireFormat:    WireFormatProtoHeaders,
	}

	change := Change{
		Sequence:      42,
		TenantID:      "org-123",
		AggregateType: "deal",
		Operation:     "create",
		RecordedAt:    time.Date(2026, 4, 11, 18, 0, 0, 0, time.UTC),
		Payload:       MustPayload(wrapperspb.String("d-1")),
	}

	fake, ok := publisher.js.(*fakeJetStream)
	if !ok { t.Fatal("unexpected type") }
	publisher.PublishChange(context.Background(), change)

	if got := fake.header.Get(headerSpecVersion); got != "1.0" {
		t.Fatalf("unexpected specversion header %q", got)
	}
	if got := fake.header.Get(headerType); got != "pipeline.changes.deal.create" {
		t.Fatalf("unexpected type header %q", got)
	}
	if got := fake.header.Get(headerSource); got != "pipeline" {
		t.Fatalf("unexpected source header %q", got)
	}
	if got := fake.header.Get(headerTenantID); got != "org-123" {
		t.Fatalf("unexpected tenant header %q", got)
	}
	if got := fake.header.Get(headerDataContentType); got != "application/protobuf" {
		t.Fatalf("unexpected content-type header %q", got)
	}

	event, err := UnmarshalMessage(&nats.Msg{
		Subject: fake.subject,
		Header:  fake.header,
		Data:    fake.payload,
	})
	if err != nil {
		t.Fatalf("unmarshal header envelope: %v", err)
	}
	if event.WireFormat != WireFormatProtoHeaders {
		t.Fatalf("unexpected wire format %q", event.WireFormat)
	}
	target := &wrapperspb.StringValue{}
	if err := UnmarshalPayload(event.Payload, target); err != nil {
		t.Fatalf("unmarshal payload: %v", err)
	}
	if target.Value != "d-1" {
		t.Fatalf("unexpected payload value %q", target.Value)
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

func TestUnmarshalEnvelopeSupportsLegacyJSON(t *testing.T) {
	t.Parallel()

	payload, err := marshalEnvelope(Envelope{
		SpecVersion: "1.0",
		ID:          "evt-1",
		Type:        "pipeline.changes.deal.create",
		Source:      "pipeline",
		Time:        time.Date(2026, 4, 11, 18, 0, 0, 0, time.UTC),
		TenantID:    "org-123",
		Payload:     MustPayload(wrapperspb.String("d-1")),
	}, WireFormatJSON)
	if err != nil {
		t.Fatalf("marshal envelope: %v", err)
	}

	event, err := UnmarshalEnvelope(payload)
	if err != nil {
		t.Fatalf("unmarshal envelope: %v", err)
	}
	if event.WireFormat != WireFormatJSON {
		t.Fatalf("unexpected wire format %q", event.WireFormat)
	}
	target := &wrapperspb.StringValue{}
	if err := UnmarshalPayload(event.Payload, target); err != nil {
		t.Fatalf("unmarshal payload: %v", err)
	}
	if target.Value != "d-1" {
		t.Fatalf("unexpected payload value %q", target.Value)
	}
}

func TestUnmarshalEnvelopeRejectsEmptyPayload(t *testing.T) {
	t.Parallel()

	if _, err := UnmarshalEnvelope(nil); !errors.Is(err, errEnvelopeEmpty) {
		t.Fatalf("expected errEnvelopeEmpty, got %v", err)
	}
}

func TestUnmarshalEnvelopeSupportsProtoLeadingTagByte(t *testing.T) {
	t.Parallel()

	payload, err := marshalEnvelope(Envelope{
		SpecVersion: "1.0",
		ID:          "evt-1",
		Type:        "pipeline.changes.deal.create",
		Source:      "pipeline",
		Time:        time.Date(2026, 4, 11, 18, 0, 0, 0, time.UTC),
		TenantID:    "org-123",
		Payload:     MustPayload(wrapperspb.String("d-1")),
	}, WireFormatProto)
	if err != nil {
		t.Fatalf("marshal envelope: %v", err)
	}
	if len(payload) == 0 || payload[0] != 0x0a {
		t.Fatalf("expected protobuf envelope to start with 0x0a, got %x", payload)
	}

	event, err := UnmarshalEnvelope(payload)
	if err != nil {
		t.Fatalf("unmarshal envelope: %v", err)
	}
	if event.WireFormat != WireFormatProto {
		t.Fatalf("unexpected wire format %q", event.WireFormat)
	}
	if event.SpecVersion != "1.0" {
		t.Fatalf("unexpected spec version %q", event.SpecVersion)
	}
}

func TestUnmarshalMessageSupportsProtoHeaders(t *testing.T) {
	t.Parallel()

	payload := MustPayload(wrapperspb.String("d-1"))
	data, err := proto.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}

	event, err := UnmarshalMessage(&nats.Msg{
		Subject: "pipeline.changes.deal.create",
		Header:  newCloudEventHeader("evt-1", "pipeline.changes.deal.create", "pipeline", "2026-04-11T18:00:00Z", "org-123", "application/protobuf"),
		Data:    data,
	})
	if err != nil {
		t.Fatalf("unmarshal message: %v", err)
	}
	if event.WireFormat != WireFormatProtoHeaders {
		t.Fatalf("unexpected wire format %q", event.WireFormat)
	}
	target := &wrapperspb.StringValue{}
	if err := UnmarshalPayload(event.Payload, target); err != nil {
		t.Fatalf("unmarshal payload: %v", err)
	}
	if target.Value != "d-1" {
		t.Fatalf("unexpected payload value %q", target.Value)
	}
}

func TestUnmarshalMessageSupportsHeaderJSONPayload(t *testing.T) {
	t.Parallel()

	payload := MustPayload(wrapperspb.String("d-1"))
	data, err := marshalPayloadJSON(payload)
	if err != nil {
		t.Fatalf("marshal payload json: %v", err)
	}

	event, err := UnmarshalMessage(&nats.Msg{
		Subject: "pipeline.changes.deal.create",
		Header:  newCloudEventHeader("evt-1", "pipeline.changes.deal.create", "pipeline", "", "", "application/json"),
		Data:    data,
	})
	if err != nil {
		t.Fatalf("unmarshal message: %v", err)
	}
	target := &wrapperspb.StringValue{}
	if err := UnmarshalPayload(event.Payload, target); err != nil {
		t.Fatalf("unmarshal payload: %v", err)
	}
	if target.Value != "d-1" {
		t.Fatalf("unexpected payload value %q", target.Value)
	}
}

func TestUnmarshalMessageRejectsNilMessage(t *testing.T) {
	t.Parallel()

	if _, err := UnmarshalMessage(nil); !errors.Is(err, errMessageNil) {
		t.Fatalf("expected errMessageNil, got %v", err)
	}
}

func TestPublishReturnsNilOnSuccess(t *testing.T) {
	t.Parallel()

	publisher := &Publisher{
		js:            &fakeJetStream{},
		logger:        slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil)),
		subjectPrefix: "pipeline.changes",
		source:        "pipeline",
	}

	change := Change{
		Sequence:      1,
		TenantID:      "org-1",
		AggregateType: "deal",
		Operation:     "create",
		RecordedAt:    time.Date(2026, 4, 11, 18, 0, 0, 0, time.UTC),
		Payload:       MustPayload(wrapperspb.String("d-1")),
	}

	if err := publisher.Publish(context.Background(), change); err != nil {
		t.Fatalf("expected nil error, got %v", err)
	}

	fake := publisher.js.(*fakeJetStream)
	if fake.subject != "pipeline.changes.deal.create" {
		t.Fatalf("expected message to be published, got subject %q", fake.subject)
	}
}

func TestPublishReturnsErrorOnJetStreamFailure(t *testing.T) {
	t.Parallel()

	publisher := &Publisher{
		js:            &fakeJetStream{publishErr: errors.New("nats unavailable")},
		logger:        slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil)),
		subjectPrefix: "pipeline.changes",
		source:        "pipeline",
	}

	err := publisher.Publish(context.Background(), Change{
		Sequence:      7,
		TenantID:      "org-1",
		AggregateType: "deal",
		Operation:     "update",
		Payload:       MustPayload(wrapperspb.String("d-1")),
	})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
}

func TestPublishReturnsErrorOnNilPublisher(t *testing.T) {
	t.Parallel()

	var publisher *Publisher
	err := publisher.Publish(context.Background(), Change{})
	if !errors.Is(err, errPublisherNil) {
		t.Fatalf("expected errPublisherNil, got %v", err)
	}
}

func TestPublishReturnsErrorOnMarshalFailure(t *testing.T) {
	t.Parallel()

	publisher := &Publisher{
		js:            &fakeJetStream{},
		logger:        slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil)),
		subjectPrefix: "pipeline.changes",
		source:        "pipeline",
		wireFormat:    WireFormat("bogus"),
	}

	err := publisher.Publish(context.Background(), Change{
		Sequence:      1,
		TenantID:      "org-1",
		AggregateType: "deal",
		Operation:     "create",
		RecordedAt:    time.Date(2026, 4, 11, 18, 0, 0, 0, time.UTC),
		Payload:       MustPayload(wrapperspb.String("d-1")),
	})
	if err == nil {
		t.Fatal("expected error from invalid wire format, got nil")
	}
}

func TestPublishChangeStillLogsErrorsForBackwardCompat(t *testing.T) {
	t.Parallel()

	var logs bytes.Buffer
	publisher := &Publisher{
		js:            &fakeJetStream{publishErr: errors.New("nats unavailable")},
		logger:        slog.New(slog.NewTextHandler(&logs, nil)),
		subjectPrefix: "pipeline.changes",
		source:        "pipeline",
	}

	// PublishChange does not return an error — it logs instead.
	publisher.PublishChange(context.Background(), Change{
		Sequence:      7,
		TenantID:      "org-1",
		AggregateType: "deal",
		Operation:     "update",
		Payload:       MustPayload(wrapperspb.String("d-1")),
	})

	if !bytes.Contains(logs.Bytes(), []byte("failed to publish change event")) {
		t.Fatalf("expected publish error log, got %q", logs.String())
	}
}

func TestNoopPublisherPublishReturnsNil(t *testing.T) {
	t.Parallel()

	var noop NoopPublisher
	if err := noop.Publish(context.Background(), Change{}); err != nil {
		t.Fatalf("expected nil error from NoopPublisher.Publish, got %v", err)
	}
}

func TestChangePublisherInterfaceAcceptsBothTypes(t *testing.T) {
	t.Parallel()

	// Compile-time verification that both types satisfy the updated interface.
	var _ ChangePublisher = &Publisher{
		js:            &fakeJetStream{},
		subjectPrefix: "test",
		source:        "test",
	}
	var _ ChangePublisher = NoopPublisher{}
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
	header       nats.Header
	payload      []byte
	publishErr   error
}

func (fake *fakeJetStream) CreateOrUpdateStream(_ context.Context, cfg jetstream.StreamConfig) (jetstream.Stream, error) {
	fake.streamConfig = cfg
	return nil, nil
}

func (fake *fakeJetStream) PublishMsg(_ context.Context, message *nats.Msg, _ ...jetstream.PublishOpt) (*jetstream.PubAck, error) {
	fake.subject = message.Subject
	if message.Header != nil {
		fake.header = cloneHeader(message.Header)
	} else {
		fake.header = nil
	}
	fake.payload = append([]byte(nil), message.Data...)
	if fake.publishErr != nil {
		return nil, fake.publishErr
	}
	return &jetstream.PubAck{}, nil
}

func cloneHeader(header nats.Header) nats.Header {
	if header == nil {
		return nil
	}
	cloned := make(nats.Header, len(header))
	for key, values := range header {
		cloned[key] = append([]string(nil), values...)
	}
	return cloned
}

func newCloudEventHeader(id, eventType, source, eventTime, tenantID, contentType string) nats.Header {
	header := nats.Header{}
	header.Set(headerSpecVersion, "1.0")
	header.Set(headerID, id)
	header.Set(headerType, eventType)
	header.Set(headerSource, source)
	if eventTime != "" {
		header.Set(headerTime, eventTime)
	}
	if tenantID != "" {
		header.Set(headerTenantID, tenantID)
	}
	if contentType != "" {
		header.Set(headerDataContentType, contentType)
	}
	return header
}
