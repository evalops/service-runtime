package natsbus

import (
	"bytes"
	"context"
	"log/slog"
	"testing"
	"time"

	"github.com/evalops/service-runtime/testutil"
	"github.com/nats-io/nats.go"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	tracetest "go.opentelemetry.io/otel/sdk/trace/tracetest"
	semconv "go.opentelemetry.io/otel/semconv/v1.40.0"
	"go.opentelemetry.io/otel/trace"
	"google.golang.org/protobuf/types/known/wrapperspb"
)

func TestPublishInjectsTraceHeadersForJSONWireFormatAndStartsProducerSpan(t *testing.T) {
	recorder := installNATSBusTelemetry(t)

	publisher := &Publisher{
		js:            &fakeJetStream{},
		logger:        slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil)),
		subjectPrefix: "runtime.events",
		source:        "runtime",
		wireFormat:    WireFormatJSON,
	}

	ctx, root := otel.Tracer("natsbus-test").Start(context.Background(), "root")
	rootContext := trace.SpanContextFromContext(ctx)

	err := publisher.Publish(ctx, Change{
		Sequence:      7,
		TenantID:      "org-123",
		AggregateType: "widget",
		Operation:     "updated",
		RecordedAt:    time.Date(2026, 4, 15, 19, 0, 0, 0, time.UTC),
		Payload:       MustPayload(wrapperspb.String("ok")),
	})
	root.End()
	if err != nil {
		t.Fatalf("Publish() error = %v", err)
	}

	fake, ok := publisher.js.(*fakeJetStream)
	if !ok {
		t.Fatal("expected fake jetstream")
	}
	if got := fake.header.Get(headerTraceParent); got == "" {
		t.Fatal("expected traceparent header on published message")
	}

	producer := waitForEndedSpan(t, recorder, "nats.publish")
	if got, want := producer.SpanKind(), trace.SpanKindProducer; got != want {
		t.Fatalf("producer span kind = %v, want %v", got, want)
	}
	if got, want := producer.Parent().SpanID(), rootContext.SpanID(); got != want {
		t.Fatalf("producer parent span id = %s, want %s", got, want)
	}
	if got, want := spanAttribute(producer, string(semconv.MessagingSystemKey)), "nats"; got != want {
		t.Fatalf("messaging.system = %q, want %q", got, want)
	}
	if got, want := spanAttribute(producer, string(semconv.MessagingDestinationNameKey)), "runtime.events.widget.updated"; got != want {
		t.Fatalf("messaging.destination.name = %q, want %q", got, want)
	}
}

func TestReliablePublishInjectsTraceHeadersAndStartsProducerSpan(t *testing.T) {
	recorder := installNATSBusTelemetry(t)

	js := &reliableFakeJetStream{}
	reliable := newTestReliablePublisher(t, js, ReliableOptions{})

	ctx, root := otel.Tracer("natsbus-test").Start(context.Background(), "root")
	rootContext := trace.SpanContextFromContext(ctx)

	err := reliable.Publish(ctx, testReliableChange(7))
	root.End()
	if err != nil {
		t.Fatalf("Publish() error = %v", err)
	}

	messages := js.Messages()
	if len(messages) != 1 {
		t.Fatalf("expected 1 published message, got %d", len(messages))
	}
	if got := messages[0].Header.Get(headerTraceParent); got == "" {
		t.Fatal("expected traceparent header on published message")
	}

	producer := waitForEndedSpan(t, recorder, "nats.publish")
	if got, want := producer.SpanKind(), trace.SpanKindProducer; got != want {
		t.Fatalf("producer span kind = %v, want %v", got, want)
	}
	if got, want := producer.Parent().SpanID(), rootContext.SpanID(); got != want {
		t.Fatalf("producer parent span id = %s, want %s", got, want)
	}

	propagated := trace.SpanContextFromContext(extractMessageContext(messages[0]))
	if !propagated.IsValid() {
		t.Fatal("expected trace context on published message")
	}
	if got, want := propagated.TraceID(), rootContext.TraceID(); got != want {
		t.Fatalf("propagated trace id = %s, want %s", got, want)
	}
	if got, want := propagated.SpanID(), producer.SpanContext().SpanID(); got != want {
		t.Fatalf("propagated span id = %s, want %s", got, want)
	}
}

