package natsbus

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/evalops/service-runtime/resilience"
	"github.com/google/uuid"
	"github.com/nats-io/nats.go"
	"github.com/prometheus/client_golang/prometheus"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/trace"
)

const (
	defaultReplayInterval  = 30 * time.Second
	defaultReplayBatchSize = 10
)

var errDeadLetterDirRequired = errors.New("dead_letter_dir_required")

// ReliableOptions configures retry, circuit breaking, and dead-letter replay
// for a ReliablePublisher.
type ReliableOptions struct {
	PublisherOptions Options
	Retry            resilience.RetryConfig
	Breaker          resilience.BreakerConfig
	Registerer       prometheus.Registerer
	DeadLetterDir    string
	ReplayInterval   time.Duration
	ReplayBatchSize  int
	Clock            func() time.Time
}

func (opts ReliableOptions) withDefaults() ReliableOptions {
	if opts.Registerer == nil {
		opts.Registerer = prometheus.DefaultRegisterer
	}
	if opts.ReplayInterval <= 0 {
		opts.ReplayInterval = defaultReplayInterval
	}
	if opts.ReplayBatchSize < 1 {
		opts.ReplayBatchSize = defaultReplayBatchSize
	}
	if opts.Clock == nil {
		opts.Clock = time.Now
	}
	return opts
}

// ReliablePublisher wraps Publisher with retry, circuit breaking, durable
// dead-letter persistence, and background replay.
type ReliablePublisher struct {
	publisher       *Publisher
	retryCfg        resilience.RetryConfig
	breaker         *resilience.Breaker
	deadLetters     *deadLetterStore
	metrics         *reliableMetrics
	logger          *slog.Logger
	replayInterval  time.Duration
	replayBatchSize int
	clock           func() time.Time

	replayMu sync.Mutex
	cancel   func()
	wg       sync.WaitGroup
}

// ConnectReliable connects to NATS and returns a ReliablePublisher using the
// provided dead-letter directory and replay settings.
func ConnectReliable(ctx context.Context, natsURL, streamName, subjectPrefix string, opts ReliableOptions) (*ReliablePublisher, error) {
	opts = opts.withDefaults()
	if strings.TrimSpace(opts.DeadLetterDir) == "" {
		return nil, errDeadLetterDirRequired
	}

	publisher, err := ConnectWithOptions(ctx, natsURL, streamName, subjectPrefix, opts.PublisherOptions)
	if err != nil {
		return nil, err
	}

	reliable, err := NewReliablePublisher(publisher, opts)
	if err != nil {
		publisher.Close()
		return nil, err
	}
	return reliable, nil
}

// NewReliablePublisher wraps an existing Publisher with retry, dead-lettering,
// and replay support.
func NewReliablePublisher(publisher *Publisher, opts ReliableOptions) (*ReliablePublisher, error) {
	opts = opts.withDefaults()
	if strings.TrimSpace(opts.DeadLetterDir) == "" {
		return nil, errDeadLetterDirRequired
	}
	if publisher == nil || publisher.js == nil {
		return nil, ErrPublisherNil
	}

	store, err := newDeadLetterStore(opts.DeadLetterDir, opts.Clock)
	if err != nil {
		return nil, err
	}
	metrics, err := newReliableMetrics(opts.Registerer)
	if err != nil {
		return nil, err
	}
	metrics.setDeadLetterSize(store.Count())

	reliable := &ReliablePublisher{
		publisher:       publisher,
		retryCfg:        opts.Retry,
		breaker:         resilience.NewBreaker(opts.Breaker),
		deadLetters:     store,
		metrics:         metrics,
		logger:          publisher.loggerOrDefault(),
		replayInterval:  opts.ReplayInterval,
		replayBatchSize: opts.ReplayBatchSize,
		clock:           opts.Clock,
	}

	replayCtx, cancel := context.WithCancel(context.Background())
	reliable.cancel = cancel
	reliable.wg.Add(1)
	go reliable.replayLoop(replayCtx)

	return reliable, nil
}

