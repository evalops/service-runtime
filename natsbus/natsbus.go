package natsbus

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	natsbusv1 "github.com/evalops/service-runtime/gen/proto/go/evalops/runtime/natsbus/v1"
	"github.com/google/uuid"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/anypb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

const (
	DefaultMaxAge = 7 * 24 * time.Hour
)

type WireFormat string

const (
	WireFormatJSON         WireFormat = "json"
	WireFormatProto        WireFormat = "proto"
	WireFormatProtoHeaders WireFormat = "proto-headers"
)

var (
	errNATSURLRequired       = errors.New("nats_url_required")
	errStreamNameRequired    = errors.New("stream_name_required")
	errSubjectPrefixRequired = errors.New("subject_prefix_required")
	errPayloadMessageNil     = errors.New("payload_message_nil")
	errPayloadTargetNil      = errors.New("payload_target_nil")
	errEnvelopeEmpty         = errors.New("envelope_empty")
	errWireFormatInvalid     = errors.New("wire_format_invalid")
	errMessageNil            = errors.New("message_nil")
)

const (
	headerSpecVersion     = "ce-specversion"
	headerID              = "ce-id"
	headerType            = "ce-type"
	headerSource          = "ce-source"
	headerTime            = "ce-time"
	headerDataContentType = "content-type"
	headerTenantID        = "ce-tenantid"
)

type Change struct {
	Sequence         int64      `json:"sequence"`
	TenantID         string     `json:"tenant_id"`
	AggregateType    string     `json:"aggregate_type"`
	AggregateID      string     `json:"aggregate_id,omitempty"`
	Operation        string     `json:"operation"`
	AggregateVersion int64      `json:"aggregate_version,omitempty"`
	RecordedAt       time.Time  `json:"recorded_at"`
	Payload          *anypb.Any `json:"payload,omitempty"`
}

type CloudEvent struct {
	SpecVersion     string          `json:"specversion"`
	ID              string          `json:"id"`
	Type            string          `json:"type"`
	Source          string          `json:"source"`
	Time            string          `json:"time"`
	DataContentType string          `json:"datacontenttype"`
	TenantID        string          `json:"tenant_id"`
	Data            json.RawMessage `json:"data"`
}

type Envelope struct {
	SpecVersion     string
	ID              string
	Type            string
	Source          string
	Time            time.Time
	DataContentType string
	TenantID        string
	Payload         *anypb.Any
	WireFormat      WireFormat
}

type Options struct {
	Logger      *slog.Logger
	Retention   jetstream.RetentionPolicy
	MaxAge      time.Duration
	Storage     jetstream.StorageType
	NATSOptions []nats.Option
	WireFormat  WireFormat
}

type ChangePublisher interface {
	PublishChange(ctx context.Context, change Change)
}

type Publisher struct {
	js            jetStreamClient
	logger        *slog.Logger
	subjectPrefix string
	source        string
	wireFormat    WireFormat
	closeFunc     func()
}

type NoopPublisher struct{}

type jetStreamClient interface {
	CreateOrUpdateStream(ctx context.Context, cfg jetstream.StreamConfig) (jetstream.Stream, error)
	PublishMsg(ctx context.Context, msg *nats.Msg, opts ...jetstream.PublishOpt) (*jetstream.PubAck, error)
}

var connectNATS = nats.Connect

var newJetStream = func(connection *nats.Conn) (jetStreamClient, error) {
	return jetstream.New(connection)
}

func Connect(ctx context.Context, natsURL, streamName, subjectPrefix string, logger *slog.Logger) (*Publisher, error) {
	return ConnectWithOptions(ctx, natsURL, streamName, subjectPrefix, Options{Logger: logger})
}

func ConnectWithOptions(ctx context.Context, natsURL, streamName, subjectPrefix string, opts Options) (*Publisher, error) {
	opts = opts.withDefaults()
	natsURL = strings.TrimSpace(natsURL)
	streamName = strings.TrimSpace(streamName)
	subjectPrefix = strings.TrimSuffix(strings.TrimSpace(subjectPrefix), ".")

	switch {
	case natsURL == "":
		return nil, errNATSURLRequired
	case streamName == "":
		return nil, errStreamNameRequired
	case subjectPrefix == "":
		return nil, errSubjectPrefixRequired
	}

	connection, err := connectNATS(natsURL, opts.natsOptions()...)
	if err != nil {
		return nil, fmt.Errorf("nats_connect: %w", err)
	}

	js, err := newJetStream(connection)
	if err != nil {
		connection.Close()
		return nil, fmt.Errorf("jetstream_connect: %w", err)
	}

	if _, err := js.CreateOrUpdateStream(ctx, jetstream.StreamConfig{
		Name:      streamName,
		Subjects:  []string{subjectPrefix + ".>"},
		Retention: opts.Retention,
		MaxAge:    opts.MaxAge,
		Storage:   opts.Storage,
	}); err != nil {
		connection.Close()
		return nil, fmt.Errorf("create_stream: %w", err)
	}

	return &Publisher{
		js:            js,
		logger:        opts.Logger,
		subjectPrefix: subjectPrefix,
		source:        eventSource(subjectPrefix),
		wireFormat:    opts.WireFormat,
		closeFunc:     connection.Close,
	}, nil
}

