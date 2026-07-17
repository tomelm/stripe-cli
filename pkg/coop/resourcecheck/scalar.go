package resourcecheck

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"
)

// ScalarKind identifies one JSON scalar type. Object and array values are
// outside this branch's normalized field contract.
type ScalarKind string

const (
	ScalarString ScalarKind = "string"
	ScalarNumber ScalarKind = "number"
	ScalarBool   ScalarKind = "bool"
	ScalarNull   ScalarKind = "null"
)

var jsonNumberPattern = regexp.MustCompile(`^-?(?:0|[1-9][0-9]*)(?:\.[0-9]+)?(?:[eE][+-]?[0-9]+)?$`)

// JSONScalar is a typed canonical JSON scalar. Its representation preserves
// string/number/bool/null distinctions and normalizes equivalent JSON number
// spellings. The zero value is invalid, which keeps it distinct from null.
type JSONScalar struct {
	kind      ScalarKind
	canonical string
}

// String deliberately omits the scalar value.
func (scalar JSONScalar) String() string {
	if scalar.Validate() != nil {
		return "JSONScalar{invalid}"
	}
	return "JSONScalar{kind=" + string(scalar.kind) + " value=[redacted]}"
}

// ParseJSONScalar parses exactly one bounded JSON scalar and canonicalizes it.
func ParseJSONScalar(raw []byte) (JSONScalar, error) {
	if len(raw) == 0 || len(raw) > maxScalarBytes || !utf8.Valid(raw) {
		return JSONScalar{}, errors.New("JSON scalar input is empty or too large")
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return JSONScalar{}, errors.New("JSON scalar is malformed")
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return JSONScalar{}, errors.New("JSON scalar must contain exactly one value")
	}
	switch typed := value.(type) {
	case string:
		return NewStringScalar(typed)
	case json.Number:
		return NewNumberScalar(string(typed))
	case bool:
		return NewBoolScalar(typed), nil
	case nil:
		return NullScalar(), nil
	default:
		return JSONScalar{}, errors.New("JSON objects and arrays are not scalar values")
	}
}

// NewStringScalar constructs a bounded JSON string scalar.
func NewStringScalar(value string) (JSONScalar, error) {
	if !utf8.ValidString(value) {
		return JSONScalar{}, errors.New("JSON string scalar is not valid UTF-8")
	}
	encoded, err := json.Marshal(value)
	if err != nil || len(encoded) > maxScalarBytes {
		return JSONScalar{}, errors.New("JSON string scalar is too large")
	}
	return JSONScalar{kind: ScalarString, canonical: string(encoded)}, nil
}

// NewNumberScalar parses and canonicalizes one JSON number. Equivalent finite
// decimal spellings such as 1, 1.0, and 1e0 compare equal.
func NewNumberScalar(value string) (JSONScalar, error) {
	canonical, err := canonicalJSONNumber(value)
	if err != nil {
		return JSONScalar{}, err
	}
	return JSONScalar{kind: ScalarNumber, canonical: canonical}, nil
}

// NewBoolScalar constructs a JSON boolean scalar.
func NewBoolScalar(value bool) JSONScalar {
	return JSONScalar{kind: ScalarBool, canonical: strconv.FormatBool(value)}
}

// NullScalar constructs the valid JSON null scalar.
func NullScalar() JSONScalar {
	return JSONScalar{kind: ScalarNull, canonical: "null"}
}

// Kind returns the scalar's JSON type.
func (scalar JSONScalar) Kind() ScalarKind {
	return scalar.kind
}

// Equal compares both JSON type and canonical value.
func (scalar JSONScalar) Equal(other JSONScalar) bool {
	return scalar.kind == other.kind && scalar.canonical == other.canonical
}

// CanonicalJSON returns a defensive copy of the canonical JSON encoding.
func (scalar JSONScalar) CanonicalJSON() ([]byte, error) {
	if err := scalar.Validate(); err != nil {
		return nil, err
	}
	return append([]byte(nil), scalar.canonical...), nil
}

