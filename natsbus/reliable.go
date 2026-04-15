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
	"time"

	"github.com/evalops/service-runtime/resilience"
	"github.com/google/uuid"
	"github.com/nats-io/nats.go"
	"github.com/prometheus/client_golang/prometheus"
)

const (
	// DefaultReliableReplayInterval is the default cadence for replaying queued
	// dead-letter messages after publish failures.
	DefaultReliableReplayInterval = 5 * time.Second
	// DefaultReliableReplayBatchSize is the default maximum number of dead-letter
	// messages replayed in a single pass.
	DefaultReliableReplayBatchSize = 100
	defaultReliableReplayTimeout   = 30 * time.Second
)

var errDeadLetterDirRequired = errors.New("dead_letter_dir_required")

// ReliableOptions configures a ReliablePublisher connection and replay loop.
type ReliableOptions struct {
	Publisher       Options
	DeadLetterDir   string
	ReplayInterval  time.Duration
	ReplayBatchSize int
	Retry           resilience.RetryConfig
	Breaker         resilience.BreakerConfig
	Registerer      prometheus.Registerer
}

// ReliablePublisher wraps a Publisher with retry, circuit breaking, and
// dead-letter replay for publish failures.
type ReliablePublisher struct {
	publisher       *Publisher
	logger          *slog.Logger
	deadLetters     *fileDeadLetterQueue
	breaker         *resilience.Breaker
	retryConfig     resilience.RetryConfig
	replayInterval  time.Duration
	replayBatchSize int
	metrics         *reliableMetrics
	replaySignal    chan struct{}
	stopCh          chan struct{}
	doneCh          chan struct{}
	closeOnce       sync.Once
}

// ConnectReliable connects to NATS and wraps the publisher with retry,
// circuit-breaking, and dead-letter replay.
func ConnectReliable(ctx context.Context, natsURL, streamName, subjectPrefix string, logger *slog.Logger, deadLetterDir string) (*ReliablePublisher, error) {
	return ConnectReliableWithOptions(ctx, natsURL, streamName, subjectPrefix, ReliableOptions{
		Publisher:     Options{Logger: logger},
		DeadLetterDir: deadLetterDir,
	})
}

// ConnectReliableWithOptions is like ConnectReliable but accepts a full
// ReliableOptions struct.
func ConnectReliableWithOptions(ctx context.Context, natsURL, streamName, subjectPrefix string, opts ReliableOptions) (*ReliablePublisher, error) {
	publisher, err := ConnectWithOptions(ctx, natsURL, streamName, subjectPrefix, opts.Publisher)
	if err != nil {
		return nil, err
	}

	reliable, err := newReliablePublisher(publisher, opts)
	if err != nil {
		publisher.Close()
		return nil, err
	}
	return reliable, nil
}

func newReliablePublisher(publisher *Publisher, opts ReliableOptions) (*ReliablePublisher, error) {
	if publisher == nil {
		return nil, ErrPublisherNil
	}
	opts = opts.withDefaults()

	deadLetters, err := newFileDeadLetterQueue(opts.DeadLetterDir)
	if err != nil {
		return nil, err
	}
	metrics, err := newReliableMetrics(opts.Registerer)
	if err != nil {
		return nil, fmt.Errorf("register reliable nats metrics: %w", err)
	}

	reliable := &ReliablePublisher{
		publisher:       publisher,
		logger:          publisher.loggerOrDefault(),
		deadLetters:     deadLetters,
		breaker:         resilience.NewBreaker(opts.Breaker),
		retryConfig:     opts.Retry,
		replayInterval:  opts.ReplayInterval,
		replayBatchSize: opts.ReplayBatchSize,
		metrics:         metrics,
		replaySignal:    make(chan struct{}, 1),
		stopCh:          make(chan struct{}),
		doneCh:          make(chan struct{}),
	}
	reliable.updateDeadLetterMetric()
	go reliable.replayLoop()
	return reliable, nil
}

