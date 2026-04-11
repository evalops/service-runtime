package httpkit

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/google/uuid"
)

type contextKey string

const requestIDContextKey contextKey = "request_id"

func DecodeJSON(writer http.ResponseWriter, request *http.Request, value any) bool {
	decoder := json.NewDecoder(request.Body)
	if err := decoder.Decode(value); err != nil {
		WriteError(writer, http.StatusBadRequest, ErrorCodeInvalidJSON, "Request body must be valid JSON")
		return false
	}

	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		WriteError(writer, http.StatusBadRequest, ErrorCodeInvalidJSON, "Request body must contain a single JSON value")
		return false
	}

	return true
}

func PathUUID(writer http.ResponseWriter, raw, name string) (uuid.UUID, bool) {
	identifier, err := uuid.Parse(raw)
	if err != nil {
		if strings.TrimSpace(name) == "" {
			name = "id"
		}
		WriteError(writer, http.StatusBadRequest, "invalid_"+name, fmt.Sprintf("%s must be a valid UUID", name))
		return uuid.Nil, false
	}
	return identifier, true
}

func RequireIfMatchVersion(writer http.ResponseWriter, request *http.Request) (int64, bool) {
	header := strings.TrimSpace(request.Header.Get("If-Match"))
	if header == "" {
		WriteError(writer, http.StatusPreconditionRequired, "missing_if_match", "If-Match header is required")
		return 0, false
	}

	version, err := strconv.ParseInt(strings.Trim(header, `"`), 10, 64)
	if err != nil {
		WriteError(writer, http.StatusBadRequest, "invalid_if_match", "If-Match must contain a numeric version")
		return 0, false
	}

	return version, true
}

func ParseInt64Query(request *http.Request, key string, fallback int64) int64 {
	raw := strings.TrimSpace(request.URL.Query().Get(key))
	if raw == "" {
		return fallback
	}

	value, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return fallback
	}

	return value
}

func RequestIDFromContext(ctx context.Context) (string, bool) {
	requestID, ok := ctx.Value(requestIDContextKey).(string)
	return requestID, ok && strings.TrimSpace(requestID) != ""
}

func RequestMetadata(request *http.Request) map[string]any {
	metadata := map[string]any{
		"method": request.Method,
		"path":   request.URL.Path,
	}
	if requestID, ok := RequestIDFromContext(request.Context()); ok {
		metadata["request_id"] = requestID
	}
	return metadata
}