// Close stops background replay and closes the underlying publisher.
func (publisher *ReliablePublisher) Close() {
	if publisher == nil {
		return
	}
	if publisher.cancel != nil {
		publisher.cancel()
	}
	publisher.wg.Wait()
	if publisher.publisher != nil {
		publisher.publisher.Close()
	}
}

// Publish encodes, retries, and publishes change. On final failure it persists
// the exact NATS message to a dead-letter queue for replay.
func (publisher *ReliablePublisher) Publish(ctx context.Context, change Change) error {
	if publisher == nil || publisher.publisher == nil || publisher.publisher.js == nil {
		return ErrPublisherNil
	}

	subject := change.subject(publisher.publisher.subjectPrefix)
	ctx, span := otel.Tracer(propagationTracerName).Start(
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

	if err := publisher.publishMessage(ctx, message); err != nil {
		recordSpanError(span, err)
		return publisher.handleDeadLetter(message, err)
	}

	publisher.logger.Debug("published reliable change event", "seq", change.Sequence, "subject", message.Subject)
	return nil
}

// PublishChange preserves the ChangePublisher behaviour and logs final errors.
func (publisher *ReliablePublisher) PublishChange(ctx context.Context, change Change) {
	if publisher == nil || publisher.publisher == nil || publisher.publisher.js == nil {
		return
	}
	if err := publisher.Publish(ctx, change); err != nil {
		publisher.logger.Error("failed to publish reliable change event", "error", err, "seq", change.Sequence)
	}
}

// ReplayPending attempts a single replay batch immediately.
func (publisher *ReliablePublisher) ReplayPending(ctx context.Context) error {
	if publisher == nil {
		return ErrPublisherNil
	}
	return publisher.replayOnce(ctx)
}

func (publisher *ReliablePublisher) replayLoop(ctx context.Context) {
	defer publisher.wg.Done()

	ticker := time.NewTicker(publisher.replayInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := publisher.replayOnce(ctx); err != nil && !errors.Is(err, ErrPublisherNil) && !errors.Is(err, context.Canceled) {
				publisher.logger.Warn("natsbus dead-letter replay failed", "error", err)
			}
		}
	}
}

func (publisher *ReliablePublisher) replayOnce(ctx context.Context) error {
	publisher.replayMu.Lock()
	defer publisher.replayMu.Unlock()

	records, err := publisher.deadLetters.List(publisher.replayBatchSize)
	if err != nil {
		return err
	}

	for _, record := range records {
		if err := publisher.publishMessage(ctx, record.message()); err != nil {
			return fmt.Errorf("replay dead letter %s: %w", filepath.Base(record.path), err)
		}
		if err := publisher.deadLetters.Delete(record.path); err != nil {
			return err
		}
		publisher.metrics.setDeadLetterSize(publisher.deadLetters.Count())
		publisher.logger.Debug("replayed natsbus dead letter", "path", record.path, "subject", record.Subject)
	}
	return nil
}

func (publisher *ReliablePublisher) publishMessage(ctx context.Context, message *nats.Msg) error {
	return publisher.breaker.Do(ctx, func(ctx context.Context) error {
		return resilience.Retry(ctx, publisher.retryCfg, func(ctx context.Context) error {
			_, err := publisher.publisher.js.PublishMsg(ctx, cloneMessage(message))
			return err
		})
	})
}