// Close stops the replay loop and closes the underlying NATS connection.
func (publisher *ReliablePublisher) Close() {
	if publisher == nil {
		return
	}
	publisher.closeOnce.Do(func() {
		close(publisher.stopCh)
		<-publisher.doneCh
		if publisher.publisher != nil {
			publisher.publisher.Close()
		}
	})
}

// Publish attempts to publish a change event, then falls back to a local
// dead-letter queue for replay if JetStream remains unavailable.
func (publisher *ReliablePublisher) Publish(ctx context.Context, change Change) error {
	if publisher == nil || publisher.publisher == nil {
		return ErrPublisherNil
	}

	message, err := publisher.publisher.buildMessage(ctx, change)
	if err != nil {
		return err
	}
	if err := publisher.publishMessage(ctx, message); err != nil {
		publisher.metrics.observePublishFailure(publisher.publisher.subjectPrefix)
		if deadLetterErr := publisher.deadLetters.enqueue(message, err); deadLetterErr != nil {
			return errors.Join(err, fmt.Errorf("enqueue dead letter: %w", deadLetterErr))
		}
		publisher.updateDeadLetterMetric()
		publisher.signalReplay()
		return fmt.Errorf("publish %s queued for replay: %w", message.Subject, err)
	}
	return nil
}

// PublishChange is the best-effort form of Publish. Errors are logged and the
// event is queued for replay when possible.
func (publisher *ReliablePublisher) PublishChange(ctx context.Context, change Change) {
	if publisher == nil || publisher.publisher == nil {
		return
	}
	if err := publisher.Publish(ctx, change); err != nil {
		publisher.loggerOrDefault().Error("failed to publish change event", "error", err, "seq", change.Sequence)
	}
}

func (publisher *ReliablePublisher) publishMessage(ctx context.Context, message *nats.Msg) error {
	return publisher.breaker.Do(ctx, func(ctx context.Context) error {
		return resilience.Retry(ctx, publisher.retryConfig, func(ctx context.Context) error {
			return publisher.publisher.publishPreparedMessage(ctx, cloneMessage(message))
		})
	})
}

func (publisher *ReliablePublisher) replayLoop() {
	ticker := time.NewTicker(publisher.replayInterval)
	defer ticker.Stop()
	defer close(publisher.doneCh)

	for {
		select {
		case <-publisher.stopCh:
			return
		case <-ticker.C:
		case <-publisher.replaySignal:
		}

		replayCtx, cancel := context.WithTimeout(context.Background(), defaultReliableReplayTimeout)
		err := publisher.replayPending(replayCtx)
		cancel()
		if err != nil {
			publisher.loggerOrDefault().Warn("failed to replay nats dead letters", "error", err)
		}
	}
}

func (publisher *ReliablePublisher) replayPending(ctx context.Context) error {
	entries, err := publisher.deadLetters.list(publisher.replayBatchSize)
	if err != nil {
		return fmt.Errorf("list dead letters: %w", err)
	}
	if len(entries) == 0 {
		publisher.updateDeadLetterMetric()
		return nil
	}

	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := publisher.publishMessage(ctx, entry.message); err != nil {
			publisher.updateDeadLetterMetric()
			return fmt.Errorf("replay dead letter %s: %w", entry.path, err)
		}
		if err := publisher.deadLetters.delete(entry.path); err != nil {
			publisher.updateDeadLetterMetric()
			return fmt.Errorf("delete replayed dead letter %s: %w", entry.path, err)
		}
	}

	publisher.updateDeadLetterMetric()
	return nil
}

func (publisher *ReliablePublisher) signalReplay() {
	select {
	case publisher.replaySignal <- struct{}{}:
	default:
	}
}

func (publisher *ReliablePublisher) updateDeadLetterMetric() {
	size, err := publisher.deadLetters.count()
	if err != nil {
		publisher.loggerOrDefault().Warn("failed to count nats dead letters", "error", err)
		return
	}
	publisher.metrics.observeDeadLetterSize(publisher.publisher.subjectPrefix, float64(size))
}

