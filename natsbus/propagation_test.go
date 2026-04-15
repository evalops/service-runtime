package natsbus

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"testing"
	"time"

	"github.com/evalops/service-runtime/testutil"
	"github.com/nats-io/nats.go"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
	"google.golang.org/protobuf/types/known/wrapperspb"
)

func TestPropagatingPublisherAddsProducerSpanAndHeaderTraceContext(t *testing.T) {
	recorder := installNATSTestTracerProvider(t)

	publisher := NewPropagatingPublisher(&Publisher{
		js:            &fakeJetStream{},
		logger:        slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil)),
		subjectPrefix: "trace.events",
		source:        "trace",
	})

	ctx, root := otel.Tracer("natsbus-test").Start(context.Background(), "root")
	change := Change{
		Sequence:      42,
		TenantID:      "org-123",
		AggregateType: "order",
		Operation:     "created",
		RecordedAt:    time.Date(2026, 4, 15, 18, 0, 0, 0, time.UTC),
		Payload:       MustPayload(wrapperspb.String("o-1")),
	}

	if err := publisher.Publish(ctx, change); err != nil {
		t.Fatalf("publish: %v", err)
	}
	root.End()

	fake, ok := publisher.publisher.js.(*fakeJetStream)
	if !ok {
		t.Fatalf("expected fakeJetStream, got %T", publisher.publisher.js)
	}
	if got := fake.header.Get(headerTraceParent); got == "" {
		t.Fatal("expected traceparent header")
	}

	publishSpan := findEndedSpan(t, recorder, "nats.publish")
	if got, want := publishSpan.SpanKind(), trace.SpanKindProducer; got != want {
		t.Fatalf("span kind = %v, want %v", got, want)
	}
	if got, want := publishSpan.Parent().SpanID(), root.SpanContext().SpanID(); got != want {
		t.Fatalf("parent span id = %s, want %s", got, want)
	}
	assertSpanAttribute(t, publishSpan, "messaging.system", "nats")
	assertSpanAttribute(t, publishSpan, "messaging.destination", "trace.events.order.created")
}

func TestPropagatingConsumerCreatesConsumerSpanFromMessageHeaders(t *testing.T) {
	recorder := installNATSTestTracerProvider(t)

	rootCtx, root := otel.Tracer("natsbus-test").Start(context.Background(), "root")
	publishCtx, publish := otel.Tracer("natsbus-test").Start(rootCtx, "producer")

	message := &nats.Msg{
		Subject: "trace.events.order.created",
		Header:  nats.Header{},
	}
	injectMessageTraceContext(publishCtx, message)
	publish.End()
	root.End()

	var handlerTraceID trace.TraceID
	consumer := NewPropagatingConsumer(func(ctx context.Context, message *nats.Msg) error {
		handlerTraceID = trace.SpanContextFromContext(ctx).TraceID()
		return nil
	})

	if err := consumer.Consume(context.Background(), message); err != nil {
		t.Fatalf("consume: %v", err)
	}

	processSpan := findEndedSpan(t, recorder, "nats.process")
	if got, want := processSpan.SpanKind(), trace.SpanKindConsumer; got != want {
		t.Fatalf("span kind = %v, want %v", got, want)
	}
	if got, want := handlerTraceID, root.SpanContext().TraceID(); got != want {
		t.Fatalf("handler trace id = %s, want %s", got, want)
	}
	if got, want := processSpan.Parent().SpanID(), publish.SpanContext().SpanID(); got != want {
		t.Fatalf("parent span id = %s, want %s", got, want)
	}
	if len(processSpan.Links()) == 0 {
		t.Fatal("expected consumer span to link to producer span")
	}
	if got, want := processSpan.Links()[0].SpanContext.SpanID(), publish.SpanContext().SpanID(); got != want {
		t.Fatalf("linked span id = %s, want %s", got, want)
	}
	assertSpanAttribute(t, processSpan, "messaging.system", "nats")
	assertSpanAttribute(t, processSpan, "messaging.destination", "trace.events.order.created")
}

func TestPropagatingPublisherAndConsumerRoundTripTraceContext(t *testing.T) {
	recorder := installNATSTestTracerProvider(t)

	_, natsURL := testutil.NewTestNATSServer(t)

	streamName := fmt.Sprintf("TRACE_%d", time.Now().UnixNano())
	durable := fmt.Sprintf("trace-consumer-%d", time.Now().UnixNano())
	publisher, err := ConnectWithOptions(context.Background(), natsURL, streamName, "trace.events", Options{})
	if err != nil {
		t.Fatalf("connect publisher: %v", err)
	}
	t.Cleanup(publisher.Close)

	propagatingPublisher := NewPropagatingPublisher(publisher)

	received := make(chan trace.SpanContext, 1)
	shutdown, err := Subscribe(context.Background(), natsURL, ConsumerOptions{
		Stream:  streamName,
		Subject: "trace.events.order.created",
		Durable: durable,
		Queue:   durable,
	}, NewPropagatingConsumer(func(ctx context.Context, message *nats.Msg) error {
		received <- trace.SpanContextFromContext(ctx)
		return nil
	}).Consume)
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	t.Cleanup(func() {
		if err := shutdown(context.Background()); err != nil {
			t.Errorf("shutdown: %v", err)
		}
	})

	rootCtx, root := otel.Tracer("natsbus-test").Start(context.Background(), "root")
	change := Change{
		Sequence:      7,
		TenantID:      "org-123",
		AggregateType: "order",
		Operation:     "created",
		RecordedAt:    time.Now().UTC(),
		Payload:       MustPayload(wrapperspb.String("o-7")),
	}
	if err := propagatingPublisher.Publish(rootCtx, change); err != nil {
		t.Fatalf("publish: %v", err)
	}
	root.End()

	var receivedSpan trace.SpanContext
	select {
	case receivedSpan = <-received:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for propagated consumer context")
	}

	if got, want := receivedSpan.TraceID(), root.SpanContext().TraceID(); got != want {
		t.Fatalf("consumer trace id = %s, want %s", got, want)
	}

	waitForEndedSpans(t, recorder, 3)
	publishSpan := findEndedSpan(t, recorder, "nats.publish")
	processSpan := findEndedSpan(t, recorder, "nats.process")
	if got, want := processSpan.Parent().SpanID(), publishSpan.SpanContext().SpanID(); got != want {
		t.Fatalf("consumer parent span id = %s, want %s", got, want)
	}
}

func installNATSTestTracerProvider(t *testing.T) *tracetest.SpanRecorder {
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

func findEndedSpan(t *testing.T, recorder *tracetest.SpanRecorder, name string) sdktrace.ReadOnlySpan {
	t.Helper()

	for _, span := range recorder.Ended() {
		if span.Name() == name {
			return span
		}
	}

	t.Fatalf("expected span %q to be recorded", name)
	return nil
}

func assertSpanAttribute(t *testing.T, span sdktrace.ReadOnlySpan, key, want string) {
	t.Helper()

	for _, attr := range span.Attributes() {
		if attr.Key != attribute.Key(key) {
			continue
		}
		if got := attr.Value.AsString(); got != want {
			t.Fatalf("attribute %q = %q, want %q", key, got, want)
		}
		return
	}

	t.Fatalf("expected attribute %q on span %q", key, span.Name())
}

func waitForEndedSpans(t *testing.T, recorder *tracetest.SpanRecorder, want int) {
	t.Helper()

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if len(recorder.Ended()) >= want {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}

	t.Fatalf("expected at least %d ended spans, got %d", want, len(recorder.Ended()))
}
