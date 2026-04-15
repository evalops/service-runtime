package natsbus

import (
	"context"
	"fmt"

	"github.com/nats-io/nats.go"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/propagation"
	semconv "go.opentelemetry.io/otel/semconv/v1.40.0"
	"go.opentelemetry.io/otel/trace"
)

const propagationTracerName = "github.com/evalops/service-runtime/natsbus"

var (
	messagingSystemNATSAttr = attribute.String("messaging.system", "nats")
)

// PropagatingPublisher wraps a Publisher with producer spans and header-level
// trace propagation.
type PropagatingPublisher struct {
	publisher *Publisher
	tracer    trace.Tracer
}

// NewPropagatingPublisher returns an opt-in publisher wrapper that emits
// producer spans and injects trace context into NATS headers.
func NewPropagatingPublisher(publisher *Publisher) *PropagatingPublisher {
	return &PropagatingPublisher{
		publisher: publisher,
		tracer:    otel.Tracer(propagationTracerName),
	}
}

// Publish creates a producer span, injects trace context into NATS headers, and
// publishes the wrapped change event through the underlying publisher.
func (publisher *PropagatingPublisher) Publish(ctx context.Context, change Change) error {
	if publisher == nil || publisher.publisher == nil || publisher.publisher.js == nil {
		return ErrPublisherNil
	}

	subject := change.subject(publisher.publisher.subjectPrefix)
	ctx, span := publisher.tracer.Start(
		ctx,
		"nats.publish",
		trace.WithSpanKind(trace.SpanKindProducer),
		trace.WithAttributes(publishSpanAttributes(subject)...),
	)
	defer span.End()

	message, err := publisher.publisher.buildMessage(ctx, change)
	if err != nil {
		recordSpanError(span, err)
		return err
	}

	injectMessageTraceContext(ctx, message)

	if _, err := publisher.publisher.js.PublishMsg(ctx, message); err != nil {
		err = fmt.Errorf("publish %s: %w", message.Subject, err)
		recordSpanError(span, err)
		return err
	}

	publisher.publisher.loggerOrDefault().Debug("published change event", "seq", change.Sequence, "subject", message.Subject)
	return nil
}

// PublishChange publishes the change event and logs failures without returning
// them, matching Publisher.PublishChange.
func (publisher *PropagatingPublisher) PublishChange(ctx context.Context, change Change) {
	if err := publisher.Publish(ctx, change); err != nil {
		if publisher != nil && publisher.publisher != nil {
			publisher.publisher.loggerOrDefault().Error("failed to publish change event", "error", err, "seq", change.Sequence)
		}
	}
}

// PropagatingConsumer wraps a message handler with consumer spans and trace
// context extraction from NATS headers.
type PropagatingConsumer struct {
	handler Consumer
	tracer  trace.Tracer
}

// NewPropagatingConsumer returns an opt-in consumer wrapper that extracts trace
// context from NATS headers and emits consumer spans around the handler.
func NewPropagatingConsumer(handler Consumer) *PropagatingConsumer {
	return &PropagatingConsumer{
		handler: handler,
		tracer:  otel.Tracer(propagationTracerName),
	}
}

// Consume extracts upstream trace context from the message, starts a consumer
// span, and invokes the wrapped handler.
func (consumer *PropagatingConsumer) Consume(_ context.Context, message *nats.Msg) error {
	if consumer == nil || consumer.handler == nil {
		return errConsumerHandlerNil
	}
	if message == nil {
		return errMessageNil
	}

	parentCtx := extractMessageContext(message)
	parentSpanContext := trace.SpanContextFromContext(parentCtx)

	options := []trace.SpanStartOption{
		trace.WithSpanKind(trace.SpanKindConsumer),
		trace.WithAttributes(processSpanAttributes(message.Subject)...),
	}
	if parentSpanContext.IsValid() {
		options = append(options, trace.WithLinks(trace.Link{SpanContext: parentSpanContext}))
	}

	ctx, span := consumer.tracer.Start(parentCtx, "nats.process", options...)
	defer span.End()

	if err := consumer.handler(ctx, message); err != nil {
		recordSpanError(span, err)
		return err
	}

	return nil
}

