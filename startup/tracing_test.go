package startup

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace/noop"
)

func TestTracingConfigFromEnvDefaults(t *testing.T) {
	cfg, err := TracingConfigFromEnv("runtime-test")
	if err != nil {
		t.Fatalf("expected defaults to parse, got %v", err)
	}
	if cfg.Endpoint != "" {
		t.Fatalf("expected tracing to be disabled by default, got endpoint %q", cfg.Endpoint)
	}
	if cfg.ServiceName != "runtime-test" {
		t.Fatalf("expected fallback service name, got %q", cfg.ServiceName)
	}
	if cfg.Sampler != "parentbased_traceidratio" {
		t.Fatalf("expected default sampler, got %q", cfg.Sampler)
	}
	if cfg.SampleRatio != 1 {
		t.Fatalf("expected default sample ratio 1, got %v", cfg.SampleRatio)
	}
}

func TestTracingConfigFromEnvParsesSampler(t *testing.T) {
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "http://tempo:4318")
	t.Setenv("OTEL_SERVICE_NAME", "runtime-from-env")
	t.Setenv("OTEL_TRACES_SAMPLER", "parentbased_traceidratio:0.25")

	cfg, err := TracingConfigFromEnv("runtime-test")
	if err != nil {
		t.Fatalf("expected env config to parse, got %v", err)
	}
	if cfg.Endpoint != "http://tempo:4318" {
		t.Fatalf("unexpected endpoint %q", cfg.Endpoint)
	}
	if cfg.ServiceName != "runtime-from-env" {
		t.Fatalf("unexpected service name %q", cfg.ServiceName)
	}
	if cfg.Sampler != "parentbased_traceidratio" {
		t.Fatalf("unexpected sampler %q", cfg.Sampler)
	}
	if cfg.SampleRatio != 0.25 {
		t.Fatalf("unexpected sample ratio %v", cfg.SampleRatio)
	}
}

func TestTracingConfigFromEnvUsesSamplerArg(t *testing.T) {
	t.Setenv("OTEL_TRACES_SAMPLER", "traceidratio")
	t.Setenv("OTEL_TRACES_SAMPLER_ARG", "0.5")

	cfg, err := TracingConfigFromEnv("runtime-test")
	if err != nil {
		t.Fatalf("expected sampler arg config to parse, got %v", err)
	}
	if cfg.Sampler != "traceidratio" {
		t.Fatalf("unexpected sampler %q", cfg.Sampler)
	}
	if cfg.SampleRatio != 0.5 {
		t.Fatalf("unexpected sample ratio %v", cfg.SampleRatio)
	}
}

func TestTracingConfigFromEnvParsesScientificNotationRatios(t *testing.T) {
	testCases := []struct {
		name        string
		rawSampler  string
		wantSampler string
		wantRatio   float64
	}{
		{
			name:        "bare ratio",
			rawSampler:  "1e-3",
			wantSampler: "parentbased_traceidratio",
			wantRatio:   1e-3,
		},
		{
			name:        "inline ratio",
			rawSampler:  "traceidratio:1e-3",
			wantSampler: "traceidratio",
			wantRatio:   1e-3,
		},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Setenv("OTEL_TRACES_SAMPLER", testCase.rawSampler)

			cfg, err := TracingConfigFromEnv("runtime-test")
			if err != nil {
				t.Fatalf("expected scientific notation ratio to parse, got %v", err)
			}
			if cfg.Sampler != testCase.wantSampler {
				t.Fatalf("unexpected sampler %q", cfg.Sampler)
			}
			if cfg.SampleRatio != testCase.wantRatio {
				t.Fatalf("unexpected sample ratio %v", cfg.SampleRatio)
			}
		})
	}
}

func TestTracingConfigFromEnvRejectsInvalidRatio(t *testing.T) {
	t.Setenv("OTEL_TRACES_SAMPLER", "parentbased_traceidratio:2")

	if _, err := TracingConfigFromEnv("runtime-test"); err == nil {
		t.Fatal("expected invalid ratio to fail")
	}
}