func (publisher *Publisher) Close() {
	if publisher != nil && publisher.closeFunc != nil {
		publisher.closeFunc()
	}
}

func (publisher *Publisher) PublishChange(ctx context.Context, change Change) {
	if publisher == nil || publisher.js == nil {
		return
	}

	subject := change.subject(publisher.subjectPrefix)
	envelope, err := change.toCloudEvent(subject, publisher.source)
	if err != nil {
		publisher.loggerOrDefault().Error("failed to build change envelope", "error", err, "seq", change.Sequence)
		return
	}

	message, err := marshalEnvelopeMessage(subject, envelope, normalizedWireFormat(publisher.wireFormat))
	if err != nil {
		publisher.loggerOrDefault().Error("failed to marshal change event", "error", err, "seq", change.Sequence)
		return
	}

	if _, err := publisher.js.PublishMsg(ctx, message); err != nil {
		publisher.loggerOrDefault().Error("failed to publish change event", "error", err, "seq", change.Sequence, "subject", subject)
		return
	}

	publisher.loggerOrDefault().Debug("published change event", "seq", change.Sequence, "subject", subject)
}

func (NoopPublisher) PublishChange(context.Context, Change) {}

func (opts Options) withDefaults() Options {
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	if opts.MaxAge <= 0 {
		opts.MaxAge = DefaultMaxAge
	}
	if opts.Retention == 0 {
		opts.Retention = jetstream.LimitsPolicy
	}
	if opts.Storage == 0 {
		opts.Storage = jetstream.FileStorage
	}
	if opts.WireFormat == "" {
		opts.WireFormat = WireFormatJSON
	}
	return opts
}

func (opts Options) natsOptions() []nats.Option {
	options := []nats.Option{
		nats.RetryOnFailedConnect(true),
		nats.MaxReconnects(10),
		nats.ReconnectWait(2 * time.Second),
	}
	options = append(options, opts.NATSOptions...)
	return options
}

func NewPayload(message proto.Message) (*anypb.Any, error) {
	if message == nil {
		return nil, errPayloadMessageNil
	}
	return anypb.New(message)
}

func MustPayload(message proto.Message) *anypb.Any {
	payload, err := NewPayload(message)
	if err != nil {
		panic(err)
	}
	return payload
}

func UnmarshalPayload(payload *anypb.Any, target proto.Message) error {
	if target == nil {
		return errPayloadTargetNil
	}
	if payload == nil {
		return nil
	}
	return payload.UnmarshalTo(target)
}

func (change Change) subject(prefix string) string {
	return fmt.Sprintf("%s.%s.%s", strings.TrimSuffix(prefix, "."), change.AggregateType, change.Operation)
}

func (change Change) toCloudEvent(subject, source string) (Envelope, error) {
	recordedAt := change.RecordedAt.UTC()
	if recordedAt.IsZero() {
		recordedAt = time.Now().UTC()
	}

	return Envelope{
		SpecVersion:     "1.0",
		ID:              uuid.NewString(),
		Type:            subject,
		Source:          source,
		Time:            recordedAt,
		DataContentType: "application/protobuf",
		TenantID:        change.TenantID,
		Payload:         change.Payload,
	}, nil
}

func (publisher *Publisher) loggerOrDefault() *slog.Logger {
	if publisher != nil && publisher.logger != nil {
		return publisher.logger
	}
	return slog.Default()
}

func eventSource(subjectPrefix string) string {
	trimmed := strings.TrimSuffix(strings.TrimSpace(subjectPrefix), ".")
	if strings.HasSuffix(trimmed, ".changes") {
		return strings.TrimSuffix(trimmed, ".changes")
	}
	return trimmed
}

