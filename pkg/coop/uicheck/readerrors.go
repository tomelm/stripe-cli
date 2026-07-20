package uicheck

import "errors"

// ReadErrorKind classifies a StripeReader failure for the observer's
// pending/failed/unavailable decision (see doc comment on Reader in
// types.go). The zero value, ReadErrorOther, is the safe default for any
// error this package did not itself produce.
type ReadErrorKind int

const (
	// ReadErrorOther is any read failure not covered by the kinds below. The
	// observer must not treat it as authoritative.
	ReadErrorOther ReadErrorKind = iota
	// ReadErrorNotFound is an authoritative 404: the object does not exist.
	ReadErrorNotFound
	// ReadErrorAuth is a 401/403: the credential cannot read this object.
	ReadErrorAuth
	// ReadErrorTransient is a 429, 5xx, or network/timeout failure that may
	// succeed on retry.
	ReadErrorTransient
	// ReadErrorMalformed is a response this package refused to trust: bad
	// JSON, an empty body, trailing data, or a body over the size cap.
	ReadErrorMalformed
)

// String renders the kind for logs and test failure messages.
func (kind ReadErrorKind) String() string {
	switch kind {
	case ReadErrorNotFound:
		return "not_found"
	case ReadErrorAuth:
		return "auth"
	case ReadErrorTransient:
		return "transient"
	case ReadErrorMalformed:
		return "malformed"
	default:
		return "other"
	}
}

// Sentinel errors StripeReader wraps every failure with. ClassifyReadError
// inspects these via errors.Is rather than re-deriving the classification
// from an HTTP status code a second time.
var (
	// errReadNotFound identifies an authoritative resource-not-found response.
	errReadNotFound = errors.New("uicheck: stripe resource not found")
	// errReadAuth identifies a response the credential is not authorized to read.
	errReadAuth = errors.New("uicheck: stripe read not authorized")
	// errReadTransient identifies a rate-limit, server, or network/timeout
	// failure that may succeed if retried.
	errReadTransient = errors.New("uicheck: stripe read temporarily unavailable")
	// errReadMalformed identifies a response this package would not trust.
	errReadMalformed = errors.New("uicheck: stripe read returned malformed data")
	// errReadOther identifies any other non-2xx or unconfigured-reader failure.
	errReadOther = errors.New("uicheck: stripe read failed")
)

// ClassifyReadError maps an error returned by StripeReader.GetObject or
// StripeReader.AccountID to a ReadErrorKind. Errors this package did not
// produce (including nil) classify as ReadErrorOther.
func ClassifyReadError(err error) ReadErrorKind {
	switch {
	case errors.Is(err, errReadNotFound):
		return ReadErrorNotFound
	case errors.Is(err, errReadAuth):
		return ReadErrorAuth
	case errors.Is(err, errReadTransient):
		return ReadErrorTransient
	case errors.Is(err, errReadMalformed):
		return ReadErrorMalformed
	default:
		return ReadErrorOther
	}
}