func TestTracingConfigFromEnvRejectsMissingInlineRatio(t *testing.T) {
	t.Setenv("OTEL_TRACES_SAMPLER", "traceidratio:")

	if _, err := TracingConfigFromEnv("runtime-test"); err == nil {
		t.Fatal("expected missing inline ratio to fail")
	}
}

func TestTracingConfigFromEnvRejectsNonNumericInlineRatio(t *testing.T) {
	t.Setenv("OTEL_TRACES_SAMPLER", "traceidratio:abc")

	if _, err := TracingConfigFromEnv("runtime-test"); err == nil {
		t.Fatal("expected non-numeric inline ratio to fail")
	}
}

func TestInitTracingDisabled(t *testing.T) {
	originalProvider := noop.NewTracerProvider()
	otel.SetTracerProvider(originalProvider)
	otel.SetTextMapPropagator(propagation.TraceContext{})

	shutdown, err := InitTracing(context.Background(), TracingConfig{ServiceName: "runtime-test"})
	if err != nil {
		t.Fatalf("expected disabled init to succeed, got %v", err)
	}
	if err := shutdown(context.Background()); err != nil {
		t.Fatalf("expected disabled shutdown to succeed, got %v", err)
	}
	if otel.GetTracerProvider() != originalProvider {
		t.Fatal("expected disabled tracing init to leave tracer provider unchanged")
	}
}

