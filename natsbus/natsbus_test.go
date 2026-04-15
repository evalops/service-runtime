package natsbus

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/evalops/service-runtime/resilience"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
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
	if !ok {
		t.Fatal("unexpected type")
	}
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
	if !ok {
		t.Fatal("unexpected type")
	}
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
	if !ok {
		t.Fatal("unexpected type")
	}
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

func TestPublishChangePropagatesTraceContext(t *testing.T) {
	originalProvider := otel.GetTracerProvider()
	originalPropagator := otel.GetTextMapPropagator()
	tracerProvider := sdktrace.NewTracerProvider()
	otel.SetTracerProvider(tracerProvider)
	otel.SetTextMapPropagator(propagation.TraceContext{})
	t.Cleanup(func() {
		otel.SetTracerProvider(originalProvider)
		otel.SetTextMapPropagator(originalPropagator)
		_ = tracerProvider.Shutdown(context.Background())
	})

	publisher := &Publisher{
		js:            &fakeJetStream{},
		logger:        slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil)),
		subjectPrefix: "pipeline.changes",
		source:        "pipeline",
		wireFormat:    WireFormatProtoHeaders,
	}

	ctx, span := tracerProvider.Tracer("natsbus-test").Start(context.Background(), "root")
	defer span.End()

	change := Change{
		Sequence:      42,
		TenantID:      "org-123",
		AggregateType: "deal",
		Operation:     "create",
		RecordedAt:    time.Date(2026, 4, 11, 18, 0, 0, 0, time.UTC),
		Payload:       MustPayload(wrapperspb.String("d-1")),
	}

	fake, ok := publisher.js.(*fakeJetStream)
	if !ok {
		t.Fatal("unexpected type")
	}
	if err := publisher.Publish(ctx, change); err != nil {
		t.Fatalf("publish: %v", err)
	}
	if got := fake.header.Get(headerTraceParent); got == "" {
		t.Fatal("expected traceparent header")
	}

	event, err := UnmarshalMessage(&nats.Msg{
		Subject: fake.subject,
		Header:  fake.header,
		Data:    fake.payload,
	})
	if err != nil {
		t.Fatalf("unmarshal header envelope: %v", err)
	}
	if event.TraceParent == "" {
		t.Fatal("expected envelope traceparent")
	}
	extracted := ExtractContext(context.Background(), event)
	if got, want := trace.SpanContextFromContext(extracted).TraceID(), trace.SpanContextFromContext(ctx).TraceID(); got != want {
		t.Fatalf("trace id = %s, want %s", got, want)
	}
}

func TestInjectTraceContextFallbackIncludesTraceStateAndMasksFlags(t *testing.T) {
	originalPropagator := otel.GetTextMapPropagator()
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator())
	t.Cleanup(func() {
		otel.SetTextMapPropagator(originalPropagator)
	})

	traceState, err := trace.ParseTraceState("vendor=value")
	if err != nil {
		t.Fatalf("parse tracestate: %v", err)
	}

	ctx := trace.ContextWithSpanContext(context.Background(), trace.NewSpanContext(trace.SpanContextConfig{
		TraceID:    trace.TraceID{0x00, 0x11, 0x22, 0x33, 0x44, 0x55, 0x66, 0x77, 0x88, 0x99, 0xaa, 0xbb, 0xcc, 0xdd, 0xee, 0xff},
		SpanID:     trace.SpanID{0x01, 0x23, 0x45, 0x67, 0x89, 0xab, 0xcd, 0xef},
		TraceFlags: trace.FlagsSampled | trace.FlagsRandom | trace.TraceFlags(0x04),
		TraceState: traceState,
	}))

	envelope := injectTraceContext(ctx, Envelope{})
	if got, want := envelope.TraceState, traceState.String(); got != want {
		t.Fatalf("trace state = %q, want %q", got, want)
	}
	if got, want := envelope.TraceParent, "00-00112233445566778899aabbccddeeff-0123456789abcdef-03"; got != want {
		t.Fatalf("traceparent = %q, want %q", got, want)
	}
}

