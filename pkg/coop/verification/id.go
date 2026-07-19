package verification

import (
	"fmt"
	"regexp"
)

const maxIDLength = 128

var stableIDPattern = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9._:-]{0,126}[a-z0-9])?$`)

// CheckID is the stable, opaque identifier for a verification check.
//
// Check IDs are part of the durable contract. Producers should persist and
// reuse them rather than deriving them from display text.
type CheckID string

// Validate reports whether id satisfies the stable identifier grammar.
func (id CheckID) Validate() error {
	return validateStableID("check ID", string(id))
}

// ResultID is the stable, opaque identifier for one verification result.
// A producer may emit multiple result IDs for the same CheckID.
type ResultID string

// Validate reports whether id satisfies the stable identifier grammar.
func (id ResultID) Validate() error {
	return validateStableID("result ID", string(id))
}

func validateStableID(kind, value string) error {
	if value == "" {
		return fmt.Errorf("%s is required", kind)
	}
	if len(value) > maxIDLength {
		return fmt.Errorf("%s exceeds %d bytes", kind, maxIDLength)
	}
	if !stableIDPattern.MatchString(value) {
		return fmt.Errorf("%s %q must use lowercase ASCII letters, digits, '.', '_', ':', or '-', and start and end with a letter or digit", kind, value)
	}
	return nil
}
