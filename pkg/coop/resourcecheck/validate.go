package resourcecheck

import (
	"errors"
	"regexp"
	"strings"
	"time"
)

const (
	// MaxListLimit is the largest page a bounded list lookup may request.
	MaxListLimit = 100
	// MaxCreationWindow bounds the node action interval used by the
	// created-in-window check.
	MaxCreationWindow = 24 * time.Hour
	// readTimeout bounds every individual Stripe read.
	readTimeout = 5 * time.Second
)

var (
	accountIDPattern        = regexp.MustCompile(`^acct_[A-Za-z0-9]{1,64}$`)
	resourceIDPattern       = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_]{1,254}$`)
	resourceIDSuffixPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_]{1,252}[A-Za-z0-9]$`)

	placeholderTokens = map[string]struct{}{
		"example":     {},
		"nil":         {},
		"none":        {},
		"null":        {},
		"pending":     {},
		"placeholder": {},
		"replace":     {},
		"sample":      {},
		"tbd":         {},
		"todo":        {},
		"unknown":     {},
		"your":        {},
	}
)

func validateResourceRef(ref ResourceRef) error {
	descriptor, ok := supportedResourceDescriptors[ref.Type]
	if !ok {
		return errors.New("resource type is not supported")
	}
	if !resourceIDPattern.MatchString(ref.ID) {
		return errors.New("resource ID is invalid")
	}
	compatible := false
	matchedPrefix := ""
	for _, prefix := range descriptor.idPrefixes {
		if strings.HasPrefix(ref.ID, prefix) {
			compatible = true
			matchedPrefix = prefix
			break
		}
	}
	if !compatible {
		return errors.New("resource ID prefix is incompatible with resource type")
	}
	suffix := strings.TrimPrefix(ref.ID, matchedPrefix)
	if !resourceIDSuffixPattern.MatchString(suffix) {
		return errors.New("resource ID suffix is invalid")
	}
	if hasPlaceholderID(strings.ToLower(suffix)) {
		return errors.New("resource ID is a placeholder")
	}
	return nil
}

// hasPlaceholderID rejects IDs whose separator-delimited segments exactly
// match a placeholder token. Prefix matching is deliberately avoided: real
// Stripe ID suffixes are random and can legitimately start with a token.
func hasPlaceholderID(suffix string) bool {
	segments := strings.FieldsFunc(suffix, func(character rune) bool {
		return character == '_' || character == '-'
	})
	for _, segment := range segments {
		if _, forbidden := placeholderTokens[segment]; forbidden {
			return true
		}
	}
	return false
}