func (publisher *ReliablePublisher) handleDeadLetter(message *nats.Msg, publishErr error) error {
	publisher.metrics.recordPublishFailure()
	record := deadLetterRecord{
		ID:        uuid.NewString(),
		CreatedAt: publisher.clock().UTC(),
		Subject:   message.Subject,
		Header:    cloneMessageHeader(message.Header),
		Data:      append([]byte(nil), message.Data...),
		LastError: publishErr.Error(),
	}
	if err := publisher.deadLetters.Save(record); err != nil {
		return fmt.Errorf("publish %s failed: %w; persist dead letter: %v", message.Subject, publishErr, err)
	}
	publisher.metrics.setDeadLetterSize(publisher.deadLetters.Count())
	return fmt.Errorf("publish %s failed after retries and was dead-lettered: %w", message.Subject, publishErr)
}

type reliableMetrics struct {
	publishFailuresTotal prometheus.Counter
	deadLetterSize       prometheus.Gauge
}

func newReliableMetrics(registerer prometheus.Registerer) (*reliableMetrics, error) {
	failures, err := registerCounter(
		registerer,
		prometheus.NewCounter(prometheus.CounterOpts{
			Name: "natsbus_publish_failures_total",
			Help: "Count of natsbus publish operations that exhausted retries and required dead-letter handling.",
		}),
	)
	if err != nil {
		return nil, err
	}
	deadLetterSize, err := registerGauge(
		registerer,
		prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "natsbus_dead_letter_size",
			Help: "Count of natsbus dead-letter messages queued for replay.",
		}),
	)
	if err != nil {
		return nil, err
	}
	return &reliableMetrics{
		publishFailuresTotal: failures,
		deadLetterSize:       deadLetterSize,
	}, nil
}

func (metrics *reliableMetrics) recordPublishFailure() {
	if metrics != nil && metrics.publishFailuresTotal != nil {
		metrics.publishFailuresTotal.Inc()
	}
}

func (metrics *reliableMetrics) setDeadLetterSize(size int64) {
	if metrics != nil && metrics.deadLetterSize != nil {
		metrics.deadLetterSize.Set(float64(size))
	}
}

func registerCounter(registerer prometheus.Registerer, collector prometheus.Counter) (prometheus.Counter, error) {
	if err := registerer.Register(collector); err != nil {
		alreadyRegistered, ok := err.(prometheus.AlreadyRegisteredError)
		if !ok {
			return nil, err
		}
		existing, ok := alreadyRegistered.ExistingCollector.(prometheus.Counter)
		if !ok {
			return nil, err
		}
		return existing, nil
	}
	return collector, nil
}

func registerGauge(registerer prometheus.Registerer, collector prometheus.Gauge) (prometheus.Gauge, error) {
	if err := registerer.Register(collector); err != nil {
		alreadyRegistered, ok := err.(prometheus.AlreadyRegisteredError)
		if !ok {
			return nil, err
		}
		existing, ok := alreadyRegistered.ExistingCollector.(prometheus.Gauge)
		if !ok {
			return nil, err
		}
		return existing, nil
	}
	return collector, nil
}

type deadLetterStore struct {
	baseDir string
	clock   func() time.Time
	count   atomic.Int64
}

type deadLetterRecord struct {
	ID        string      `json:"id"`
	CreatedAt time.Time   `json:"created_at"`
	Subject   string      `json:"subject"`
	Header    nats.Header `json:"header,omitempty"`
	Data      []byte      `json:"data,omitempty"`
	LastError string      `json:"last_error,omitempty"`
}

type storedDeadLetter struct {
	deadLetterRecord
	path string
}

func newDeadLetterStore(baseDir string, clock func() time.Time) (*deadLetterStore, error) {
	trimmed := strings.TrimSpace(baseDir)
	if trimmed == "" {
		return nil, errDeadLetterDirRequired
	}
	if err := os.MkdirAll(trimmed, 0o750); err != nil {
		return nil, fmt.Errorf("create dead-letter dir: %w", err)
	}
	store := &deadLetterStore{baseDir: trimmed, clock: clock}
	count, err := store.scanCount()
	if err != nil {
		return nil, err
	}
	store.count.Store(count)
	return store, nil
}

