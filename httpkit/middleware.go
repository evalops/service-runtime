package httpkit

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
)

// WithRequestID is an HTTP middleware that ensures every request has an X-Request-Id header.
func WithRequestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		requestID := request.Header.Get("X-Request-Id")
		if requestID == "" {
			requestID = newRequestID()
		}
		writer.Header().Set("X-Request-Id", requestID)
		ctx := context.WithValue(request.Context(), requestIDContextKey, requestID)
		next.ServeHTTP(writer, request.WithContext(ctx))
	})
}

// WithMaxBodySize returns an HTTP middleware that limits request bodies to maxBytes.
func WithMaxBodySize(maxBytes int64) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
			if maxBytes > 0 {
				request.Body = http.MaxBytesReader(writer, request.Body, maxBytes)
			}
			next.ServeHTTP(writer, request)
		})
	}
}

// WithRequestLogging returns an HTTP middleware that logs each request to the given logger.
func WithRequestLogging(logger *slog.Logger) func(http.Handler) http.Handler {
	if logger == nil {
		logger = slog.Default()
	}

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
			start := time.Now()
			recorder := NewCaptureResponseWriter(writer)
			next.ServeHTTP(recorder, request)

			requestID, _ := RequestIDFromContext(request.Context())
			logger.Info(
				"http request complete",
				"request_id", requestID,
				"method", request.Method,
				"path", request.URL.Path,
				"route", RoutePattern(request),
				"status", recorder.StatusCode(),
				"duration_ms", time.Since(start).Milliseconds(),
			)
		})
	}
}

// WithTelemetry returns an HTTP middleware that creates OpenTelemetry server spans.
func WithTelemetry(service string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return otelhttp.NewMiddleware(service, otelhttp.WithSpanNameFormatter(func(_ string, request *http.Request) string {
			route := RoutePattern(request)
			if route == "" {
				route = request.URL.Path
			}
			if route == "" {
				route = "/"
			}
			return request.Method + " " + route
		}))(next)
	}
}

// RoutePattern returns the chi route pattern for the request, falling back to the URL path.
func RoutePattern(request *http.Request) string {
	route := request.URL.Path
	if routeContext := chi.RouteContext(request.Context()); routeContext != nil {
		if pattern := routeContext.RoutePattern(); pattern != "" {
			route = pattern
		}
	}
	return route
}

func newRequestID() string {
	identifier, err := uuid.NewV7()
	if err == nil {
		return identifier.String()
	}
	return uuid.NewString()
}
