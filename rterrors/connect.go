package rterrors

import "connectrpc.com/connect"

// ConnectCode maps runtime errors onto Connect error codes.
func ConnectCode(err error) connect.Code {
	if err == nil {
		return connect.CodeUnknown
	}

	if code := connect.CodeOf(err); code != connect.CodeUnknown {
		return code
	}

	switch CodeOf(err) {
	case CodeInvalidInput, CodeInvalidJSON:
		return connect.CodeInvalidArgument
	case CodeUnauthorized:
		return connect.CodeUnauthenticated
	case CodeForbidden:
		return connect.CodePermissionDenied
	case CodeNotFound:
		return connect.CodeNotFound
	case CodeConflict:
		return connect.CodeAlreadyExists
	case CodeVersionConflict:
		return connect.CodeFailedPrecondition
	case CodeRateLimited:
		return connect.CodeResourceExhausted
	case CodeUnavailable:
		return connect.CodeUnavailable
	case CodeInternal, "":
		return connect.CodeInternal
	default:
		return connect.CodeInternal
	}
}

// ToConnectError converts an error into a Connect error while preserving existing Connect errors.
func ToConnectError(err error) error {
	if err == nil {
		return nil
	}
	if code := connect.CodeOf(err); code != connect.CodeUnknown {
		return err
	}
	return connect.NewError(ConnectCode(err), err)
}
