package coopcmd

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/stripe/stripe-cli/pkg/coop"
	"github.com/stripe/stripe-cli/pkg/coop/verification"
	"github.com/stripe/stripe-cli/pkg/coop/workflow"
)

func TestCoopVerificationCommandWiresResourceProviderAndRedactedReviewResults(t *testing.T) {
	const (
		accountID  = "acct_command123"
		apiKey     = "sk_test_command_secret"
		checkoutID = "cs_command123"
		sessionID  = "coop_command_verification"
	)
	created := time.Now().UTC().Add(-time.Minute).Truncate(time.Second)
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		require.Equal(t, "Bearer "+apiKey, request.Header.Get("Authorization"))
		switch request.URL.Path {
		case "/v1/account":
			fmt.Fprintf(response, `{"id":%q,"object":"account"}`, accountID)
		case "/v1/checkout/sessions/" + checkoutID:
			fmt.Fprintf(response, `{"id":%q,"object":"checkout.session","created":%d,"livemode":false,"mode":"payment","amount_total":2000,"currency":"usd","payment_status":"paid","status":"complete"}`, checkoutID, created.Unix())
		default:
			http.NotFound(response, request)
		}
	}))
	defer server.Close()

	configFolder := t.TempDir()
	store, err := coop.NewStore(configFolder)
	require.NoError(t, err)
	blueprint, err := coop.LoadBlueprint("one-time-payment")
	require.NoError(t, err)
	session := coop.NewSessionFromBlueprint(blueprint, sessionID, nil, nil)
	require.NoError(t, store.Write(session))

	previousOptions := options
	options = Options{
		ConfigFolder:   func() string { return configFolder },
		TestModeAPIKey: func() (string, error) { return apiKey, nil },
		AccountID:      func() (string, error) { return accountID, nil },
	}
	t.Cleanup(func() { options = previousOptions })

	command := newCoopVerificationCmd().cmd
	command.SetArgs([]string{
		"--session=" + sessionID,
		"--application-resource=checkout=checkout.session:" + checkoutID,
		"--api-base=" + server.URL,
	})
	require.NoError(t, command.Execute())

	loaded, err := store.Read(sessionID)
	require.NoError(t, err)
	var checkoutNode *coop.SessionNode
	checkoutNodeNumber := 0
	nodeNumber := 0
	for stepIndex := range loaded.Steps {
		for nodeIndex := range loaded.Steps[stepIndex].Nodes {
			nodeNumber++
			node := &loaded.Steps[stepIndex].Nodes[nodeIndex]
			if node.Key == "create-checkout-session" {
				checkoutNode = node
				checkoutNodeNumber = nodeNumber
			}
		}
	}
	require.NotNil(t, checkoutNode)
	require.NotNil(t, checkoutNode.VerificationResults)
	require.NoError(t, checkoutNode.VerificationResults.Validate())
	require.Len(t, checkoutNode.VerificationResults.Results, 7)
	for _, result := range checkoutNode.VerificationResults.Results {
		assert.Equal(t, verification.StatusPassed, result.Status, result.ID)
	}
	summaries := verification.AgentSummaries(checkoutNode.VerificationResults)
	require.Len(t, summaries, 7)
	_, err = store.Update(sessionID, func(current *coop.Session) error {
		node, nodeErr := current.NodeByNumber(checkoutNodeNumber)
		if nodeErr != nil {
			return nodeErr
		}
		node.State = coop.NodeActive
		return nil
	})
	require.NoError(t, err)
	reviewResponse, err := workflow.NewService(store).ReportWork(sessionID, checkoutNodeNumber, workflow.ReportWorkInput{File: "server.go"}, false)
	require.NoError(t, err)
	require.True(t, reviewResponse.OK, "resource verification must remain advisory")
	require.Len(t, reviewResponse.VerificationResults, 7)

	durable, err := os.ReadFile(filepath.Join(configFolder, "coop", sessionID+".json"))
	require.NoError(t, err)
	assert.NotContains(t, string(durable), apiKey)
	assert.NotContains(t, string(durable), checkoutID)
}

func TestParseResourceVerificationInputs(t *testing.T) {
	references, err := parseResourceInputs(
		[]string{"intent=payment_intent:pi_parse123"},
		[]string{"checkout=checkout.session:cs_parse123"},
	)
	require.NoError(t, err)
	assert.Len(t, references, 2)
	assert.Equal(t, "explicit", string(references["intent"].Origin))
	assert.Equal(t, "application_record", string(references["checkout"].Origin))

	values, err := parseVerificationValues([]string{`currency="usd"`, "amount=2000", "enabled=true"})
	require.NoError(t, err)
	assert.Len(t, values, 3)

	window, err := parseActionWindow("2026-07-18T20:00:00Z", "2026-07-18T20:10:00Z")
	require.NoError(t, err)
	assert.Equal(t, 10*time.Minute, window.End.Sub(window.Start))
}

func TestParseResourceVerificationInputsRejectsMalformedValues(t *testing.T) {
	_, err := parseResourceInputs([]string{"checkout=unknown:cs_parse123"}, nil)
	assert.Error(t, err)
	_, err = parseResourceInputs([]string{"checkout=checkout.session:cs_parse123"}, []string{"checkout=checkout.session:cs_parse456"})
	assert.ErrorContains(t, err, "more than once")
	_, err = parseVerificationValues([]string{"currency=usd"})
	assert.ErrorContains(t, err, "JSON scalar")
	_, err = parseActionWindow("2026-07-18T20:00:00Z", "")
	assert.ErrorContains(t, err, "supplied together")
}
