package rterrors

import (
	"encoding/json"
	"net/http"

	"connectrpc.com/connect"
)

// ErrorResponse matches the shared EvalOps JSON error shape.
type ErrorResponse struct {
	Error ErrorDetail `json:"error"`
}

// ErrorDetail carries the machine-readable code and display message.
type ErrorDetail struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// HTTPStatus maps runtime errors onto HTTP status codes.
func HTTPStatus(err error) int {
	switch ConnectCode(err) {
	case connect.CodeInvalidArgument:
		return http.StatusBadRequest
	case connect.CodeUnauthenticated:
		return http.StatusUnauthorized
	case connect.CodePermissionDenied:
		return http.StatusForbidden
	case connect.CodeNotFound:
		return http.StatusNotFound
	case connect.CodeAlreadyExists:
		return http.StatusConflict
	case connect.CodeFailedPrecondition:
		return http.StatusPreconditionFailed
	case connect.CodeResourceExhausted:
		return http.StatusTooManyRequests
	case connect.CodeUnavailable:
		return http.StatusServiceUnavailable
	default:
		return http.StatusInternalServerError
	}
}

// WriteError writes the structured JSON representation of an error.
func WriteError(writer http.ResponseWriter, err error) {
	if writer == nil {
		return
	}

	status := HTTPStatus(err)
	code := CodeOf(err)
	if code == "" {
		code = CodeInternal
	}

	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(status)
	_ = json.NewEncoder(writer).Encode(ErrorResponse{
		Error: ErrorDetail{
			Code:    string(code),
			Message: MessageOf(err),
		},
	})
}
