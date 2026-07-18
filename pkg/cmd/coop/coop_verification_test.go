package coopcmd

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/stripe/stripe-cli/pkg/coop"
	"github.com/stripe/stripe-cli/pkg/coop/verification"
	verificationruntime "github.com/stripe/stripe-cli/pkg/coop/verification/runtime"
	"github.com/stripe/stripe-cli/pkg/stripe"
)

func TestCoopVerifyIsExplicitlyLaunchable(t *testing.T) {
	command := newCoopCmd().cmd
	verify, _, err := command.Find([]string{"verify"})
	require.NoError(t, err)
	assert.NotNil(t, verify.Flags().Lookup("session"))
	assert.NotNil(t, verify.Flags().Lookup("api-base"))
	assert.NotNil(t, verify.Flags().Lookup("no-wss"))
}

func TestRunCoopVerificationPersistsUnavailableWithoutCredentials(t *testing.T) {
	previousOptions := options
	options = Options{}
	t.Cleanup(func() { options = previousOptions })

	store, err := coop.NewStoreAt(t.TempDir())
	require.NoError(t, err)
	blueprint, err := coop.LoadBlueprint("one-time-payment")
	require.NoError(t, err)
	session := coop.NewSessionFromBlueprint(blueprint, "verify_missing_credentials", nil, nil)
	require.NoError(t, store.Write(session))
	metadata := verificationruntime.SessionMetadata(session)
	requestNode := firstNodeWithRequest(metadata)
	require.Positive(t, requestNode)

	done := make(chan error, 1)
	go func() {
		done <- runCoopVerification(context.Background(), store, session.ID, stripe.DefaultAPIBaseURL, false, nil, nil)
	}()
	require.Eventually(t, func() bool {
		loaded, readErr := store.Read(session.ID)
		if readErr != nil {
			return false
		}
		node, nodeErr := loaded.NodeByNumber(requestNode)
		if nodeErr != nil || node.VerificationResults == nil {
			return false
		}
		for _, result := range node.VerificationResults.Results {
			if result.ID == "passive.request" {
				return result.Status == verification.StatusUnavailable &&
					result.FailureDomain == verification.FailureDomainCollector
			}
		}
		return false
	}, 2*time.Second, 5*time.Millisecond)

	_, err = store.Update(session.ID, func(current *coop.Session) error {
		current.Status = coop.SessionCompleted
		return nil
	})
	require.NoError(t, err)
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for verification command")
	}
}

func TestFallbackStartupOwnsVerificationForExplicitSession(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	started := make(chan string, 1)
	stopped := make(chan struct{})
	previousRunner := runOwnedCoopVerification
	runOwnedCoopVerification = func(ctx context.Context, sessionID string) error {
		started <- sessionID
		<-ctx.Done()
		close(stopped)
		return nil
	}
	t.Cleanup(func() { runOwnedCoopVerification = previousRunner })

	run := &coopRunCmd{language: "node"}
	err := run.runFallbackWithCommand("/stripe", "one-time-payment", func(session *coop.Session) (string, func(), error) {
		require.NotNil(t, session)
		return "true", nil, nil
	})
	require.NoError(t, err)

	select {
	case sessionID := <-started:
		assert.Contains(t, sessionID, "coop_")
	case <-time.After(2 * time.Second):
		t.Fatal("verification process was not started")
	}
	select {
	case <-stopped:
	case <-time.After(2 * time.Second):
		t.Fatal("verification process was not stopped with its owner")
	}
}

func firstNodeWithRequest(session verificationruntime.Session) int {
	for _, node := range session.Nodes {
		if len(node.Requests) > 0 {
			return node.Number
		}
	}
	return 0
}
