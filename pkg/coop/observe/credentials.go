package observe

import "strings"

// UnavailableLiveCredentials reports that passive observation refused to start
// because the resolved API key is a live-mode key. Passive verification has no
// live-mode opt-in; live sessions stay advisory-unavailable.
const UnavailableLiveCredentials UnavailableReason = "live_credentials"

var liveModeKeyPrefixes = []string{"sk_live_", "rk_live_", "pk_live_"}

// IsLiveModeAPIKey reports whether key is a recognized live-mode Stripe API
// key. Unrecognized formats are not classified as live; they fail closed later
// at stream authentication instead.
func IsLiveModeAPIKey(key string) bool {
	for _, prefix := range liveModeKeyPrefixes {
		if strings.HasPrefix(key, prefix) {
			return true
		}
	}
	return false
}
