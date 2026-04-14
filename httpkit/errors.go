package httpkit

import (
	"errors"
	"net/http"

	"github.com/evalops/service-runtime/rterrors"
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
type ErrorResponse = rterrors.ErrorResponse

// ErrorDetail carries the error code and human-readable message.
type ErrorDetail = rterrors.ErrorDetail

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

// WriteMappedError writes a structured error response using the shared runtime error mapping.
func WriteMappedError(writer http.ResponseWriter, err error) {
	rterrors.WriteError(writer, err)
}

// WriteStoreError maps a store error to an appropriate HTTP error response.
func WriteStoreError(writer http.ResponseWriter, err error, storeErrors StoreErrors) {
	switch {
	case storeErrors.NotFound != nil && errors.Is(err, storeErrors.NotFound):
		WriteMappedError(writer, rterrors.E(rterrors.CodeNotFound, "store.read", err.Error(), err))
	case storeErrors.VersionConflict != nil && errors.Is(err, storeErrors.VersionConflict):
		WriteMappedError(writer, rterrors.E(rterrors.CodeVersionConflict, "store.write", "resource version does not match If-Match", err))
	case storeErrors.Conflict != nil && errors.Is(err, storeErrors.Conflict):
		WriteMappedError(writer, rterrors.E(rterrors.CodeConflict, "store.write", err.Error(), err))
	default:
		WriteMappedError(writer, rterrors.E(rterrors.CodeInternal, "store.write", err.Error(), err))
	}
}
