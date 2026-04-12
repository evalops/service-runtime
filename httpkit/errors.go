package httpkit

import (
	"errors"
	"net/http"
)

// ErrorCodeInvalidJSON and related constants are standard error codes for HTTP error responses.
const (
	ErrorCodeInvalidJSON     = "invalid_json"
	ErrorCodeNotFound        = "not_found"
	ErrorCodeVersionConflict = "version_conflict"
	ErrorCodeConflict        = "conflict"
	ErrorCodeInternal        = "internal_error"
)

// ErrorResponse is the JSON body shape for all error responses.
type ErrorResponse struct {
	Error ErrorDetail `json:"error"`
}

// ErrorDetail carries the error code and human-readable message.
type ErrorDetail struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// StoreErrors maps domain store sentinel errors to HTTP status codes.
type StoreErrors struct {
	NotFound        error
	VersionConflict error
	Conflict        error
}

// WriteError writes a JSON error response with the given status, code, and message.
func WriteError(writer http.ResponseWriter, status int, code, message string) {
	WriteJSON(writer, status, ErrorResponse{
		Error: ErrorDetail{
			Code:    code,
			Message: message,
		},
	})
}

// WriteStoreError maps a store error to an appropriate HTTP error response.
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
