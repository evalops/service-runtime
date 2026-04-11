package httpkit

import (
	"context"
	"net/http"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

func HealthHandler(service string) http.HandlerFunc {
	return func(writer http.ResponseWriter, _ *http.Request) {
		WriteJSON(writer, http.StatusOK, map[string]any{
			"status":  "ok",
			"service": service,
		})
	}
}

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

func MetricsHandler() http.Handler {
	return MetricsHandlerFor(prometheus.DefaultGatherer)
}

func MetricsHandlerFor(gatherer prometheus.Gatherer) http.Handler {
	if gatherer == nil {
		gatherer = prometheus.DefaultGatherer
	}
	return promhttp.HandlerFor(gatherer, promhttp.HandlerOpts{})
}
