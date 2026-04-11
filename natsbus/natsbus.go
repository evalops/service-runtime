package natsbus

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

const (
	DefaultMaxAge = 7 * 24 * time.Hour
)

var (
	errNATSURLRequired       = errors.New("nats_url_required")
	errStreamNameRequired    = errors.New("stream_name_required")
	errSubjectPrefixRequired = errors.New("subject_prefix_required")
)

type Change struct {
	Sequence         int64           `json:"sequence"`
	TenantID         string          `json:"tenant_id"`
	AggregateType    string          `json:"aggregate_type"`
	AggregateID      string          `json:"aggregate_id,omitempty"`
	Operation        string          `json:"operation"`
	AggregateVersion int64           `json:"aggregate_version,omitempty"`
	RecordedAt       time.Time       `json:"recorded_at"`
	Payload          json.RawMessage `json:"payload"`
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

type Options struct {
	Logger      *slog.Logger
	Retention   jetstream.RetentionPolicy
	MaxAge      time.Duration
	Storage     jetstream.StorageType
	NATSOptions []nats.Option
}

type ChangePublisher interface {
	PublishChange(ctx context.Context, change Change)
}

type Publisher struct {
	js            jetStreamClient
	logger        *slog.Logger
	subjectPrefix string
	source        string
	closeFunc     func()
}

type NoopPublisher struct{}

type jetStreamClient interface {
	CreateOrUpdateStream(ctx context.Context, cfg jetstream.StreamConfig) (jetstream.Stream, error)
	Publish(ctx context.Context, subject string, data []byte, opts ...jetstream.PublishOpt) (*jetstream.PubAck, error)
}

var connectNATS = func(url string, options ...nats.Option) (*nats.Conn, error) {
	return nats.Connect(url, options...)
}

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
	envelope := change.toCloudEvent(subject, publisher.source)

	payload, err := json.Marshal(envelope)
	if err != nil {
		publisher.loggerOrDefault().Error("failed to marshal change event", "error", err, "seq", change.Sequence)
		return
	}

	if _, err := publisher.js.Publish(ctx, subject, payload); err != nil {
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

func (change Change) subject(prefix string) string {
	return fmt.Sprintf("%s.%s.%s", strings.TrimSuffix(prefix, "."), change.AggregateType, change.Operation)
}

func (change Change) toCloudEvent(subject, source string) CloudEvent {
	recordedAt := change.RecordedAt.UTC()
	if recordedAt.IsZero() {
		recordedAt = time.Now().UTC()
	}

	return CloudEvent{
		SpecVersion:     "1.0",
		ID:              uuid.NewString(),
		Type:            subject,
		Source:          source,
		Time:            recordedAt.Format(time.RFC3339),
		DataContentType: "application/json",
		TenantID:        change.TenantID,
		Data:            change.Payload,
	}
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