func TestExtractContextFallsBackToTraceParentWithoutGlobalPropagator(t *testing.T) {
	originalPropagator := otel.GetTextMapPropagator()
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator())
	t.Cleanup(func() {
		otel.SetTextMapPropagator(originalPropagator)
	})

	traceState, err := trace.ParseTraceState("vendor=value")
	if err != nil {
		t.Fatalf("parse tracestate: %v", err)
	}

	parent := trace.NewSpanContext(trace.SpanContextConfig{
		TraceID:    trace.TraceID{0x00, 0x11, 0x22, 0x33, 0x44, 0x55, 0x66, 0x77, 0x88, 0x99, 0xaa, 0xbb, 0xcc, 0xdd, 0xee, 0xff},
		SpanID:     trace.SpanID{0x01, 0x23, 0x45, 0x67, 0x89, 0xab, 0xcd, 0xef},
		TraceFlags: trace.FlagsSampled | trace.FlagsRandom,
		TraceState: traceState,
	})

	envelope := injectTraceContext(trace.ContextWithSpanContext(context.Background(), parent), Envelope{})
	extracted := trace.SpanContextFromContext(ExtractContext(context.Background(), envelope))

	if !extracted.IsValid() {
		t.Fatal("expected extracted span context")
	}
	if !extracted.IsRemote() {
		t.Fatal("expected remote span context")
	}
	if got, want := extracted.TraceID(), parent.TraceID(); got != want {
		t.Fatalf("trace id = %s, want %s", got, want)
	}
	if got, want := extracted.SpanID(), parent.SpanID(); got != want {
		t.Fatalf("span id = %s, want %s", got, want)
	}
	if got, want := extracted.TraceFlags(), parent.TraceFlags()&(trace.FlagsSampled|trace.FlagsRandom); got != want {
		t.Fatalf("trace flags = %s, want %s", got, want)
	}
	if got, want := extracted.TraceState().String(), traceState.String(); got != want {
		t.Fatalf("trace state = %q, want %q", got, want)
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

	fake, ok := publisher.js.(*fakeJetStream)
	if !ok {
		t.Fatalf("expected fakeJetStream, got %T", publisher.js)
	}
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
	if !strings.Contains(err.Error(), "nats unavailable") {
		t.Fatalf("expected wrapped nats error, got %v", err)
	}
}

func TestPublishReturnsErrorOnNilPublisher(t *testing.T) {
	t.Parallel()

	var publisher *Publisher
	err := publisher.Publish(context.Background(), Change{})
	if !errors.Is(err, ErrPublisherNil) {
		t.Fatalf("expected ErrPublisherNil, got %v", err)
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

func TestChangePublisherInterfaceAcceptsSupportedTypes(t *testing.T) {
	t.Parallel()

	// Compile-time verification that all publisher types satisfy the interface.
	var _ ChangePublisher = &Publisher{
		js:            &fakeJetStream{},
		subjectPrefix: "test",
		source:        "test",
	}
	var _ ChangePublisher = &ReliablePublisher{
		publisher: &Publisher{
			js:            &fakeJetStream{},
			subjectPrefix: "test",
			source:        "test",
		},
	}
	var _ ChangePublisher = NoopPublisher{}
}

func TestConnectReliableWithOptionsRejectsMissingDeadLetterDir(t *testing.T) {
	t.Parallel()

	_, err := newReliablePublisher(&Publisher{
		js:            &fakeJetStream{},
		logger:        slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil)),
		subjectPrefix: "pipeline.changes",
		source:        "pipeline",
	}, ReliableOptions{})
	if !errors.Is(err, errDeadLetterDirRequired) {
		t.Fatalf("expected errDeadLetterDirRequired, got %v", err)
	}
}

func TestReliablePublisherRetriesTransientFailures(t *testing.T) {
	t.Parallel()

	fake := &fakeJetStream{
		publishErrs: []error{errors.New("nats unavailable"), nil},
	}
	publisher := newTestReliablePublisher(t, fake, ReliableOptions{
		DeadLetterDir:  t.TempDir(),
		Registerer:     prometheus.NewRegistry(),
		ReplayInterval: time.Hour,
		Retry: resilience.RetryConfig{
			MaxAttempts:  2,
			InitialDelay: time.Millisecond,
			MaxDelay:     time.Millisecond,
		},
	})

	err := publisher.Publish(context.Background(), Change{
		Sequence:      1,
		TenantID:      "org-1",
		AggregateType: "deal",
		Operation:     "create",
		Payload:       MustPayload(wrapperspb.String("d-1")),
	})
	if err != nil {
		t.Fatalf("expected transient failure to recover, got %v", err)
	}
	if fake.publishCalls != 2 {
		t.Fatalf("publish calls = %d, want 2", fake.publishCalls)
	}
	if count, err := publisher.deadLetters.count(); err != nil || count != 0 {
		t.Fatalf("dead-letter count = %d, err = %v, want 0", count, err)
	}
}

func TestReliablePublisherQueuesDeadLettersAndOpensBreaker(t *testing.T) {
	t.Parallel()

	registry := prometheus.NewRegistry()
	fake := &fakeJetStream{
		publishErr: errors.New("nats unavailable"),
	}
	publisher := newTestReliablePublisher(t, fake, ReliableOptions{
		DeadLetterDir:  t.TempDir(),
		Registerer:     registry,
		ReplayInterval: time.Hour,
		Retry: resilience.RetryConfig{
			MaxAttempts:  1,
			InitialDelay: time.Millisecond,
			MaxDelay:     time.Millisecond,
		},
		Breaker: resilience.BreakerConfig{
			FailureThreshold: 1,
			ResetTimeout:     time.Hour,
		},
	})
	change := Change{
		Sequence:      7,
		TenantID:      "org-1",
		AggregateType: "deal",
		Operation:     "update",
		Payload:       MustPayload(wrapperspb.String("d-1")),
	}

	err := publisher.Publish(context.Background(), change)
	if err == nil {
		t.Fatal("expected publish error, got nil")
	}
	if !strings.Contains(err.Error(), "queued for replay") {
		t.Fatalf("expected queued-for-replay error, got %v", err)
	}

	err = publisher.Publish(context.Background(), change)
	if !errors.Is(err, resilience.ErrCircuitOpen) {
		t.Fatalf("expected ErrCircuitOpen, got %v", err)
	}
	if fake.publishCalls != 1 {
		t.Fatalf("publish calls = %d, want 1 after breaker opens", fake.publishCalls)
	}

	if count, err := publisher.deadLetters.count(); err != nil || count != 2 {
		t.Fatalf("dead-letter count = %d, err = %v, want 2", count, err)
	}

	if got := counterValue(t, publisher.metrics.publishFailures.WithLabelValues("pipeline.changes")); got != 2 {
		t.Fatalf("publish failure metric = %v, want 2", got)
	}
	if got := gaugeValue(t, publisher.metrics.deadLetterSize.WithLabelValues("pipeline.changes")); got != 2 {
		t.Fatalf("dead-letter size metric = %v, want 2", got)
	}
}