func (store *deadLetterStore) Save(record deadLetterRecord) error {
	if record.CreatedAt.IsZero() {
		record.CreatedAt = store.clock().UTC()
	}
	if strings.TrimSpace(record.ID) == "" {
		record.ID = uuid.NewString()
	}

	payload, err := json.Marshal(record)
	if err != nil {
		return fmt.Errorf("marshal dead letter: %w", err)
	}

	tmp, err := os.CreateTemp(store.baseDir, "natsbus-dead-letter-*.tmp")
	if err != nil {
		return fmt.Errorf("create dead-letter temp file: %w", err)
	}
	tmpName := tmp.Name()
	defer func() {
		_ = os.Remove(tmpName)
	}()

	if _, err := tmp.Write(payload); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write dead-letter temp file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close dead-letter temp file: %w", err)
	}

	name := fmt.Sprintf("%s-%s.json", record.CreatedAt.UTC().Format("20060102T150405.000000000Z0700"), record.ID)
	path := filepath.Join(store.baseDir, name)
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("finalize dead-letter file: %w", err)
	}
	store.count.Add(1)
	return nil
}

func (store *deadLetterStore) List(limit int) ([]storedDeadLetter, error) {
	entries, err := os.ReadDir(store.baseDir)
	if err != nil {
		return nil, fmt.Errorf("list dead letters: %w", err)
	}
	sort.Slice(entries, func(i, j int) bool {
		return entries[i].Name() < entries[j].Name()
	})

	records := make([]storedDeadLetter, 0, limit)
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		path := filepath.Join(store.baseDir, entry.Name())
		//nolint:gosec // Path comes from os.ReadDir(store.baseDir), filtered to files within the configured dead-letter directory.
		// #nosec G304 -- Path stays scoped to the configured dead-letter directory contents.
		payload, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("read dead letter %s: %w", path, err)
		}
		var record deadLetterRecord
		if err := json.Unmarshal(payload, &record); err != nil {
			return nil, fmt.Errorf("decode dead letter %s: %w", path, err)
		}
		records = append(records, storedDeadLetter{
			deadLetterRecord: record,
			path:             path,
		})
		if len(records) >= limit {
			break
		}
	}
	return records, nil
}

func (store *deadLetterStore) Delete(path string) error {
	if err := os.Remove(path); err != nil {
		return fmt.Errorf("remove dead letter %s: %w", path, err)
	}
	store.count.Add(-1)
	return nil
}

func (store *deadLetterStore) Count() int64 {
	return store.count.Load()
}

func (store *deadLetterStore) scanCount() (int64, error) {
	entries, err := os.ReadDir(store.baseDir)
	if err != nil {
		return 0, fmt.Errorf("scan dead-letter dir: %w", err)
	}
	var count int64
	for _, entry := range entries {
		if !entry.IsDir() && strings.HasSuffix(entry.Name(), ".json") {
			count++
		}
	}
	return count, nil
}

func (record storedDeadLetter) message() *nats.Msg {
	return &nats.Msg{
		Subject: record.Subject,
		Header:  cloneMessageHeader(record.Header),
		Data:    append([]byte(nil), record.Data...),
	}
}

func cloneMessage(message *nats.Msg) *nats.Msg {
	if message == nil {
		return nil
	}
	return &nats.Msg{
		Subject: message.Subject,
		Reply:   message.Reply,
		Header:  cloneMessageHeader(message.Header),
		Data:    append([]byte(nil), message.Data...),
	}
}

func cloneMessageHeader(header nats.Header) nats.Header {
	if header == nil {
		return nil
	}
	cloned := make(nats.Header, len(header))
	for key, values := range header {
		cloned[key] = append([]string(nil), values...)
	}
	return cloned
}

var _ ChangePublisher = (*ReliablePublisher)(nil)
var _ interface{ Close() } = (*ReliablePublisher)(nil)
var _ interface {
	Publish(context.Context, Change) error
} = (*ReliablePublisher)(nil)