func TestInitTracingExportsSpansAndRestoresGlobals(t *testing.T) {
	server, requests := newTraceCollector(t)

	originalProvider := noop.NewTracerProvider()
	otel.SetTracerProvider(originalProvider)
	otel.SetTextMapPropagator(propagation.TraceContext{})

	shutdown, err := InitTracing(context.Background(), TracingConfig{
		Endpoint:      server.URL,
		ServiceName:   "runtime-test",
		Sampler:       "always_on",
		SampleRatio:   1,
		BatchTimeout:  10 * time.Millisecond,
		ExportTimeout: 250 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("expected tracing init to succeed, got %v", err)
	}

	_, span := otel.Tracer("runtime-test").Start(context.Background(), "boot")
	span.End()

	shutdownCtx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := shutdown(shutdownCtx); err != nil {
		t.Fatalf("expected tracing shutdown to succeed, got %v", err)
	}

	select {
	case request := <-requests:
		if request.path != "/v1/traces" {
			t.Fatalf("expected otlp traces path, got %q", request.path)
		}
		if !strings.HasPrefix(request.contentType, "application/x-protobuf") {
			t.Fatalf("expected protobuf content type, got %q", request.contentType)
		}
		if len(request.body) == 0 {
			t.Fatal("expected non-empty export payload")
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for exported spans")
	}

	if otel.GetTracerProvider() != originalProvider {
		t.Fatal("expected tracer provider to be restored after shutdown")
	}
	if _, ok := otel.GetTextMapPropagator().(propagation.TraceContext); !ok {
		t.Fatalf("expected original propagator to be restored, got %T", otel.GetTextMapPropagator())
	}
}

func TestInitTracingShutsDownExporterOnResourceError(t *testing.T) {
	exporter := &recordingSpanExporter{}
	stubOTLPTraceExporter(t, exporter, nil)
	t.Setenv("OTEL_RESOURCE_ATTRIBUTES", "invalid")

	_, err := InitTracing(context.Background(), TracingConfig{
		Endpoint:    "http://tempo:4318",
		ServiceName: "runtime-test",
		Sampler:     "always_on",
		SampleRatio: 1,
	})
	if err == nil {
		t.Fatal("expected invalid resource attributes to fail")
	}
	if !exporter.shutdownCalled {
		t.Fatal("expected exporter shutdown on resource initialization error")
	}
}

func TestInitTracingShutsDownExporterOnSamplerError(t *testing.T) {
	exporter := &recordingSpanExporter{}
	stubOTLPTraceExporter(t, exporter, nil)

	_, err := InitTracing(context.Background(), TracingConfig{
		Endpoint:    "http://tempo:4318",
		ServiceName: "runtime-test",
		Sampler:     "invalid",
		SampleRatio: 1,
	})
	if err == nil {
		t.Fatal("expected invalid sampler to fail")
	}
	if !exporter.shutdownCalled {
		t.Fatal("expected exporter shutdown on sampler initialization error")
	}
}

func TestLifecycleShutdownRunsHooksInReverseOrder(t *testing.T) {
	lifecycle := NewLifecycle()
	order := make([]string, 0, 2)

	lifecycle.OnShutdown("first", func(context.Context) error {
		order = append(order, "first")
		return errors.New("first failed")
	})
	lifecycle.OnShutdown("second", func(context.Context) error {
		order = append(order, "second")
		return errors.New("second failed")
	})

	err := lifecycle.Shutdown(context.Background())
	if err == nil {
		t.Fatal("expected joined shutdown error")
	}
	if !reflect.DeepEqual(order, []string{"second", "first"}) {
		t.Fatalf("expected reverse shutdown order, got %v", order)
	}
	if !strings.Contains(err.Error(), "second failed") || !strings.Contains(err.Error(), "first failed") {
		t.Fatalf("expected joined errors, got %v", err)
	}

	if err := lifecycle.Shutdown(context.Background()); err != nil {
		t.Fatalf("expected shutdown to be idempotent, got %v", err)
	}
}

func TestLifecycleEnableTracingFromEnvRegistersShutdown(t *testing.T) {
	server, requests := newTraceCollector(t)
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", server.URL)
	t.Setenv("OTEL_TRACES_SAMPLER", "always_on")

	originalProvider := noop.NewTracerProvider()
	otel.SetTracerProvider(originalProvider)
	otel.SetTextMapPropagator(propagation.TraceContext{})

	lifecycle := NewLifecycle()
	if err := lifecycle.EnableTracingFromEnv(context.Background(), "runtime-test"); err != nil {
		t.Fatalf("expected lifecycle tracing bootstrap to succeed, got %v", err)
	}

	_, span := otel.Tracer("runtime-test").Start(context.Background(), "request")
	span.End()

	shutdownCtx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := lifecycle.Shutdown(shutdownCtx); err != nil {
		t.Fatalf("expected lifecycle shutdown to succeed, got %v", err)
	}

	select {
	case <-requests:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for lifecycle-registered trace export")
	}
}

type recordingSpanExporter struct {
	shutdownCalled bool
}

func (e *recordingSpanExporter) ExportSpans(context.Context, []sdktrace.ReadOnlySpan) error {
	return nil
}

func (e *recordingSpanExporter) Shutdown(context.Context) error {
	e.shutdownCalled = true
	return nil
}

func stubOTLPTraceExporter(t *testing.T, exporter sdktrace.SpanExporter, err error) {
	t.Helper()

	original := newOTLPTraceExporter
	newOTLPTraceExporter = func(context.Context, string) (sdktrace.SpanExporter, error) {
		return exporter, err
	}
	t.Cleanup(func() {
		newOTLPTraceExporter = original
	})
}

type traceRequest struct {
	path        string
	contentType string
	body        []byte
}

func newTraceCollector(t *testing.T) (*httptest.Server, <-chan traceRequest) {
	t.Helper()

	requests := make(chan traceRequest, 8)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		body, err := io.ReadAll(request.Body)
		if err != nil {
			writer.WriteHeader(http.StatusInternalServerError)
			return
		}
		_ = request.Body.Close()
		requests <- traceRequest{
			path:        request.URL.Path,
			contentType: request.Header.Get("Content-Type"),
			body:        body,
		}
		writer.WriteHeader(http.StatusAccepted)
	}))
	t.Cleanup(server.Close)
	return server, requests
}
