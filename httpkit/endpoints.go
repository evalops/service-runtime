package httpkit

import (
	"context"
	"net/http"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// HealthHandler returns a simple liveness handler that always responds 200 OK.
func HealthHandler(service string) http.HandlerFunc {
	return func(writer http.ResponseWriter, _ *http.Request) {
		WriteJSON(writer, http.StatusOK, map[string]any{
			"status":  "ok",
			"service": service,
		})
	}
}

// ReadyHandler returns a readiness handler that calls ping and reports 503 on failure.
func ReadyHandler(ping func(context.Context) error) http.HandlerFunc {
	return func(writer http.ResponseWriter, request *http.Request) {
		if ping != nil {
			if err := ping(request.Context()); err != nil {
				WriteError(writer, http.StatusServiceUnavailable, "not_ready", err.Error())
				return
			}
		}
		WriteJSON(writer, http.StatusOK, map[string]any{"status": "ready"})
	}
}

// MetricsHandler returns a Prometheus metrics handler using the default gatherer.
func MetricsHandler() http.Handler {
	return MetricsHandlerFor(prometheus.DefaultGatherer)
}

// MetricsHandlerFor returns a Prometheus metrics handler using the given gatherer.
func MetricsHandlerFor(gatherer prometheus.Gatherer) http.Handler {
	if gatherer == nil {
		gatherer = prometheus.DefaultGatherer
	}
	return promhttp.HandlerFor(gatherer, promhttp.HandlerOpts{})
}
