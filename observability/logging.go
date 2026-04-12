package observability

import (
	"bytes"
	"log/slog"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
)

func RequestLoggingMiddleware(logger *slog.Logger, metrics *Metrics) func(http.Handler) http.Handler {
	if logger == nil {
		logger = slog.Default()
	}

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
			request = withRequestState(request)
			start := time.Now()
			recorder := newResponseRecorder(writer)
			next.ServeHTTP(recorder, request)

			duration := time.Since(start)
			route := routePattern(request)
			metrics.RecordRequest(request.Method, route, recorder.statusCode, duration)

			attributes := []any{
				"request_id", recorder.Header().Get("X-Request-Id"),
				"method", request.Method,
				"path", request.URL.Path,
				"route", route,
				"status", recorder.statusCode,
				"duration_ms", duration.Milliseconds(),
			}
			if event, ok := WideEventFromContext(request.Context()); ok {
				attributes = append(attributes, "wide_event", event)
			}
			logger.Info("http request complete", attributes...)
		})
	}
}

func routePattern(request *http.Request) string {
	route := request.URL.Path
	if routeContext := chi.RouteContext(request.Context()); routeContext != nil {
		if pattern := routeContext.RoutePattern(); pattern != "" {
			route = pattern
		}
	}
	return route
}

type responseRecorder struct {
	http.ResponseWriter
	statusCode  int
	wroteHeader bool
	body        bytes.Buffer
}

func newResponseRecorder(writer http.ResponseWriter) *responseRecorder {
	return &responseRecorder{
		ResponseWriter: writer,
		statusCode:     http.StatusOK,
	}
}

func (writer *responseRecorder) Header() http.Header {
	return writer.ResponseWriter.Header()
}

func (writer *responseRecorder) WriteHeader(statusCode int) {
	if writer.wroteHeader {
		return
	}
	writer.wroteHeader = true
	writer.statusCode = statusCode
	writer.ResponseWriter.WriteHeader(statusCode)
}

func (writer *responseRecorder) Write(body []byte) (int, error) {
	if !writer.wroteHeader {
		writer.WriteHeader(http.StatusOK)
	}
	_, _ = writer.body.Write(body)
	return writer.ResponseWriter.Write(body)
}

func (writer *responseRecorder) BodyBytes() []byte {
	return append([]byte(nil), writer.body.Bytes()...)
}
