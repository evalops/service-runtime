package observability

import (
	"log/slog"
	"net/http"
	"time"

	"github.com/evalops/service-runtime/httpkit"
)

// RequestLoggingMiddleware returns an HTTP middleware that logs each request and records Prometheus metrics.
func RequestLoggingMiddleware(logger *slog.Logger, metrics *Metrics) func(http.Handler) http.Handler {
	if logger == nil {
		logger = slog.Default()
	}

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
			request = withRequestState(request)
			start := time.Now()
			recorder := httpkit.NewCaptureResponseWriter(writer)
			next.ServeHTTP(recorder, request)

			duration := time.Since(start)
			route := httpkit.RoutePattern(request)
			metrics.RecordRequest(request.Method, route, recorder.StatusCode(), duration)

			attributes := []any{
				"request_id", recorder.Header().Get("X-Request-Id"),
				"method", request.Method,
				"path", request.URL.Path,
				"route", route,
				"status", recorder.StatusCode(),
				"duration_ms", duration.Milliseconds(),
			}
			if event, ok := WideEventFromContext(request.Context()); ok {
				attributes = append(attributes, "wide_event", event)
			}
			logger.Info("http request complete", attributes...)
		})
	}
}

