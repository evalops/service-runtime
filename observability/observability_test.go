package observability

import (
	"context"
	"database/sql"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	goredis "github.com/redis/go-redis/v9"
)

func TestNewMetricsUsesServiceScopedNames(t *testing.T) {
	t.Parallel()

	registry := prometheus.NewRegistry()
	metrics, err := NewMetrics("llm-gateway", MetricsOptions{
		Registerer: registry,
		Gatherer:   registry,
	})
	if err != nil {
		t.Fatalf("new metrics: %v", err)
	}

	metrics.RecordRequest(http.MethodGet, "/healthz", http.StatusOK, 0)
	metricFamilies, err := registry.Gather()
	if err != nil {
		t.Fatalf("gather metrics: %v", err)
	}

	if !containsMetric(metricFamilies, "llm_gateway_http_requests_total") {
		t.Fatalf("expected llm_gateway_http_requests_total metric, got %#v", metricFamilies)
	}
	if !containsMetric(metricFamilies, "llm_gateway_http_request_duration_seconds") {
		t.Fatalf("expected llm_gateway_http_request_duration_seconds metric")
	}
}

func TestRegisterDBStatsRegistersGaugesOnce(t *testing.T) {
	t.Parallel()

	registry := prometheus.NewRegistry()
	statFunc := func() sql.DBStats {
		return sql.DBStats{
			OpenConnections: 3,
			InUse:           2,
			Idle:            1,
		}
	}

	if err := RegisterDBStats("pipeline", statFunc, DBStatsOptions{Registerer: registry}); err != nil {
		t.Fatalf("register db stats: %v", err)
	}
	if err := RegisterDBStats("pipeline", statFunc, DBStatsOptions{Registerer: registry}); err != nil {
		t.Fatalf("repeat register db stats: %v", err)
	}

	metricFamilies, err := registry.Gather()
	if err != nil {
		t.Fatalf("gather db stats: %v", err)
	}

	if !containsMetric(metricFamilies, "pipeline_db_open_connections") {
		t.Fatalf("expected pipeline_db_open_connections metric")
	}
}

func TestRegisterRedisPoolStatsRegistersCollectorsOnce(t *testing.T) {
	t.Parallel()

	registry := prometheus.NewRegistry()
	stats := &goredis.PoolStats{
		Hits:            11,
		Misses:          2,
		Timeouts:        1,
		WaitCount:       3,
		Unusable:        4,
		WaitDurationNs:  2 * int64(1e9),
		TotalConns:      5,
		IdleConns:       2,
		StaleConns:      6,
		PendingRequests: 1,
	}

	if err := RegisterRedisPoolStats("pipeline", func() *goredis.PoolStats { return stats }, RedisStatsOptions{Registerer: registry}); err != nil {
		t.Fatalf("register redis stats: %v", err)
	}
	if err := RegisterRedisPoolStats("pipeline", func() *goredis.PoolStats { return stats }, RedisStatsOptions{Registerer: registry}); err != nil {
		t.Fatalf("repeat register redis stats: %v", err)
	}

	metricFamilies, err := registry.Gather()
	if err != nil {
		t.Fatalf("gather redis stats: %v", err)
	}

	if !containsMetric(metricFamilies, "pipeline_redis_pool_total_connections") {
		t.Fatalf("expected pipeline_redis_pool_total_connections metric")
	}
	if !containsMetric(metricFamilies, "pipeline_redis_pool_hits_total") {
		t.Fatalf("expected pipeline_redis_pool_hits_total metric")
	}
	if got := metricValue(metricFamilies, "pipeline_redis_pool_pending_requests"); got != 1 {
		t.Fatalf("pending requests = %v, want 1", got)
	}
	if got := metricValue(metricFamilies, "pipeline_redis_pool_wait_duration_seconds_total"); got != 2 {
		t.Fatalf("wait duration seconds = %v, want 2", got)
	}
}

