package observe

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestClassifyRequestSource(t *testing.T) {
	cases := []struct {
		name          string
		fromDashboard bool
		userAgent     string
		want          RequestSourceClass
	}{
		{"dashboard wins", true, "Stripe/v1 stripe-cli/1.0 AIAgent/claude_code", RequestSourceDashboard},
		{"agent marker", false, "Stripe/v1 stripe-cli/master AIAgent/codex_cli", RequestSourceAgent},
		{"plain cli", false, "Stripe/v1 stripe-cli/master", RequestSourceCLI},
		{"sdk", false, "Stripe/v1 GoBindings/72.0.0", RequestSourceSDK},
		{"empty", false, "", RequestSourceUnknown},
		{"curl", false, "curl/8.0", RequestSourceUnknown},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, classifyRequestSource(tc.fromDashboard, tc.userAgent))
		})
	}
}

func TestBoundRequestLogDetailRetainsOnlyKeys(t *testing.T) {
	var payload requestLogDetailPayload
	raw := `{
		"livemode": false,
		"request": {
			"from_dashboard": false,
			"get_params": {"limit": "3"},
			"post_params": {"currency": "usd", "amount": "1234", "customer": "cus_secret_value"},
			"headers": {"User-Agent": "Stripe/v1 stripe-cli/master"}
		}
	}`
	require.NoError(t, json.Unmarshal([]byte(raw), &payload))

	detail := boundRequestLogDetail(payload)

	assert.Equal(t, []string{"amount", "currency", "customer", "limit"}, detail.ParamKeys)
	assert.Equal(t, RequestSourceCLI, detail.SourceClass)
	assert.False(t, detail.Livemode)

	// The bounded projection must not carry any param values.
	encoded, err := json.Marshal(detail)
	require.NoError(t, err)
	assert.NotContains(t, string(encoded), "cus_secret_value")
	assert.NotContains(t, string(encoded), "1234")
}

func TestClassifyRequestSourceSDKVariants(t *testing.T) {
	cases := []struct {
		name      string
		userAgent string
		want      RequestSourceClass
	}{
		{"python sdk", "Stripe/v1 PythonBindings/9.0", RequestSourceSDK},
		{"v2 sdk prefix", "Stripe/v2 NodeBindings/14.0", RequestSourceSDK},
		{"cli with agent marker classifies as agent", "Stripe/v1 stripe-cli/1.2 AIAgent/claude_code", RequestSourceAgent},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, classifyRequestSource(false, tc.userAgent))
		})
	}
}

func TestClassifyRequestSourceMoreVariants(t *testing.T) {
	cases := []struct {
		name          string
		fromDashboard bool
		userAgent     string
		want          RequestSourceClass
	}{
		{"python sdk", false, "Stripe/v1 PythonBindings/9.0", RequestSourceSDK},
		{"cli with agent marker classifies as agent", false, "Stripe/v1 stripe-cli/1.2 AIAgent/claude_code", RequestSourceAgent},
		{"from_dashboard overrides agent user-agent", true, "Stripe/v1 stripe-cli/1.2 AIAgent/claude_code", RequestSourceDashboard},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, classifyRequestSource(tc.fromDashboard, tc.userAgent))
		})
	}
}

// TestFetchWithRetrySucceedsAfterTransientErrors is intentionally not
// present: fetchWithRetry's backoff constants (fetchRetryBaseDelay =
// 1*time.Second, fetchAttempts = 3) are unexported package consts, not
// injectable fields, so exercising a fail-twice-then-succeed retry through
// fetchWithRetry would cost ~1s of real sleep per test run with no way to
// speed it up. See pkg/coop/observe/provider.go's fetchRetryBaseDelay/
// fetchAttempts consts.

func TestParamPresence(t *testing.T) {
	present, missing := paramPresence(
		[]string{"currency", "amount", "customer", "description", "email"},
		[]string{"amount", "currency"},
		2,
	)
	assert.Equal(t, 2, present)
	assert.Equal(t, []string{"customer", "description"}, missing, "missing list is bounded and sorted")

	present, missing = paramPresence(nil, []string{"anything"}, 4)
	assert.Zero(t, present)
	assert.Empty(t, missing)

	present, missing = paramPresence([]string{"currency"}, []string{"currency"}, 4)
	assert.Equal(t, 1, present)
	assert.Empty(t, missing)
}
