package observability

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// MetricsOptions configures Prometheus metric registration for HTTP handlers.
type MetricsOptions struct {
	Registerer     prometheus.Registerer
	Gatherer       prometheus.Gatherer
	RequestBuckets []float64
}

// Metrics holds Prometheus counters and histograms for HTTP request tracking.
type Metrics struct {
	serviceName     string
	gatherer        prometheus.Gatherer
	requestsTotal   *prometheus.CounterVec
	requestDuration *prometheus.HistogramVec
}

var (
	httpMethodLabel = NewBoundedLabel(
		"method",
		http.MethodConnect,
		http.MethodDelete,
		http.MethodGet,
		http.MethodHead,
		http.MethodOptions,
		http.MethodPatch,
		http.MethodPost,
		http.MethodPut,
		http.MethodTrace,
	)
	httpStatusLabel = newHTTPStatusLabel()
)

// NewMetrics creates and registers Prometheus metrics scoped to the given service name.
func NewMetrics(serviceName string, opts MetricsOptions) (*Metrics, error) {
	opts = opts.withDefaults()
	prefix := metricPrefix(serviceName)

	requestsTotal, err := registerCounterVec(
		opts.Registerer,
		prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Name: fmt.Sprintf("%s_http_requests_total", prefix),
				Help: fmt.Sprintf("Count of HTTP requests handled by %s.", serviceName),
			},
			[]string{"method", "route", "status"},
		),
	)
	if err != nil {
		return nil, err
	}

	requestDuration, err := registerHistogramVec(
		opts.Registerer,
		prometheus.NewHistogramVec(
			prometheus.HistogramOpts{
				Name:    fmt.Sprintf("%s_http_request_duration_seconds", prefix),
				Help:    fmt.Sprintf("Duration of HTTP requests handled by %s.", serviceName),
				Buckets: opts.RequestBuckets,
			},
			[]string{"method", "route", "status"},
		),
	)
	if err != nil {
		return nil, err
	}

	return &Metrics{
		serviceName:     serviceName,
		gatherer:        opts.Gatherer,
		requestsTotal:   requestsTotal,
		requestDuration: requestDuration,
	}, nil
}

// RecordRequest increments the request counter and observes the request duration.
func (metrics *Metrics) RecordRequest(method, route string, status int, duration time.Duration) {
	if metrics == nil {
		return
	}

	method = httpMethodLabel.Value(strings.ToUpper(strings.TrimSpace(method)))
	if route == "" {
		route = "unknown"
	}
	statusLabel := httpStatusLabel.Value(strconv.Itoa(status))
	metrics.requestsTotal.WithLabelValues(method, route, statusLabel).Inc()
	metrics.requestDuration.WithLabelValues(method, route, statusLabel).Observe(duration.Seconds())
}

// Handler returns an HTTP handler that serves the Prometheus metrics endpoint.
func (metrics *Metrics) Handler() http.Handler {
	gatherer := prometheus.Gatherer(prometheus.DefaultGatherer)
	if metrics != nil && metrics.gatherer != nil {
		gatherer = metrics.gatherer
	}
	return promhttp.HandlerFor(gatherer, promhttp.HandlerOpts{})
}

func (opts MetricsOptions) withDefaults() MetricsOptions {
	if opts.Registerer == nil {
		opts.Registerer = prometheus.DefaultRegisterer
	}
	if opts.Gatherer == nil {
		opts.Gatherer = prometheus.DefaultGatherer
	}
	if len(opts.RequestBuckets) == 0 {
		opts.RequestBuckets = prometheus.DefBuckets
	}
	return opts
}

func metricPrefix(serviceName string) string {
	serviceName = strings.TrimSpace(serviceName)
	if serviceName == "" {
		return "service"
	}

	var builder strings.Builder
	for index, runeValue := range serviceName {
		switch {
		case unicode.IsLetter(runeValue), unicode.IsDigit(runeValue):
			builder.WriteRune(unicode.ToLower(runeValue))
		default:
			builder.WriteByte('_')
		}
		if index == 0 && unicode.IsDigit(runeValue) {
			builder.WriteByte('_')
		}
	}

	prefix := strings.Trim(builder.String(), "_")
	if prefix == "" {
		return "service"
	}
	if prefix[0] >= '0' && prefix[0] <= '9' {
		return "service_" + prefix
	}
	return prefix
}

func newHTTPStatusLabel() BoundedLabel {
	values := make([]string, 0, 500)
	for code := 100; code < 600; code++ {
		values = append(values, strconv.Itoa(code))
	}
	return NewBoundedLabel("status", values...)
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

func registerHistogramVec(registerer prometheus.Registerer, collector *prometheus.HistogramVec) (*prometheus.HistogramVec, error) {
	if err := registerer.Register(collector); err != nil {
		alreadyRegistered, ok := err.(prometheus.AlreadyRegisteredError)
		if !ok {
			return nil, err
		}
		existing, ok := alreadyRegistered.ExistingCollector.(*prometheus.HistogramVec)
		if !ok {
			return nil, err
		}
		return existing, nil
	}
	return collector, nil
}
