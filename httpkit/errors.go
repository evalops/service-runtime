package httpkit

import (
	"errors"
	"net/http"
)

// ErrorCodeInvalidJSON, ErrorCodeNotFound, ErrorCodeVersionConflict, ErrorCodeConflict, and
// ErrorCodeInternal are standard error codes returned in JSON error responses.
const (
	ErrorCodeInvalidJSON     = "invalid_json"
	ErrorCodeNotFound        = "not_found"
	ErrorCodeVersionConflict = "version_conflict"
	ErrorCodeConflict        = "conflict"
	ErrorCodeInternal        = "internal_error"
)

// ErrorResponse is the top-level JSON envelope for error responses.
type ErrorResponse struct {
	Error ErrorDetail `json:"error"`
}

// ErrorDetail holds the code and human-readable message for an error response.
type ErrorDetail struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// StoreErrors maps domain store sentinel errors to HTTP error codes.
type StoreErrors struct {
	NotFound        error
	VersionConflict error
	Conflict        error
}

// WriteError writes a JSON error response with the given HTTP status, code, and message.
func WriteError(writer http.ResponseWriter, status int, code, message string) {
	WriteJSON(writer, status, ErrorResponse{
		Error: ErrorDetail{
			Code:    code,
			Message: message,
		},
	})
}

// WriteStoreError maps a store error to the appropriate HTTP status and writes a JSON error response.
func WriteStoreError(writer http.ResponseWriter, err error, storeErrors StoreErrors) {
	switch {
	case storeErrors.NotFound != nil && errors.Is(err, storeErrors.NotFound):
		WriteError(writer, http.StatusNotFound, ErrorCodeNotFound, err.Error())
	case storeErrors.VersionConflict != nil && errors.Is(err, storeErrors.VersionConflict):
		WriteError(writer, http.StatusPreconditionFailed, ErrorCodeVersionConflict, "resource version does not match If-Match")
	case storeErrors.Conflict != nil && errors.Is(err, storeErrors.Conflict):
		WriteError(writer, http.StatusConflict, ErrorCodeConflict, err.Error())
	default:
		WriteError(writer, http.StatusInternalServerError, ErrorCodeInternal, err.Error())
	}
}