func TestPublishSubscribeRoundTripPropagatesTraceContextAndStartsConsumerSpan(t *testing.T) {
	recorder := installNATSBusTelemetry(t)

	conn, natsURL := testutil.NewTestNATSServer(t)
	_ = conn

	publisher, err := ConnectWithOptions(
		context.Background(),
		natsURL,
		"runtime_events",
		"runtime.events",
		Options{
			Logger:     slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil)),
			WireFormat: WireFormatJSON,
		},
	)
	if err != nil {
		t.Fatalf("ConnectWithOptions() error = %v", err)
	}
	t.Cleanup(publisher.Close)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	type handledMessage struct {
		traceID trace.TraceID
		header  string
	}
	handled := make(chan handledMessage, 1)

	cleanup, err := Subscribe(ctx, natsURL, ConsumerOptions{
		Logger:  slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil)),
		Stream:  "runtime_events",
		Subject: "runtime.events.widget.updated",
		Durable: "runtime-observer",
	}, func(ctx context.Context, message *nats.Msg) error {
		spanContext := trace.SpanContextFromContext(ctx)
		handled <- handledMessage{
			traceID: spanContext.TraceID(),
			header:  message.Header.Get(headerTraceParent),
		}
		return nil
	})
	if err != nil {
		t.Fatalf("Subscribe() error = %v", err)
	}
	t.Cleanup(func() {
		if err := cleanup(context.Background()); err != nil {
			t.Errorf("cleanup() error = %v", err)
		}
	})

	rootCtx, root := otel.Tracer("natsbus-test").Start(context.Background(), "root")
	rootContext := trace.SpanContextFromContext(rootCtx)

	err = publisher.Publish(rootCtx, Change{
		Sequence:      8,
		TenantID:      "org-123",
		AggregateType: "widget",
		Operation:     "updated",
		RecordedAt:    time.Date(2026, 4, 15, 19, 5, 0, 0, time.UTC),
		Payload:       MustPayload(wrapperspb.String("ok")),
	})
	root.End()
	if err != nil {
		t.Fatalf("Publish() error = %v", err)
	}

	select {
	case got := <-handled:
		if got.header == "" {
			t.Fatal("expected traceparent header on consumed message")
		}
		if got.traceID != rootContext.TraceID() {
			t.Fatalf("handler trace id = %s, want %s", got.traceID, rootContext.TraceID())
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for nats handler")
	}

	producer := waitForEndedSpan(t, recorder, "nats.publish")
	consumer := waitForEndedSpan(t, recorder, "nats.process")

	if got, want := producer.SpanKind(), trace.SpanKindProducer; got != want {
		t.Fatalf("producer span kind = %v, want %v", got, want)
	}
	if got, want := consumer.SpanKind(), trace.SpanKindConsumer; got != want {
		t.Fatalf("consumer span kind = %v, want %v", got, want)
	}
	if got, want := producer.Parent().SpanID(), rootContext.SpanID(); got != want {
		t.Fatalf("producer parent span id = %s, want %s", got, want)
	}
	if got, want := consumer.Parent().SpanID(), producer.SpanContext().SpanID(); got != want {
		t.Fatalf("consumer parent span id = %s, want %s", got, want)
	}
	if got, want := spanAttribute(consumer, string(semconv.MessagingDestinationNameKey)), "runtime.events.widget.updated"; got != want {
		t.Fatalf("consumer messaging.destination.name = %q, want %q", got, want)
	}
	if got, want := spanAttribute(consumer, string(semconv.MessagingDestinationSubscriptionNameKey)), "runtime-observer"; got != want {
		t.Fatalf("consumer messaging.destination.subscription.name = %q, want %q", got, want)
	}
}

func installNATSBusTelemetry(t *testing.T) *tracetest.SpanRecorder {
	t.Helper()

	originalProvider := otel.GetTracerProvider()
	originalPropagator := otel.GetTextMapPropagator()
	recorder := tracetest.NewSpanRecorder()
	tracerProvider := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(recorder))
	otel.SetTracerProvider(tracerProvider)
	otel.SetTextMapPropagator(propagation.TraceContext{})
	t.Cleanup(func() {
		otel.SetTracerProvider(originalProvider)
		otel.SetTextMapPropagator(originalPropagator)
		_ = tracerProvider.Shutdown(context.Background())
	})

	return recorder
}

func waitForEndedSpan(t *testing.T, recorder *tracetest.SpanRecorder, name string) sdktrace.ReadOnlySpan {
	t.Helper()

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		for _, span := range recorder.Ended() {
			if span.Name() == name {
				return span
			}
		}
		time.Sleep(25 * time.Millisecond)
	}

	t.Fatalf("timed out waiting for ended span %q", name)
	return nil
}

func spanAttribute(span sdktrace.ReadOnlySpan, key string) string {
	for _, attribute := range span.Attributes() {
		if string(attribute.Key) == key {
			return attribute.Value.AsString()
		}
	}
	return ""
}
