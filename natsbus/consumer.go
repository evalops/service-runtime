package natsbus

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/nats-io/nats.go"
)

// DeliveryPolicy controls where a consumer starts reading messages.
type DeliveryPolicy string

const (
	// DeliverNew starts the consumer at newly published messages.
	DeliverNew DeliveryPolicy = "new"
	// DeliverAll starts the consumer at the beginning of the stream.
	DeliverAll DeliveryPolicy = "all"
	// DeliverLast starts the consumer at the most recent message.
	DeliverLast DeliveryPolicy = "last"
)

var (
	errConsumerSubjectRequired = errors.New("consumer_subject_required")
	errConsumerStreamRequired  = errors.New("consumer_stream_required")
	errConsumerDurableRequired = errors.New("consumer_durable_required")
	errConsumerHandlerNil      = errors.New("consumer_handler_nil")
)

// Consumer handles a NATS message inside a subscription callback.
type Consumer func(context.Context, *nats.Msg) error

// ConsumerOptions configures a shared JetStream queue consumer.
type ConsumerOptions struct {
	Logger        *slog.Logger
	Stream        string
	Subject       string
	Durable       string
	Queue         string
	AckWait       time.Duration
	DeliverPolicy DeliveryPolicy
	NATSOptions   []nats.Option
}

type consumerSubscription interface {
	Drain() error
	Unsubscribe() error
}

type consumerJetStream interface {
	QueueSubscribe(subject, queue string, cb nats.MsgHandler, opts ...nats.SubOpt) (consumerSubscription, error)
}

type consumerJetStreamWrapper struct {
	js nats.JetStreamContext
}

var (
	ackMessage = func(message *nats.Msg) error {
		return message.Ack()
	}
	nakMessage = func(message *nats.Msg) error {
		return message.Nak()
	}
	closeConnection = func(connection *nats.Conn) {
		connection.Close()
	}
)

func (wrapper consumerJetStreamWrapper) QueueSubscribe(subject, queue string, cb nats.MsgHandler, opts ...nats.SubOpt) (consumerSubscription, error) {
	return wrapper.js.QueueSubscribe(subject, queue, cb, opts...)
}

var newConsumerJetStream = func(connection *nats.Conn) (consumerJetStream, error) {
	js, err := connection.JetStream()
	if err != nil {
		return nil, err
	}
	return consumerJetStreamWrapper{js: js}, nil
}

// Subscribe connects a shared JetStream queue consumer and returns a shutdown function.
func Subscribe(ctx context.Context, natsURL string, opts ConsumerOptions, handler Consumer) (func(context.Context) error, error) {
	opts = opts.withDefaults()
	natsURL = strings.TrimSpace(natsURL)
	if natsURL == "" {
		return nil, errNATSURLRequired
	}
	if opts.Stream == "" {
		return nil, errConsumerStreamRequired
	}
	if opts.Subject == "" {
		return nil, errConsumerSubjectRequired
	}
	if opts.Durable == "" {
		return nil, errConsumerDurableRequired
	}
	if handler == nil {
		return nil, errConsumerHandlerNil
	}

	connection, err := connectNATS(natsURL, opts.natsOptions()...)
	if err != nil {
		return nil, fmt.Errorf("nats_connect: %w", err)
	}

	js, err := newConsumerJetStream(connection)
	if err != nil {
		closeConnection(connection)
		return nil, fmt.Errorf("jetstream_connect: %w", err)
	}

	subscription, err := js.QueueSubscribe(opts.Subject, opts.queueName(), func(message *nats.Msg) {
		if err := handler(context.Background(), message); err != nil {
			opts.loggerOrDefault().Error("consumer handler failed", "subject", opts.Subject, "durable", opts.Durable, "error", err)
			if nakErr := nakMessage(message); nakErr != nil {
				opts.loggerOrDefault().Error("consumer nak failed", "subject", opts.Subject, "durable", opts.Durable, "error", nakErr)
			}
			return
		}
		if err := ackMessage(message); err != nil {
			opts.loggerOrDefault().Error("consumer ack failed", "subject", opts.Subject, "durable", opts.Durable, "error", err)
		}
	}, opts.subscribeOptions()...)
	if err != nil {
		closeConnection(connection)
		return nil, fmt.Errorf("queue_subscribe: %w", err)
	}

	var (
		once    sync.Once
		cleanup error
		cleanFn = func() {
			if err := subscription.Drain(); err != nil && cleanup == nil {
				cleanup = err
			}
			closeConnection(connection)
		}
	)

	go func() {
		<-ctx.Done()
		once.Do(cleanFn)
	}()

	return func(context.Context) error {
		once.Do(cleanFn)
		return cleanup
	}, nil
}

func (opts ConsumerOptions) withDefaults() ConsumerOptions {
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	opts.Stream = strings.TrimSpace(opts.Stream)
	opts.Subject = strings.TrimSpace(opts.Subject)
	opts.Durable = strings.TrimSpace(opts.Durable)
	opts.Queue = strings.TrimSpace(opts.Queue)
	if opts.DeliverPolicy == "" {
		opts.DeliverPolicy = DeliverNew
	}
	return opts
}

func (opts ConsumerOptions) loggerOrDefault() *slog.Logger {
	if opts.Logger != nil {
		return opts.Logger
	}
	return slog.Default()
}

func (opts ConsumerOptions) queueName() string {
	if opts.Queue != "" {
		return opts.Queue
	}
	return opts.Durable
}

func (opts ConsumerOptions) natsOptions() []nats.Option {
	options := []nats.Option{
		nats.RetryOnFailedConnect(true),
		nats.MaxReconnects(10),
		nats.ReconnectWait(2 * time.Second),
		nats.NoCallbacksAfterClientClose(),
	}
	options = append(options, opts.NATSOptions...)
	return options
}

func (opts ConsumerOptions) subscribeOptions() []nats.SubOpt {
	options := []nats.SubOpt{
		nats.BindStream(opts.Stream),
		nats.Durable(opts.Durable),
		nats.ManualAck(),
	}
	if opts.AckWait > 0 {
		options = append(options, nats.AckWait(opts.AckWait))
	}
	switch opts.DeliverPolicy {
	case DeliverNew:
		options = append(options, nats.DeliverNew())
	case DeliverAll:
		options = append(options, nats.DeliverAll())
	case DeliverLast:
		options = append(options, nats.DeliverLast())
	default:
		options = append(options, nats.DeliverNew())
	}
	return options
}