func TestNewRedisCommandHookRecordsCommandMetrics(t *testing.T) {
	t.Parallel()

	registry := prometheus.NewRegistry()
	hook, err := NewRedisCommandHook("pipeline", RedisCommandMetricsOptions{
		Registerer:      registry,
		DurationBuckets: []float64{0.000001, 1},
	})
	if err != nil {
		t.Fatalf("new redis command hook: %v", err)
	}

	cmd := goredis.NewStringCmd(context.Background(), "get", "contact:123")
	if err := hook.ProcessHook(func(ctx context.Context, cmd goredis.Cmder) error {
		return nil
	})(context.Background(), cmd); err != nil {
		t.Fatalf("process hook: %v", err)
	}

	metricFamilies, err := registry.Gather()
	if err != nil {
		t.Fatalf("gather redis command metrics: %v", err)
	}

	if got := metricValueWithLabels(metricFamilies, "pipeline_redis_commands_total", map[string]string{"command": "get", "status": "ok"}); got != 1 {
		t.Fatalf("redis command counter = %v, want 1", got)
	}
	if got := histogramCountWithLabels(metricFamilies, "pipeline_redis_command_duration_seconds", map[string]string{"command": "get", "status": "ok"}); got != 1 {
		t.Fatalf("redis command histogram count = %v, want 1", got)
	}
}

func TestNewRedisCommandHookRecordsPipelineErrors(t *testing.T) {
	t.Parallel()

	registry := prometheus.NewRegistry()
	hook, err := NewRedisCommandHook("pipeline", RedisCommandMetricsOptions{
		Registerer:      registry,
		DurationBuckets: []float64{0.000001, 1},
	})
	if err != nil {
		t.Fatalf("new redis command hook: %v", err)
	}

	errBoom := errors.New("boom")
	cmds := []goredis.Cmder{
		goredis.NewStatusCmd(context.Background(), "hset", "proxy:abc", "active", 1),
		goredis.NewStatusCmd(context.Background(), "expireat", "proxy:abc", "2026-01-01T00:00:00Z"),
	}
	if err := hook.ProcessPipelineHook(func(ctx context.Context, cmds []goredis.Cmder) error {
		return errBoom
	})(context.Background(), cmds); !errors.Is(err, errBoom) {
		t.Fatalf("process pipeline hook error = %v, want %v", err, errBoom)
	}

	metricFamilies, err := registry.Gather()
	if err != nil {
		t.Fatalf("gather redis pipeline metrics: %v", err)
	}

	if got := metricValueWithLabels(metricFamilies, "pipeline_redis_commands_total", map[string]string{"command": "pipeline", "status": "error"}); got != 1 {
		t.Fatalf("redis pipeline counter = %v, want 1", got)
	}
	if got := histogramCountWithLabels(metricFamilies, "pipeline_redis_command_duration_seconds", map[string]string{"command": "pipeline", "status": "error"}); got != 1 {
		t.Fatalf("redis pipeline histogram count = %v, want 1", got)
	}
}

func TestRequestLoggingMiddlewareRecordsMetricsAndWideEvent(t *testing.T) {
	t.Parallel()

	registry := prometheus.NewRegistry()
	metrics, err := NewMetrics("pipeline", MetricsOptions{
		Registerer: registry,
		Gatherer:   registry,
	})
	if err != nil {
		t.Fatalf("new metrics: %v", err)
	}

	var output strings.Builder
	logger := slog.New(slog.NewTextHandler(&output, nil))

	router := chi.NewRouter()
	router.Use(RequestLoggingMiddleware(logger, metrics))
	router.Get("/contacts/{contactID}", func(writer http.ResponseWriter, request *http.Request) {
		SetWideEvent(request, NewWideEvent("pipeline.contact.read", "pipeline.contacts", "contact", "read").AddAttributes(map[string]any{
			"contact_id": chi.URLParam(request, "contactID"),
		}))
		writer.Header().Set("X-Request-Id", "req-123")
		writer.WriteHeader(http.StatusOK)
		_, _ = writer.Write([]byte(`{"ok":true}`))
	})

	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/contacts/123", nil))

	logLine := output.String()
	if !strings.Contains(logLine, "request_id=req-123") {
		t.Fatalf("expected request_id in log output, got %q", logLine)
	}
	if !strings.Contains(logLine, "pipeline.contact.read") {
		t.Fatalf("expected wide event in log output, got %q", logLine)
	}

	metricFamilies, err := registry.Gather()
	if err != nil {
		t.Fatalf("gather request metrics: %v", err)
	}
	if !containsMetric(metricFamilies, "pipeline_http_requests_total") {
		t.Fatalf("expected pipeline_http_requests_total metric")
	}
}

