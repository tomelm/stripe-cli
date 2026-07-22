package observe

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/stripe/stripe-cli/pkg/logtailing"
	"github.com/stripe/stripe-cli/pkg/proxy"
	"github.com/stripe/stripe-cli/pkg/websocket"
)

func TestNormalizeRequestBoundsAndRedacts(t *testing.T) {
	payload := logtailing.EventPayload{
		CreatedAt: 1700000000,
		Method:    "post",
		RequestID: "req_123",
		Status:    402,
		URL:       "https://api.stripe.com/v1/payment_intents?client_secret=sk_test_secret",
		Error: logtailing.RedactedError{
			Type:        "card_error",
			Code:        "card_declined",
			DeclineCode: "generic_decline",
			Message:     "secret customer text",
			Param:       "payment_method_data[billing_details][email]",
		},
	}
	fact, ok := Normalize(websocket.DataElement{Data: payload})
	require.True(t, ok)
	require.NotNil(t, fact.Request)
	assert.Equal(t, "POST", fact.Request.Method)
	assert.Equal(t, "/v1/payment_intents", fact.Request.Path)
	assert.Equal(t, "card_declined", fact.Request.ErrorCode)
	assert.NotContains(t, fact.Request.Path, "client_secret")

	typeOfFact := reflect.TypeOf(*fact.Request)
	_, hasMessage := typeOfFact.FieldByName("Message")
	_, hasParams := typeOfFact.FieldByName("Params")
	assert.False(t, hasMessage)
	assert.False(t, hasParams)

	payload.RequestID = strings.Repeat("x", maxIdentifierBytes+1)
	payload.Error.Code = strings.Repeat("x", maxErrorCodeBytes+1)
	fact, ok = Normalize(websocket.DataElement{Data: payload})
	require.True(t, ok)
	assert.Empty(t, fact.Request.RequestID)
	assert.Empty(t, fact.Request.ErrorCode)

	payload.URL = "/" + strings.Repeat("x", maxPathBytes)
	_, ok = Normalize(websocket.DataElement{Data: payload})
	assert.False(t, ok)
}

func TestNormalizeV1AndV2EventDiscoveries(t *testing.T) {
	v1, ok := Normalize(websocket.DataElement{Data: proxy.StripeEvent{
		Created: 1700000000,
		ID:      "evt_123",
		Type:    "payment_intent.succeeded",
		Data: map[string]interface{}{"object": map[string]interface{}{
			"object": "payment_intent", "id": "pi_123",
		}},
	}})
	require.True(t, ok)
	assert.Equal(t, []Discovery{{Type: "payment_intent", ID: "pi_123"}}, v1.Event.Discoveries)

	raw := `{"created":"2026-07-21T12:00:00Z","id":"evt_v2_123","type":"v2.core.account[configuration.merchant].capability_status_updated","related_object":{"id":"acct_123","type":"v2.core.account"}}`
	var payload proxy.V2EventPayload
	require.NoError(t, json.Unmarshal([]byte(raw), &payload))
	v2, ok := Normalize(websocket.DataElement{Data: payload})
	require.True(t, ok)
	assert.Equal(t, "v2.core.account[configuration.merchant].capability_status_updated", v2.Event.Type)
	assert.Equal(t, []Discovery{{Type: "v2.core.account", ID: "acct_123"}}, v2.Event.Discoveries)
}

func TestNormalizeOmitsUnsafeDiscoveriesAndPayloads(t *testing.T) {
	for _, unsafeID := range []string{"sk_test_secret", "pi_123_secret_456", "not-an-id"} {
		fact, ok := Normalize(websocket.DataElement{Data: proxy.StripeEvent{
			ID: "evt_123", Type: "payment_intent.created",
			Data: map[string]interface{}{"object": map[string]interface{}{
				"object": "payment_intent", "id": unsafeID,
			}},
		}})
		require.True(t, ok)
		assert.Empty(t, fact.Event.Discoveries, unsafeID)
	}

	oversized, err := json.Marshal(map[string]interface{}{"type": strings.Repeat("x", maxIdentifierBytes+1), "data": map[string]interface{}{}})
	require.NoError(t, err)
	_, ok := Normalize(websocket.DataElement{Marshaled: string(oversized)})
	assert.False(t, ok)

	_, ok = Normalize(websocket.DataElement{Data: struct{ Secret string }{Secret: "sk_test_secret"}})
	assert.False(t, ok)
	_, ok = Normalize(websocket.DataElement{Marshaled: strings.Repeat(" ", maxPayloadBytes+1)})
	assert.False(t, ok)
}