func marshalPayloadJSON(payload *anypb.Any) (json.RawMessage, error) {
	if payload == nil {
		return json.RawMessage("null"), nil
	}

	data, err := protojson.MarshalOptions{UseProtoNames: true}.Marshal(payload)
	if err != nil {
		return nil, err
	}
	return json.RawMessage(data), nil
}

func marshalEnvelopeMessage(subject string, envelope Envelope, wireFormat WireFormat) (*nats.Msg, error) {
	switch wireFormat {
	case WireFormatJSON, WireFormatProto:
		payload, err := marshalEnvelope(envelope, wireFormat)
		if err != nil {
			return nil, err
		}
		return &nats.Msg{Subject: subject, Data: payload}, nil
	case WireFormatProtoHeaders:
		return marshalEnvelopeProtoHeaders(subject, envelope)
	default:
		return nil, errWireFormatInvalid
	}
}

func marshalEnvelope(envelope Envelope, wireFormat WireFormat) ([]byte, error) {
	switch wireFormat {
	case WireFormatJSON:
		return marshalEnvelopeJSON(envelope)
	case WireFormatProto:
		return marshalEnvelopeProto(envelope)
	default:
		return nil, errWireFormatInvalid
	}
}

func marshalEnvelopeProtoHeaders(subject string, envelope Envelope) (*nats.Msg, error) {
	message := &nats.Msg{
		Subject: subject,
		Header:  nats.Header{},
	}
	message.Header.Set(headerSpecVersion, envelope.SpecVersion)
	message.Header.Set(headerID, envelope.ID)
	message.Header.Set(headerType, envelope.Type)
	message.Header.Set(headerSource, envelope.Source)
	if !envelope.Time.IsZero() {
		message.Header.Set(headerTime, envelope.Time.UTC().Format(time.RFC3339))
	}
	if envelope.TenantID != "" {
		message.Header.Set(headerTenantID, envelope.TenantID)
	}

	dataContentType := strings.TrimSpace(envelope.DataContentType)
	if dataContentType == "" {
		dataContentType = "application/protobuf"
	}
	message.Header.Set(headerDataContentType, dataContentType)

	if envelope.Payload == nil {
		return message, nil
	}

	payload, err := proto.Marshal(envelope.Payload)
	if err != nil {
		return nil, err
	}
	message.Data = payload
	return message, nil
}

func marshalEnvelopeJSON(envelope Envelope) ([]byte, error) {
	data, err := marshalPayloadJSON(envelope.Payload)
	if err != nil {
		return nil, err
	}
	return json.Marshal(CloudEvent{
		SpecVersion:     envelope.SpecVersion,
		ID:              envelope.ID,
		Type:            envelope.Type,
		Source:          envelope.Source,
		Time:            envelope.Time.UTC().Format(time.RFC3339),
		DataContentType: "application/json",
		TenantID:        envelope.TenantID,
		Data:            data,
	})
}

func marshalEnvelopeProto(envelope Envelope) ([]byte, error) {
	message := &natsbusv1.EventEnvelope{
		SpecVersion:     envelope.SpecVersion,
		Id:              envelope.ID,
		Type:            envelope.Type,
		Source:          envelope.Source,
		Time:            timestamppb.New(envelope.Time.UTC()),
		DataContentType: "application/protobuf",
		TenantId:        envelope.TenantID,
		Data:            envelope.Payload,
	}
	return proto.Marshal(message)
}

func UnmarshalEnvelope(data []byte) (Envelope, error) {
	if len(data) == 0 {
		return Envelope{}, errEnvelopeEmpty
	}

	trimmed, isTrimmed := trimLeadingJSONWhitespace(data)
	if len(trimmed) == 0 {
		return Envelope{}, errEnvelopeEmpty
	}

	switch detectWireFormat(trimmed) {
	case WireFormatJSON:
		envelope, err := unmarshalEnvelopeJSON(trimmed)
		if err == nil {
			return envelope, nil
		}
		if isTrimmed {
			protoEnvelope, protoErr := unmarshalEnvelopeProto(data)
			if protoErr == nil {
				return protoEnvelope, nil
			}
		}
		return Envelope{}, err
	case WireFormatProto:
		return unmarshalEnvelopeProto(data)
	default:
		return Envelope{}, errWireFormatInvalid
	}
}

func UnmarshalMessage(message *nats.Msg) (Envelope, error) {
	if message == nil {
		return Envelope{}, errMessageNil
	}
	if hasCloudEventHeaders(message.Header) {
		return unmarshalEnvelopeProtoHeaders(message)
	}
	return UnmarshalEnvelope(message.Data)
}

