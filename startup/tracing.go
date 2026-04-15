package startup

import (
	"context"
	"fmt"
	"math"
	"os"
	"strconv"
	"strings"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.40.0"
)

const (
	defaultTracingServiceName = "service"
	defaultTracingSampler     = "parentbased_traceidratio"
	defaultTracingRatio       = 1.0

	// DefaultTraceBatchTimeout is the default max delay before spans are exported.
	DefaultTraceBatchTimeout = time.Second
	// DefaultTraceExportTimeout is the default per-batch export timeout.
	DefaultTraceExportTimeout = 5 * time.Second
)

// TracingConfig controls shared OpenTelemetry tracer bootstrap.
type TracingConfig struct {
	Endpoint      string
	ServiceName   string
	Sampler       string
	SampleRatio   float64
	BatchTimeout  time.Duration
	ExportTimeout time.Duration
}

// Enabled reports whether tracing should be configured.
func (cfg TracingConfig) Enabled() bool {
	return strings.TrimSpace(cfg.Endpoint) != ""
}

// TracingConfigFromEnv loads tracer configuration from the standard OTEL env vars.
func TracingConfigFromEnv(serviceName string) (TracingConfig, error) {
	sampler, ratio, err := parseTracingSampler(
		os.Getenv("OTEL_TRACES_SAMPLER"),
		os.Getenv("OTEL_TRACES_SAMPLER_ARG"),
	)
	if err != nil {
		return TracingConfig{}, err
	}

	defaultServiceName := strings.TrimSpace(serviceName)
	serviceName = strings.TrimSpace(os.Getenv("OTEL_SERVICE_NAME"))
	if serviceName == "" {
		serviceName = defaultServiceName
	}
	if serviceName == "" {
		serviceName = defaultTracingServiceName
	}

	return TracingConfig{
		Endpoint:      strings.TrimSpace(os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT")),
		ServiceName:   serviceName,
		Sampler:       sampler,
		SampleRatio:   ratio,
		BatchTimeout:  DefaultTraceBatchTimeout,
		ExportTimeout: DefaultTraceExportTimeout,
	}, nil
}

// InitTracing configures the global tracer provider and propagators.
func InitTracing(ctx context.Context, cfg TracingConfig) (ShutdownFunc, error) {
	if !cfg.Enabled() {
		return func(context.Context) error { return nil }, nil
	}

	serviceName := strings.TrimSpace(cfg.ServiceName)
	if serviceName == "" {
		serviceName = defaultTracingServiceName
	}

	exporter, err := otlptracehttp.New(
		ctx,
		otlptracehttp.WithEndpointURL(strings.TrimSpace(cfg.Endpoint)),
	)
	if err != nil {
		return nil, fmt.Errorf("initialize otlp trace exporter: %w", err)
	}

	res, err := resource.New(
		ctx,
		resource.WithFromEnv(),
		resource.WithProcess(),
		resource.WithContainer(),
		resource.WithHost(),
		resource.WithTelemetrySDK(),
		resource.WithAttributes(semconv.ServiceName(serviceName)),
	)
	if err != nil {
		return nil, fmt.Errorf("initialize otel resource: %w", err)
	}

	sampler, err := tracingSampler(cfg)
	if err != nil {
		return nil, err
	}

	batchTimeout := cfg.BatchTimeout
	if batchTimeout <= 0 {
		batchTimeout = DefaultTraceBatchTimeout
	}
	exportTimeout := cfg.ExportTimeout
	if exportTimeout <= 0 {
		exportTimeout = DefaultTraceExportTimeout
	}

	tracerProvider := sdktrace.NewTracerProvider(
		sdktrace.WithSampler(sampler),
		sdktrace.WithResource(res),
		sdktrace.WithBatcher(
			exporter,
			sdktrace.WithBatchTimeout(batchTimeout),
			sdktrace.WithExportTimeout(exportTimeout),
		),
	)

	previousProvider := otel.GetTracerProvider()
	previousPropagator := otel.GetTextMapPropagator()

	otel.SetTracerProvider(tracerProvider)
	otel.SetTextMapPropagator(
		propagation.NewCompositeTextMapPropagator(
			propagation.TraceContext{},
			propagation.Baggage{},
		),
	)

	return func(ctx context.Context) error {
		err := tracerProvider.Shutdown(ctx)
		otel.SetTracerProvider(previousProvider)
		otel.SetTextMapPropagator(previousPropagator)
		return err
	}, nil
}

// EnableTracingFromEnv configures tracing from OTEL env vars and registers the shutdown hook.
func (l *Lifecycle) EnableTracingFromEnv(ctx context.Context, serviceName string) error {
	cfg, err := TracingConfigFromEnv(serviceName)
	if err != nil {
		return err
	}

	shutdown, err := InitTracing(ctx, cfg)
	if err != nil {
		return err
	}
	if cfg.Enabled() {
		l.OnShutdown("otel tracer provider", shutdown)
	}
	return nil
}

func tracingSampler(cfg TracingConfig) (sdktrace.Sampler, error) {
	switch cfg.Sampler {
	case "always_on":
		return sdktrace.AlwaysSample(), nil
	case "always_off":
		return sdktrace.NeverSample(), nil
	case "traceidratio":
		if err := validateTracingRatio(cfg.SampleRatio); err != nil {
			return nil, err
		}
		return sdktrace.TraceIDRatioBased(cfg.SampleRatio), nil
	case "parentbased_traceidratio", "":
		if err := validateTracingRatio(cfg.SampleRatio); err != nil {
			return nil, err
		}
		return sdktrace.ParentBased(sdktrace.TraceIDRatioBased(cfg.SampleRatio)), nil
	default:
		return nil, fmt.Errorf("unsupported OTEL_TRACES_SAMPLER %q", cfg.Sampler)
	}
}

func parseTracingSampler(raw, rawArg string) (string, float64, error) {
	raw = normalizeSampler(raw)
	rawArg = strings.TrimSpace(rawArg)

	if raw == "" {
		return defaultTracingSampler, defaultTracingRatio, nil
	}

	if ratio, ok, err := parseTracingRatio(raw); err != nil {
		return "", 0, err
	} else if ok {
		return defaultTracingSampler, ratio, nil
	}

	if sampler, ratio, ok, err := parseInlineTracingRatio(raw); err != nil {
		return "", 0, err
	} else if ok {
		return sampler, ratio, nil
	}

	switch raw {
	case "always_on", "always_off":
		return raw, defaultTracingRatio, nil
	case "traceidratio", "parentbased_traceidratio":
		ratio := defaultTracingRatio
		if rawArg != "" {
			parsedRatio, ok, err := parseTracingRatio(rawArg)
			if err != nil {
				return "", 0, err
			}
			if !ok {
				return "", 0, fmt.Errorf("OTEL_TRACES_SAMPLER_ARG %q must be a number between 0 and 1", rawArg)
			}
			ratio = parsedRatio
		}
		if err := validateTracingRatio(ratio); err != nil {
			return "", 0, err
		}
		return raw, ratio, nil
	default:
		return "", 0, fmt.Errorf("unsupported OTEL_TRACES_SAMPLER %q", raw)
	}
}

func parseInlineTracingRatio(raw string) (string, float64, bool, error) {
	sampler, rawRatio, ok := strings.Cut(raw, ":")
	if !ok {
		return "", 0, false, nil
	}

	sampler = normalizeSampler(sampler)
	if sampler != "traceidratio" && sampler != "parentbased_traceidratio" {
		return "", 0, false, fmt.Errorf("unsupported OTEL_TRACES_SAMPLER %q", raw)
	}

	ratio, _, err := parseTracingRatio(rawRatio)
	if err != nil {
		return "", 0, false, err
	}
	return sampler, ratio, true, nil
}

func parseTracingRatio(raw string) (float64, bool, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0, false, nil
	}

	if !looksLikeTracingRatio(raw) {
		return 0, false, nil
	}
	ratio, err := strconv.ParseFloat(raw, 64)
	if err != nil {
		return 0, false, fmt.Errorf("invalid OTEL trace sample ratio %q: %w", raw, err)
	}
	if err := validateTracingRatio(ratio); err != nil {
		return 0, false, err
	}
	return ratio, true, nil
}

func validateTracingRatio(ratio float64) error {
	if math.IsNaN(ratio) || ratio < 0 || ratio > 1 {
		return fmt.Errorf("OTEL trace sample ratio must be between 0 and 1, got %v", ratio)
	}
	return nil
}

func normalizeSampler(raw string) string {
	raw = strings.TrimSpace(strings.ToLower(raw))
	raw = strings.ReplaceAll(raw, "-", "_")
	return raw
}

func looksLikeTracingRatio(raw string) bool {
	for _, runeValue := range raw {
		switch {
		case runeValue >= '0' && runeValue <= '9':
		case runeValue == '.', runeValue == '+', runeValue == '-', runeValue == 'e', runeValue == 'E':
		default:
			return false
		}
	}
	return true
}