func (publisher *ReliablePublisher) loggerOrDefault() *slog.Logger {
	if publisher != nil && publisher.logger != nil {
		return publisher.logger
	}
	if publisher != nil && publisher.publisher != nil {
		return publisher.publisher.loggerOrDefault()
	}
	return slog.Default()
}

func (opts ReliableOptions) withDefaults() ReliableOptions {
	opts.Publisher = opts.Publisher.withDefaults()
	if opts.ReplayInterval <= 0 {
		opts.ReplayInterval = DefaultReliableReplayInterval
	}
	if opts.ReplayBatchSize <= 0 {
		opts.ReplayBatchSize = DefaultReliableReplayBatchSize
	}
	if opts.Registerer == nil {
		opts.Registerer = prometheus.DefaultRegisterer
	}
	return opts
}

type reliableMetrics struct {
	publishFailures *prometheus.CounterVec
	deadLetterSize  *prometheus.GaugeVec
}

func newReliableMetrics(registerer prometheus.Registerer) (*reliableMetrics, error) {
	publishFailures, err := registerCounterVec(
		registerer,
		prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Name: "natsbus_publish_failures_total",
				Help: "Count of NATS change publish failures that fell back to dead-letter replay.",
			},
			[]string{"subject_prefix"},
		),
	)
	if err != nil {
		return nil, err
	}

	deadLetterSize, err := registerGaugeVec(
		registerer,
		prometheus.NewGaugeVec(
			prometheus.GaugeOpts{
				Name: "natsbus_dead_letter_size",
				Help: "Current number of queued NATS dead-letter files awaiting replay.",
			},
			[]string{"subject_prefix"},
		),
	)
	if err != nil {
		return nil, err
	}

	return &reliableMetrics{
		publishFailures: publishFailures,
		deadLetterSize:  deadLetterSize,
	}, nil
}

func (metrics *reliableMetrics) observePublishFailure(subjectPrefix string) {
	metrics.publishFailures.WithLabelValues(subjectPrefix).Inc()
}

func (metrics *reliableMetrics) observeDeadLetterSize(subjectPrefix string, size float64) {
	metrics.deadLetterSize.WithLabelValues(subjectPrefix).Set(size)
}

func registerCounterVec(registerer prometheus.Registerer, collector *prometheus.CounterVec) (*prometheus.CounterVec, error) {
	if err := registerer.Register(collector); err != nil {
		alreadyRegistered, ok := err.(prometheus.AlreadyRegisteredError)
		if !ok {
			return nil, err
		}
		existing, ok := alreadyRegistered.ExistingCollector.(*prometheus.CounterVec)
		if !ok {
			return nil, err
		}
		return existing, nil
	}
	return collector, nil
}

func registerGaugeVec(registerer prometheus.Registerer, collector *prometheus.GaugeVec) (*prometheus.GaugeVec, error) {
	if err := registerer.Register(collector); err != nil {
		alreadyRegistered, ok := err.(prometheus.AlreadyRegisteredError)
		if !ok {
			return nil, err
		}
		existing, ok := alreadyRegistered.ExistingCollector.(*prometheus.GaugeVec)
		if !ok {
			return nil, err
		}
		return existing, nil
	}
	return collector, nil
}

type fileDeadLetterQueue struct {
	baseDir string
}

type deadLetterEntry struct {
	path    string
	message *nats.Msg
}

type deadLetterRecord struct {
	CreatedAt time.Time           `json:"created_at"`
	LastError string              `json:"last_error,omitempty"`
	Subject   string              `json:"subject"`
	Header    map[string][]string `json:"header,omitempty"`
	Data      []byte              `json:"data"`
}