func TestReliablePublisherReplayPendingDeletesDeadLetters(t *testing.T) {
	t.Parallel()

	registry := prometheus.NewRegistry()
	fake := &fakeJetStream{
		publishErr: errors.New("nats unavailable"),
	}
	publisher := newTestReliablePublisher(t, fake, ReliableOptions{
		DeadLetterDir:  t.TempDir(),
		Registerer:     registry,
		ReplayInterval: time.Hour,
		Retry: resilience.RetryConfig{
			MaxAttempts:  1,
			InitialDelay: time.Millisecond,
			MaxDelay:     time.Millisecond,
		},
		Breaker: resilience.BreakerConfig{
			FailureThreshold: 1,
			ResetTimeout:     time.Hour,
		},
	})

	if err := publisher.Publish(context.Background(), Change{
		Sequence:      9,
		TenantID:      "org-1",
		AggregateType: "deal",
		Operation:     "create",
		Payload:       MustPayload(wrapperspb.String("d-1")),
	}); err == nil {
		t.Fatal("expected publish error, got nil")
	}

	publisher.Close()
	fake.publishErr = nil
	publisher.breaker.Reset()
	if err := publisher.replayPending(context.Background()); err != nil {
		t.Fatalf("replayPending() error = %v", err)
	}

	if fake.publishCalls != 2 {
		t.Fatalf("publish calls = %d, want 2", fake.publishCalls)
	}
	if count, err := publisher.deadLetters.count(); err != nil || count != 0 {
		t.Fatalf("dead-letter count = %d, err = %v, want 0", count, err)
	}
	if got := gaugeValue(t, publisher.metrics.deadLetterSize.WithLabelValues("pipeline.changes")); got != 0 {
		t.Fatalf("dead-letter size metric = %v, want 0", got)
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
	header       nats.Header
	payload      []byte
	publishCalls int
	publishErrs  []error
	publishErr   error
}

func (fake *fakeJetStream) CreateOrUpdateStream(_ context.Context, cfg jetstream.StreamConfig) (jetstream.Stream, error) {
	fake.streamConfig = cfg
	return nil, nil
}

func (fake *fakeJetStream) PublishMsg(_ context.Context, message *nats.Msg, _ ...jetstream.PublishOpt) (*jetstream.PubAck, error) {
	fake.publishCalls++
	fake.subject = message.Subject
	if message.Header != nil {
		fake.header = cloneHeader(message.Header)
	} else {
		fake.header = nil
	}
	fake.payload = append([]byte(nil), message.Data...)
	if len(fake.publishErrs) > 0 {
		err := fake.publishErrs[0]
		fake.publishErrs = fake.publishErrs[1:]
		if err != nil {
			return nil, err
		}
		return &jetstream.PubAck{}, nil
	}
	if fake.publishErr != nil {
		return nil, fake.publishErr
	}
	return &jetstream.PubAck{}, nil
}

func newTestReliablePublisher(t *testing.T, js *fakeJetStream, opts ReliableOptions) *ReliablePublisher {
	t.Helper()

	publisher, err := newReliablePublisher(&Publisher{
		js:            js,
		logger:        slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil)),
		subjectPrefix: "pipeline.changes",
		source:        "pipeline",
	}, opts)
	if err != nil {
		t.Fatalf("newReliablePublisher() error = %v", err)
	}
	t.Cleanup(publisher.Close)
	return publisher
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

func counterValue(t *testing.T, counter prometheus.Counter) float64 {
	t.Helper()

	metric := &dto.Metric{}
	if err := counter.Write(metric); err != nil {
		t.Fatalf("counter.Write() error = %v", err)
	}
	if metric.Counter == nil {
		t.Fatal("counter metric missing counter payload")
	}
	return metric.Counter.GetValue()
}

func gaugeValue(t *testing.T, gauge prometheus.Gauge) float64 {
	t.Helper()

	metric := &dto.Metric{}
	if err := gauge.Write(metric); err != nil {
		t.Fatalf("gauge.Write() error = %v", err)
	}
	if metric.Gauge == nil {
		t.Fatal("gauge metric missing gauge payload")
	}
	return metric.Gauge.GetValue()
}
