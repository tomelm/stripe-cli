package observe

import (
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestConfigRequiresExplicitBoundedSessionInput(t *testing.T) {
	t.Parallel()

	config := defaultTestConfig(StreamListen)
	config.AccountID = "acct_example"
	config.EventTypes = []string{"payment_intent.succeeded"}
	require.NoError(t, config.Validate())

	encoded, err := json.Marshal(config)
	require.NoError(t, err)
	assert.NotContains(t, string(encoded), config.APIKey)
	assert.NotContains(t, fmt.Sprint(config), config.APIKey)
	assert.Contains(t, fmt.Sprint(config), "api_key=[redacted]")

	request := ConnectRequest{APIKey: config.APIKey, Deadline: testStart}
	encoded, err = json.Marshal(request)
	require.NoError(t, err)
	assert.NotContains(t, string(encoded), config.APIKey)
	assert.NotContains(t, fmt.Sprint(request), config.APIKey)

	missingKey := config
	missingKey.APIKey = ""
	assert.ErrorContains(t, missingKey.Validate(), "api_key is required")

	wrongFilters := defaultTestConfig(StreamLogsTail)
	wrongFilters.EventTypes = []string{"charge.succeeded"}
	assert.ErrorContains(t, wrongFilters.Validate(), "does not accept event_types")

	requestFilters := defaultTestConfig(StreamLogsTail)
	requestFilters.RequestMethods = []string{"GET", "POST"}
	requestFilters.RequestPaths = []string{"/v1/invoices/", "/v1/payment_intents"}
	require.NoError(t, requestFilters.Validate())
	wrongRequestStream := defaultTestConfig(StreamListen)
	wrongRequestStream.RequestMethods = []string{"POST"}
	assert.ErrorContains(t, wrongRequestStream.Validate(), "does not accept request filters")
	lowercaseMethod := requestFilters
	lowercaseMethod.RequestMethods = []string{"post"}
	assert.ErrorContains(t, lowercaseMethod.Validate(), "must be uppercase")

	unknownFilter := config
	unknownFilter.EventTypes = []string{"payment_intent.suceeded"}
	assert.ErrorContains(t, unknownFilter.Validate(), "not a supported listen event")

	unbounded := config
	unbounded.StartupTimeout = 24*time.Hour + time.Nanosecond
	assert.ErrorContains(t, unbounded.Validate(), "startup_timeout")

	tooManyEvents := config
	tooManyEvents.EventTypes = make([]string, maxEventTypes+1)
	for index := range tooManyEvents.EventTypes {
		tooManyEvents.EventTypes[index] = fmt.Sprintf("event.%d", index)
	}
	assert.ErrorContains(t, tooManyEvents.Validate(), "event_types exceeds")
}

func TestConfigValidateAcceptsThinAndLegacyListenEvents(t *testing.T) {
	t.Parallel()

	config := defaultTestConfig(StreamListen)
	config.EventTypes = []string{
		"payment_intent.succeeded",
		"v2.billing.pricing_plan_subscription.servicing_activated",
	}
	require.NoError(t, config.Validate())

	misspelled := defaultTestConfig(StreamListen)
	misspelled.EventTypes = []string{"payment_intent.suceeded"}
	assert.ErrorContains(t, misspelled.Validate(), "not a supported listen event")
}

func TestBackoffIsExponentialJitteredAndCapped(t *testing.T) {
	t.Parallel()

	policy := defaultTestConfig(StreamLogsTail).Backoff
	tests := []struct {
		failures uint
		sample   float64
		want     time.Duration
	}{
		{failures: 1, sample: 0.5, want: time.Second},
		{failures: 2, sample: 0.5, want: 2 * time.Second},
		{failures: 4, sample: 0.5, want: 8 * time.Second},
		{failures: 20, sample: 0.999, want: 8 * time.Second},
		{failures: 1, sample: 0, want: 750 * time.Millisecond},
	}
	for _, test := range tests {
		delay, err := policy.Delay(test.failures, test.sample)
		require.NoError(t, err)
		assert.Equal(t, test.want, delay)
	}

	_, err := policy.Delay(0, 0.5)
	assert.Error(t, err)
	_, err = policy.Delay(1, 1)
	assert.Error(t, err)
}

func TestObservationsAreMinimalAndSourceSpecific(t *testing.T) {
	t.Parallel()

	validRequest := Observation{Request: &RequestObservation{
		RequestID: "req_example", Method: "POST", Path: "/v1/payment_intents", Status: 200,
	}}
	require.NoError(t, validRequest.validateFor(StreamLogsTail))
	assert.Error(t, validRequest.validateFor(StreamListen))

	withQuery := validRequest
	requestCopy := *validRequest.Request
	requestCopy.Path = "/v1/payment_intents?secret=value"
	withQuery.Request = &requestCopy
	assert.ErrorContains(t, withQuery.validateFor(StreamLogsTail), "omit query")

	validEvent := Observation{Event: &EventObservation{
		EventID: "evt_example", EventType: "payment_intent.succeeded", AccountID: "acct_example",
	}}
	require.NoError(t, validEvent.validateFor(StreamListen))
	assert.Error(t, validEvent.validateFor(StreamLogsTail))
	assert.Error(t, (Observation{}).validateFor(StreamListen))
}
