package resourcecheck

import "errors"

var (
	// ErrNotFound identifies an authoritative resource-not-found response.
	ErrNotFound = errors.New("resource not found")
	// ErrUnavailable identifies a non-transient inability to read metadata.
	ErrUnavailable = errors.New("resource source unavailable")
	// ErrTransientUnavailable identifies a transient inability to read metadata.
	ErrTransientUnavailable = errors.New("resource source temporarily unavailable")
	// ErrUnauthorized identifies missing authority to read the configured
	// account. It is never classified as transient or fail-open.
	ErrUnauthorized = errors.New("resource source authorization unavailable")
)
