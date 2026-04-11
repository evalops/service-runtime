package authmw

import "errors"

var (
	errMissingScopes              = errors.New("missing_required_scopes")
	errTokenVerifierUnavailable   = errors.New("token_verifier_not_configured")
	errAPIKeyValidatorUnavailable = errors.New("api_key_validator_not_configured")
)