// Validate rejects the invalid zero value and malformed internal state.
func (scalar JSONScalar) Validate() error {
	switch scalar.kind {
	case ScalarString:
		parsed, err := ParseJSONScalar([]byte(scalar.canonical))
		if err != nil || parsed.kind != ScalarString || parsed.canonical != scalar.canonical {
			return errors.New("invalid canonical JSON string scalar")
		}
	case ScalarNumber:
		canonical, err := canonicalJSONNumber(scalar.canonical)
		if err != nil || canonical != scalar.canonical {
			return errors.New("invalid canonical JSON number scalar")
		}
	case ScalarBool:
		if scalar.canonical != "true" && scalar.canonical != "false" {
			return errors.New("invalid canonical JSON boolean scalar")
		}
	case ScalarNull:
		if scalar.canonical != "null" {
			return errors.New("invalid canonical JSON null scalar")
		}
	default:
		return errors.New("JSON scalar kind is invalid")
	}
	if len(scalar.canonical) == 0 || len(scalar.canonical) > maxScalarBytes {
		return errors.New("canonical JSON scalar is empty or too large")
	}
	return nil
}

// MarshalJSON emits the canonical scalar rather than an object envelope.
func (scalar JSONScalar) MarshalJSON() ([]byte, error) {
	return scalar.CanonicalJSON()
}

// UnmarshalJSON accepts only a single JSON scalar.
func (scalar *JSONScalar) UnmarshalJSON(raw []byte) error {
	parsed, err := ParseJSONScalar(raw)
	if err != nil {
		return err
	}
	*scalar = parsed
	return nil
}

func canonicalJSONNumber(value string) (string, error) {
	if len(value) == 0 || len(value) > maxScalarBytes || !jsonNumberPattern.MatchString(value) {
		return "", errors.New("JSON number scalar is malformed or too large")
	}
	negative := strings.HasPrefix(value, "-")
	unsigned := strings.TrimPrefix(value, "-")

	exponent := int64(0)
	if index := strings.IndexAny(unsigned, "eE"); index >= 0 {
		parsed, err := strconv.ParseInt(unsigned[index+1:], 10, 32)
		if err != nil {
			return "", errors.New("JSON number exponent is out of bounds")
		}
		exponent = parsed
		unsigned = unsigned[:index]
	}
	integer := unsigned
	fraction := ""
	if index := strings.IndexByte(unsigned, '.'); index >= 0 {
		integer = unsigned[:index]
		fraction = unsigned[index+1:]
	}
	digits := strings.TrimLeft(integer+fraction, "0")
	if digits == "" {
		return "0", nil
	}
	exponent -= int64(len(fraction))
	for strings.HasSuffix(digits, "0") {
		digits = strings.TrimSuffix(digits, "0")
		exponent++
	}

	signBytes := int64(0)
	if negative {
		signBytes = 1
	}
	digitCount := int64(len(digits))
	var outputLength int64
	switch decimalPoint := digitCount + exponent; {
	case exponent >= 0:
		outputLength = signBytes + digitCount + exponent
	case decimalPoint > 0:
		outputLength = signBytes + digitCount + 1
	default:
		outputLength = signBytes + 2 + (-decimalPoint) + digitCount
	}
	if outputLength > maxScalarBytes {
		return "", errors.New("canonical JSON number scalar is too large")
	}

	var canonical string
	decimalPoint := digitCount + exponent
	switch {
	case exponent >= 0:
		canonical = digits + strings.Repeat("0", int(exponent))
	case decimalPoint > 0:
		position := int(decimalPoint)
		canonical = digits[:position] + "." + digits[position:]
	default:
		canonical = "0." + strings.Repeat("0", int(-decimalPoint)) + digits
	}
	if negative {
		canonical = "-" + canonical
	}
	return canonical, nil
}