func unmarshalEnvelopeJSON(data []byte) (Envelope, error) {
	var event CloudEvent
	if err := json.Unmarshal(data, &event); err != nil {
		return Envelope{}, err
	}
	recordedAt, err := time.Parse(time.RFC3339, event.Time)
	if err != nil {
		return Envelope{}, err
	}
	payload, err := unmarshalPayloadJSON(event.Data)
	if err != nil {
		return Envelope{}, err
	}
	return Envelope{
		SpecVersion:     event.SpecVersion,
		ID:              event.ID,
		Type:            event.Type,
		Source:          event.Source,
		Time:            recordedAt.UTC(),
		DataContentType: event.DataContentType,
		TenantID:        event.TenantID,
		Payload:         payload,
		WireFormat:      WireFormatJSON,
	}, nil
}

func unmarshalEnvelopeProto(data []byte) (Envelope, error) {
	message := &natsbusv1.EventEnvelope{}
	if err := proto.Unmarshal(data, message); err != nil {
		return Envelope{}, err
	}
	recordedAt := time.Time{}
	if message.Time != nil {
		recordedAt = message.Time.AsTime().UTC()
	}
	return Envelope{
		SpecVersion:     message.SpecVersion,
		ID:              message.Id,
		Type:            message.Type,
		Source:          message.Source,
		Time:            recordedAt,
		DataContentType: message.DataContentType,
		TenantID:        message.TenantId,
		Payload:         message.Data,
		WireFormat:      WireFormatProto,
	}, nil
}

func unmarshalEnvelopeProtoHeaders(message *nats.Msg) (Envelope, error) {
	recordedAt := time.Time{}
	if rawTime := strings.TrimSpace(message.Header.Get(headerTime)); rawTime != "" {
		parsedTime, err := time.Parse(time.RFC3339, rawTime)
		if err != nil {
			return Envelope{}, err
		}
		recordedAt = parsedTime.UTC()
	}

	payload, err := unmarshalHeaderPayload(message.Header.Get(headerDataContentType), message.Data)
	if err != nil {
		return Envelope{}, err
	}

	return Envelope{
		SpecVersion:     strings.TrimSpace(message.Header.Get(headerSpecVersion)),
		ID:              strings.TrimSpace(message.Header.Get(headerID)),
		Type:            strings.TrimSpace(message.Header.Get(headerType)),
		Source:          strings.TrimSpace(message.Header.Get(headerSource)),
		Time:            recordedAt,
		DataContentType: strings.TrimSpace(message.Header.Get(headerDataContentType)),
		TenantID:        strings.TrimSpace(message.Header.Get(headerTenantID)),
		Payload:         payload,
		WireFormat:      WireFormatProtoHeaders,
	}, nil
}

func unmarshalPayloadJSON(data json.RawMessage) (*anypb.Any, error) {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return nil, nil
	}
	payload := &anypb.Any{}
	if err := protojson.Unmarshal(trimmed, payload); err != nil {
		return nil, err
	}
	return payload, nil
}

func unmarshalHeaderPayload(contentType string, data []byte) (*anypb.Any, error) {
	if len(bytes.TrimSpace(data)) == 0 {
		return nil, nil
	}

	if strings.Contains(strings.ToLower(contentType), "application/json") {
		return unmarshalPayloadJSON(json.RawMessage(data))
	}

	payload := &anypb.Any{}
	if err := proto.Unmarshal(data, payload); err != nil {
		return nil, err
	}
	return payload, nil
}

func detectWireFormat(data []byte) WireFormat {
	if len(data) == 0 {
		return ""
	}
	if data[0] == '{' {
		return WireFormatJSON
	}
	return WireFormatProto
}

func trimLeadingJSONWhitespace(data []byte) ([]byte, bool) {
	for index, value := range data {
		switch value {
		case ' ', '\n', '\r', '\t':
			continue
		default:
			return data[index:], index > 0
		}
	}
	return nil, false
}

func normalizedWireFormat(wireFormat WireFormat) WireFormat {
	switch wireFormat {
	case "", WireFormatJSON:
		return WireFormatJSON
	case WireFormatProto:
		return WireFormatProto
	case WireFormatProtoHeaders:
		return WireFormatProtoHeaders
	default:
		return ""
	}
}

func hasCloudEventHeaders(header nats.Header) bool {
	if len(header) == 0 {
		return false
	}
	return strings.TrimSpace(header.Get(headerSpecVersion)) != "" &&
		strings.TrimSpace(header.Get(headerID)) != "" &&
		strings.TrimSpace(header.Get(headerType)) != "" &&
		strings.TrimSpace(header.Get(headerSource)) != ""
}
