package rterrors

import (
	"fmt"
	"log/slog"
	"net/http"
	"runtime/debug"
)

// RecoverMiddleware catches panics, logs a stack trace, and returns a structured 500 response.
func RecoverMiddleware(logger *slog.Logger) func(http.Handler) http.Handler {
	if logger == nil {
		logger = slog.Default()
	}

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
			recoveryWriter := &recoveryResponseWriter{ResponseWriter: writer}

			defer func() {
				recovered := recover()
				if recovered == nil {
					return
				}

				logger.Error(
					"http panic recovered",
					"panic", fmt.Sprint(recovered),
					"method", request.Method,
					"path", request.URL.Path,
					"stack", string(debug.Stack()),
				)
				if recoveryWriter.wroteHeader {
					return
				}
				WriteError(recoveryWriter, E(CodeInternal, "http.recover", "internal error", fmt.Errorf("panic: %v", recovered)))
			}()

			next.ServeHTTP(recoveryWriter, request)
		})
	}
}

type recoveryResponseWriter struct {
	http.ResponseWriter
	wroteHeader bool
}

func (writer *recoveryResponseWriter) WriteHeader(statusCode int) {
	writer.wroteHeader = true
	writer.ResponseWriter.WriteHeader(statusCode)
}

func (writer *recoveryResponseWriter) Write(body []byte) (int, error) {
	if !writer.wroteHeader {
		writer.WriteHeader(http.StatusOK)
	}
	return writer.ResponseWriter.Write(body)
}