func TestMetricsHandlerUsesCustomGatherer(t *testing.T) {
	t.Parallel()

	registry := prometheus.NewRegistry()
	metrics, err := NewMetrics("pipeline", MetricsOptions{
		Registerer: registry,
		Gatherer:   registry,
	})
	if err != nil {
		t.Fatalf("new metrics: %v", err)
	}
	metrics.RecordRequest(http.MethodGet, "/metrics", http.StatusOK, 0)

	recorder := httptest.NewRecorder()
	metrics.Handler().ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/metrics", nil))

	if recorder.Code != http.StatusOK {
		t.Fatalf("expected status %d, got %d", http.StatusOK, recorder.Code)
	}
	body, _ := io.ReadAll(recorder.Body)
	if !strings.Contains(string(body), "pipeline_http_requests_total") {
		t.Fatalf("expected metrics body to contain pipeline_http_requests_total, got %q", string(body))
	}
}

func TestRequestLoggingMiddlewareUsesCaptureResponseWriter(t *testing.T) {
	t.Parallel()

	registry := prometheus.NewRegistry()
	metrics, err := NewMetrics("testkit", MetricsOptions{
		Registerer: registry,
		Gatherer:   registry,
	})
	if err != nil {
		t.Fatalf("new metrics: %v", err)
	}

	var output strings.Builder
	logger := slog.New(slog.NewTextHandler(&output, nil))

	router := chi.NewRouter()
	router.Use(RequestLoggingMiddleware(logger, metrics))
	router.Post("/items/{itemID}", func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("X-Request-Id", "req-abc")
		writer.WriteHeader(http.StatusCreated)
		_, _ = writer.Write([]byte(`{"created":true}`))
	})

	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/items/42", nil))

	// Verify status code was captured and recorded in metrics.
	metricFamilies, err := registry.Gather()
	if err != nil {
		t.Fatalf("gather metrics: %v", err)
	}
	if !containsMetric(metricFamilies, "testkit_http_requests_total") {
		t.Fatalf("expected testkit_http_requests_total metric after refactor")
	}

	// Verify log line contains the captured status code (201) and route pattern.
	logLine := output.String()
	if !strings.Contains(logLine, "status=201") {
		t.Fatalf("expected status=201 in log output, got %q", logLine)
	}
	if !strings.Contains(logLine, "request_id=req-abc") {
		t.Fatalf("expected request_id=req-abc in log output, got %q", logLine)
	}
	if !strings.Contains(logLine, "route=/items/{itemID}") {
		t.Fatalf("expected route=/items/{itemID} in log output, got %q", logLine)
	}
}

func containsMetric(metricFamilies []*dto.MetricFamily, name string) bool {
	for _, family := range metricFamilies {
		if family.GetName() == name {
			return true
		}
	}
	return false
}

func metricValue(metricFamilies []*dto.MetricFamily, name string) float64 {
	for _, family := range metricFamilies {
		if family.GetName() != name || len(family.Metric) == 0 {
			continue
		}
		switch family.GetType() {
		case dto.MetricType_COUNTER:
			return family.Metric[0].GetCounter().GetValue()
		case dto.MetricType_GAUGE:
			return family.Metric[0].GetGauge().GetValue()
		}
	}
	return 0
}

func metricValueWithLabels(metricFamilies []*dto.MetricFamily, name string, labels map[string]string) float64 {
	for _, family := range metricFamilies {
		if family.GetName() != name {
			continue
		}
		for _, metric := range family.Metric {
			if !metricHasLabels(metric, labels) {
				continue
			}
			switch family.GetType() {
			case dto.MetricType_COUNTER:
				return metric.GetCounter().GetValue()
			case dto.MetricType_GAUGE:
				return metric.GetGauge().GetValue()
			}
		}
	}
	return 0
}

func histogramCountWithLabels(metricFamilies []*dto.MetricFamily, name string, labels map[string]string) uint64 {
	for _, family := range metricFamilies {
		if family.GetName() != name || family.GetType() != dto.MetricType_HISTOGRAM {
			continue
		}
		for _, metric := range family.Metric {
			if metricHasLabels(metric, labels) {
				return metric.GetHistogram().GetSampleCount()
			}
		}
	}
	return 0
}

func metricHasLabels(metric *dto.Metric, labels map[string]string) bool {
	if len(labels) == 0 {
		return true
	}
	values := make(map[string]string, len(metric.Label))
	for _, label := range metric.Label {
		values[label.GetName()] = label.GetValue()
	}
	for key, want := range labels {
		if values[key] != want {
			return false
		}
	}
	return true
}