func newFileDeadLetterQueue(baseDir string) (*fileDeadLetterQueue, error) {
	baseDir = strings.TrimSpace(baseDir)
	if baseDir == "" {
		return nil, errDeadLetterDirRequired
	}
	if err := os.MkdirAll(baseDir, 0o750); err != nil {
		return nil, fmt.Errorf("create dead-letter dir: %w", err)
	}
	return &fileDeadLetterQueue{baseDir: baseDir}, nil
}

func (queue *fileDeadLetterQueue) enqueue(message *nats.Msg, publishErr error) error {
	record := deadLetterRecord{
		CreatedAt: time.Now().UTC(),
		Subject:   message.Subject,
		Header:    headerMap(message.Header),
		Data:      append([]byte(nil), message.Data...),
	}
	if publishErr != nil {
		record.LastError = publishErr.Error()
	}

	dir := filepath.Join(queue.baseDir, record.CreatedAt.Format("2006-01-02"))
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return fmt.Errorf("create dead-letter subdir: %w", err)
	}

	body, err := json.Marshal(record)
	if err != nil {
		return fmt.Errorf("marshal dead-letter record: %w", err)
	}

	tmp, err := os.CreateTemp(dir, "nats-dead-letter-*.tmp")
	if err != nil {
		return fmt.Errorf("create dead-letter temp file: %w", err)
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()

	if _, err := tmp.Write(body); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write dead-letter temp file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close dead-letter temp file: %w", err)
	}

	finalName := fmt.Sprintf("%s-%s.json", record.CreatedAt.Format("20060102T150405.000000000Z0700"), uuid.NewString())
	if err := os.Rename(tmpName, filepath.Join(dir, finalName)); err != nil {
		return fmt.Errorf("finalize dead-letter file: %w", err)
	}
	return nil
}

func (queue *fileDeadLetterQueue) list(limit int) ([]deadLetterEntry, error) {
	paths := make([]string, 0)
	if err := filepath.WalkDir(queue.baseDir, func(path string, dirEntry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if dirEntry.IsDir() || filepath.Ext(path) != ".json" {
			return nil
		}
		paths = append(paths, path)
		return nil
	}); err != nil {
		return nil, err
	}

	sort.Strings(paths)
	if limit > 0 && len(paths) > limit {
		paths = paths[:limit]
	}

	entries := make([]deadLetterEntry, 0, len(paths))
	for _, path := range paths {
		body, err := os.ReadFile(path) //nolint:gosec // Path is enumerated from queue.baseDir via filepath.WalkDir above.
		if err != nil {
			return nil, fmt.Errorf("read dead-letter file %s: %w", path, err)
		}
		var record deadLetterRecord
		if err := json.Unmarshal(body, &record); err != nil {
			return nil, fmt.Errorf("unmarshal dead-letter file %s: %w", path, err)
		}
		entries = append(entries, deadLetterEntry{
			path: path,
			message: &nats.Msg{
				Subject: record.Subject,
				Header:  nats.Header(record.Header),
				Data:    append([]byte(nil), record.Data...),
			},
		})
	}
	return entries, nil
}

func (queue *fileDeadLetterQueue) delete(path string) error {
	return os.Remove(path)
}

func (queue *fileDeadLetterQueue) count() (int, error) {
	count := 0
	if err := filepath.WalkDir(queue.baseDir, func(path string, dirEntry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if dirEntry.IsDir() || filepath.Ext(path) != ".json" {
			return nil
		}
		count++
		return nil
	}); err != nil {
		return 0, err
	}
	return count, nil
}

func cloneMessage(message *nats.Msg) *nats.Msg {
	if message == nil {
		return nil
	}
	cloned := &nats.Msg{
		Subject: message.Subject,
		Reply:   message.Reply,
		Data:    append([]byte(nil), message.Data...),
	}
	if message.Header != nil {
		cloned.Header = nats.Header(headerMap(message.Header))
	}
	return cloned
}

func headerMap(header nats.Header) map[string][]string {
	if header == nil {
		return nil
	}
	cloned := make(map[string][]string, len(header))
	for key, values := range header {
		cloned[key] = append([]string(nil), values...)
	}
	return cloned
}