func publishSpanAttributes(subject string) []attribute.KeyValue {
	return []attribute.KeyValue{
		messagingSystemNATSAttr,
		attribute.String("messaging.destination", subject),
		semconv.MessagingDestinationName(subject),
		semconv.MessagingOperationName("publish"),
		semconv.MessagingOperationTypeSend,
	}
}

func processSpanAttributes(subject string) []attribute.KeyValue {
	return []attribute.KeyValue{
		messagingSystemNATSAttr,
		attribute.String("messaging.destination", subject),
		semconv.MessagingDestinationName(subject),
		semconv.MessagingOperationName("process"),
		semconv.MessagingOperationTypeProcess,
	}
}

func recordSpanError(span trace.Span, err error) {
	if span == nil || err == nil {
		return
	}
	span.RecordError(err)
	span.SetStatus(codes.Error, err.Error())
}

func extractMessageContext(message *nats.Msg) context.Context {
	ctx := context.Background()
	if message == nil {
		return ctx
	}

	carrier := newNATSHeaderCarrier(&message.Header)
	if headerTraceContextPresent(message.Header) {
		ctx = otel.GetTextMapPropagator().Extract(ctx, carrier)
		if trace.SpanContextFromContext(ctx).IsValid() {
			return ctx
		}
		return propagationFallbackExtract(ctx, carrier)
	}

	envelope, err := UnmarshalMessage(message)
	if err != nil {
		return ctx
	}
	return ExtractContext(ctx, envelope)
}

func injectMessageTraceContext(ctx context.Context, message *nats.Msg) {
	if message == nil {
		return
	}

	carrier := newNATSHeaderCarrier(&message.Header)
	otel.GetTextMapPropagator().Inject(ctx, carrier)

	if carrier.Get(headerTraceParent) != "" {
		return
	}

	spanContext := trace.SpanContextFromContext(ctx)
	if !spanContext.IsValid() {
		return
	}
	flags := spanContext.TraceFlags() & (trace.FlagsSampled | trace.FlagsRandom)
	carrier.Set(headerTraceParent, fmt.Sprintf("00-%s-%s-%02x", spanContext.TraceID(), spanContext.SpanID(), byte(flags)))
	if traceState := spanContext.TraceState().String(); traceState != "" {
		carrier.Set(headerTraceState, traceState)
	}
}

func headerTraceContextPresent(header nats.Header) bool {
	return header != nil && header.Get(headerTraceParent) != ""
}

func propagationFallbackExtract(ctx context.Context, carrier natsHeaderCarrier) context.Context {
	if carrier.Get(headerTraceParent) == "" {
		return ctx
	}
	return propagationTraceContextExtract(ctx, carrier)
}

func propagationTraceContextExtract(ctx context.Context, carrier natsHeaderCarrier) context.Context {
	return propagation.TraceContext{}.Extract(ctx, carrier)
}

type natsHeaderCarrier struct {
	header *nats.Header
}

func newNATSHeaderCarrier(header *nats.Header) natsHeaderCarrier {
	if header != nil && *header == nil {
		*header = nats.Header{}
	}
	return natsHeaderCarrier{header: header}
}

func (carrier natsHeaderCarrier) Get(key string) string {
	if carrier.header == nil || *carrier.header == nil {
		return ""
	}
	return (*carrier.header).Get(key)
}

func (carrier natsHeaderCarrier) Set(key, value string) {
	if carrier.header == nil {
		return
	}
	if *carrier.header == nil {
		*carrier.header = nats.Header{}
	}
	(*carrier.header).Set(key, value)
}

func (carrier natsHeaderCarrier) Keys() []string {
	if carrier.header == nil || *carrier.header == nil {
		return nil
	}
	keys := make([]string, 0, len(*carrier.header))
	for key := range *carrier.header {
		keys = append(keys, key)
	}
	return keys
}
