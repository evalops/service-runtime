// Package httpkit provides HTTP handler helpers, middleware, and JSON request/response utilities.
package httpkit

import (
	"context"
	"net/http"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// HealthHandler returns an HTTP handler that responds with the service health status.
func HealthHandler(service string) http.HandlerFunc {
	return func(writer http.ResponseWriter, _ *http.Request) {
		WriteJSON(writer, http.StatusOK, map[string]any{
			"status":  "ok",
			"service": service,
		})
	}
}

// ReadyHandler returns an HTTP handler that pings a dependency and reports readiness.
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

// MetricsHandler returns an HTTP handler that serves default Prometheus metrics.
func MetricsHandler() http.Handler {
	return MetricsHandlerFor(prometheus.DefaultGatherer)
}

// MetricsHandlerFor returns an HTTP handler that serves Prometheus metrics from the given gatherer.
func MetricsHandlerFor(gatherer prometheus.Gatherer) http.Handler {
	if gatherer == nil {
		gatherer = prometheus.DefaultGatherer
	}
	return promhttp.HandlerFor(gatherer, promhttp.HandlerOpts{})
}
