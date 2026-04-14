package rterrors

import (
	"errors"
	"strings"
)

// Code identifies a stable runtime error category.
type Code string

// Error codes define the stable runtime categories exposed to callers.
const (
	CodeInvalidInput    Code = "invalid_input"
	CodeInvalidJSON     Code = "invalid_json"
	CodeNotFound        Code = "not_found"
	CodeUnauthorized    Code = "unauthorized"
	CodeForbidden       Code = "forbidden"
	CodeConflict        Code = "conflict"
	CodeVersionConflict Code = "version_conflict"
	CodeRateLimited     Code = "rate_limited"
	CodeUnavailable     Code = "unavailable"
	CodeInternal        Code = "internal_error"
)

// Error preserves a machine-readable code while wrapping the original cause.
type Error struct {
	Code    Code
	Message string
	Op      string
	Err     error
}

// New creates a structured runtime error without a wrapped cause.
func New(code Code, message string) *Error {
	return &Error{
		Code:    code,
		Message: strings.TrimSpace(message),
	}
}

// Wrap adds a code and operation to an existing error.
func Wrap(code Code, op string, err error) *Error {
	if err == nil {
		return nil
	}
	return &Error{
		Code: code,
		Op:   strings.TrimSpace(op),
		Err:  err,
	}
}

// E creates a structured runtime error with full control over code, message, operation, and cause.
func E(code Code, op, message string, err error) *Error {
	return &Error{
		Code:    code,
		Op:      strings.TrimSpace(op),
		Message: strings.TrimSpace(message),
		Err:     err,
	}
}

func (e *Error) Error() string {
	if e == nil {
		return ""
	}

	message := e.message()
	switch {
	case e.Op != "" && message != "":
		return e.Op + ": " + message
	case e.Op != "":
		return e.Op
	default:
		return message
	}
}

// Unwrap returns the wrapped cause.
func (e *Error) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Err
}

// Is lets errors.Is match on shared runtime error codes.
func (e *Error) Is(target error) bool {
	if e == nil {
		return target == nil
	}

	targetError, ok := target.(*Error)
	if !ok {
		return false
	}
	if targetError.Code != "" && e.Code == targetError.Code {
		return true
	}
	if targetError.Op != "" && targetError.Code == "" && e.Op == targetError.Op {
		return true
	}
	return false
}

// CodeOf extracts a runtime error code from an error chain.
func CodeOf(err error) Code {
	if err == nil {
		return ""
	}

	var runtimeError *Error
	if errors.As(err, &runtimeError) && runtimeError != nil && runtimeError.Code != "" {
		return runtimeError.Code
	}
	return ""
}

// MessageOf returns the client-facing message for an error.
func MessageOf(err error) string {
	if err == nil {
		return defaultMessage(CodeInternal)
	}

	var runtimeError *Error
	if errors.As(err, &runtimeError) && runtimeError != nil {
		return runtimeError.message()
	}

	message := strings.TrimSpace(err.Error())
	if message != "" {
		return message
	}
	return defaultMessage(CodeInternal)
}

func (e *Error) message() string {
	if e == nil {
		return defaultMessage(CodeInternal)
	}

	if message := strings.TrimSpace(e.Message); message != "" {
		return message
	}
	if e.Err != nil && e.Code != CodeInternal {
		if message := strings.TrimSpace(e.Err.Error()); message != "" {
			return message
		}
	}
	return defaultMessage(e.Code)
}

func defaultMessage(code Code) string {
	switch code {
	case CodeInvalidInput, CodeInvalidJSON:
		return "invalid input"
	case CodeNotFound:
		return "resource not found"
	case CodeUnauthorized:
		return "authentication required"
	case CodeForbidden:
		return "forbidden"
	case CodeConflict, CodeVersionConflict:
		return "request conflicts with current state"
	case CodeRateLimited:
		return "rate limit exceeded"
	case CodeUnavailable:
		return "service unavailable"
	case CodeInternal, "":
		return "internal error"
	default:
		return "request failed"
	}
}
