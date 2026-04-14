package httpkit

import (
	"log/slog"
	"net/http"

	"github.com/evalops/service-runtime/rterrors"
)

// RecoverMiddleware returns the shared runtime panic recovery middleware.
func RecoverMiddleware(logger *slog.Logger) func(http.Handler) http.Handler {
	return rterrors.RecoverMiddleware(logger)
}
