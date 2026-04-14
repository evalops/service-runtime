package observability

import (
	"net/http"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
)

func TestBoundedLabelFallsBackToOther(t *testing.T) {
	t.Parallel()

	label := NewBoundedLabel("method", http.MethodGet, http.MethodPost)
	if got := label.Name(); got != "method" {
		t.Fatalf("label name = %q, want method", got)
	}
	if got := label.Value(http.MethodGet); got != http.MethodGet {
		t.Fatalf("allowed value = %q, want %q", got, http.MethodGet)
	}
	if got := label.Value("BREW"); got != "other" {
		t.Fatalf("unexpected value = %q, want other", got)
	}
	if got := label.Value("   "); got != "other" {
		t.Fatalf("blank value = %q, want other", got)
	}
}

func TestMetricsRecordRequestClampsUnexpectedLabels(t *testing.T) {
	t.Parallel()

	registry := prometheus.NewRegistry()
	metrics, err := NewMetrics("pipeline", MetricsOptions{
		Registerer: registry,
		Gatherer:   registry,
	})
	if err != nil {
		t.Fatalf("new metrics: %v", err)
	}

	metrics.RecordRequest("brew", "/healthz", 0, 0)

	metricFamilies, err := registry.Gather()
	if err != nil {
		t.Fatalf("gather request metrics: %v", err)
	}
	if got := metricValueWithLabels(metricFamilies, "pipeline_http_requests_total", map[string]string{
		"method": "other",
		"route":  "/healthz",
		"status": "other",
	}); got != 1 {
		t.Fatalf("request counter = %v, want 1", got)
	}
	if got := histogramCountWithLabels(metricFamilies, "pipeline_http_request_duration_seconds", map[string]string{
		"method": "other",
		"route":  "/healthz",
		"status": "other",
	}); got != 1 {
		t.Fatalf("request histogram count = %v, want 1", got)
	}
}
