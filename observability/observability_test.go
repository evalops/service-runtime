package observability

import (
	"database/sql"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
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
