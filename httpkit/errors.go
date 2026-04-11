package httpkit

import (
	"errors"
	"net/http"
)

const (
	ErrorCodeInvalidJSON     = "invalid_json"
	ErrorCodeNotFound        = "not_found"
	ErrorCodeVersionConflict = "version_conflict"
	ErrorCodeConflict        = "conflict"
	ErrorCodeInternal        = "internal_error"
)

type ErrorResponse struct {
	Error ErrorDetail `json:"error"`
}

type ErrorDetail struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

type StoreErrors struct {
	NotFound        error
	VersionConflict error
	Conflict        error
}

func WriteError(writer http.ResponseWriter, status int, code, message string) {
	WriteJSON(writer, status, ErrorResponse{
		Error: ErrorDetail{
			Code:    code,
			Message: message,
		},
	})
}

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
