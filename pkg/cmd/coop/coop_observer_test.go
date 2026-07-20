package coopcmd

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/stripe/stripe-cli/pkg/coop"
	"github.com/stripe/stripe-cli/pkg/coop/observe"
	"github.com/stripe/stripe-cli/pkg/coop/tui"
	"github.com/stripe/stripe-cli/pkg/coop/verification"
	verificationruntime "github.com/stripe/stripe-cli/pkg/coop/verification/runtime"
)

func TestCoopVerifyCommandRemoved(t *testing.T) {
	cmd := newCoopCmd().cmd

	for _, sub := range cmd.Commands() {
		assert.NotEqual(t, "verify", sub.Name())
	}

	found, _, err := cmd.Find([]string{"verify"})
	if err == nil {
		require.NotNil(t, found)
		assert.NotEqual(t, "verify", found.Name())
	}
}

func TestObserveSessionPersistsUnavailableWithoutCredentials(t *testing.T) {
	previousOptions := options
	options = Options{APIKey: func() (string, error) { return "", nil }}
	t.Cleanup(func() { options = previousOptions })

	store, err := coop.NewStoreAt(t.TempDir())
	require.NoError(t, err)
	session := writeObservedSession(t, store, "observe_missing_credentials")
	requestNode := firstNodeWithRequest(verificationruntime.SessionMetadata(session))
	require.Positive(t, requestNode)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- observeSession(ctx, store, session.ID) }()

	require.Eventually(t, func() bool {
		result, ok := loadPassiveRequestResult(store, session.ID, requestNode)
		return ok &&
			result.Status == verification.StatusUnavailable &&
			result.FailureDomain == verification.FailureDomainCollector &&
			evidenceValue(result, "reason") == string(observe.UnavailableMissingCredentials)
	}, 2*time.Second, 5*time.Millisecond)

	cancel()
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for observation to stop")
	}
}

func TestObserveSessionMarksLiveCredentialsUnavailable(t *testing.T) {
	const liveKey = "sk_live_abc"
	previousOptions := options
	options = Options{APIKey: func() (string, error) { return liveKey, nil }}
	t.Cleanup(func() { options = previousOptions })

	storeDir := t.TempDir()
	store, err := coop.NewStoreAt(storeDir)
	require.NoError(t, err)
	session := writeObservedSession(t, store, "observe_live_credentials")
	requestNode := firstNodeWithRequest(verificationruntime.SessionMetadata(session))
	require.Positive(t, requestNode)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- observeSession(ctx, store, session.ID) }()

	require.Eventually(t, func() bool {
		result, ok := loadPassiveRequestResult(store, session.ID, requestNode)
		return ok &&
			result.Status == verification.StatusUnavailable &&
			result.FailureDomain == verification.FailureDomainCollector &&
			evidenceValue(result, "reason") == string(observe.UnavailableLiveCredentials) &&
			strings.Contains(result.Detail, "live mode")
	}, 2*time.Second, 5*time.Millisecond)

	cancel()
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for observation to stop")
	}

	// The live-mode key must never reach durable session state.
	raw, err := os.ReadFile(filepath.Join(storeDir, session.ID+".json"))
	require.NoError(t, err)
	assert.NotContains(t, string(raw), liveKey)
}

func TestPassiveProviderConfigPolicy(t *testing.T) {
	previousOptions := options
	t.Cleanup(func() { options = previousOptions })

	keyFunc := func(key string) func() (string, error) {
		return func() (string, error) { return key, nil }
	}

	tests := []struct {
		name       string
		apiKey     func() (string, error)
		wantReason observe.UnavailableReason
		wantKey    string
	}{
		{name: "nil resolver", apiKey: nil, wantReason: observe.UnavailableMissingCredentials},
		{name: "resolver error", apiKey: func() (string, error) { return "", errors.New("no profile") }, wantReason: observe.UnavailableMissingCredentials},
		{name: "empty key", apiKey: keyFunc(""), wantReason: observe.UnavailableMissingCredentials},
		{name: "live secret key", apiKey: keyFunc("sk_live_123"), wantReason: observe.UnavailableLiveCredentials},
		{name: "live restricted key", apiKey: keyFunc("rk_live_123"), wantReason: observe.UnavailableLiveCredentials},
		{name: "live publishable key", apiKey: keyFunc("pk_live_123"), wantReason: observe.UnavailableLiveCredentials},
		{name: "test secret key", apiKey: keyFunc("sk_test_123"), wantKey: "sk_test_123"},
		{name: "test restricted key", apiKey: keyFunc("rk_test_123"), wantKey: "rk_test_123"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			options = Options{APIKey: tt.apiKey}

			config := passiveProviderConfig()

			assert.Equal(t, tt.wantReason, config.UnavailableReason)
			// Live-mode keys must not be carried on the config at all.
			assert.Equal(t, tt.wantKey, config.APIKey)
		})
	}
}

func TestSessionObserverFactoryStartsAndStopsObservation(t *testing.T) {
	started := make(chan string, 1)
	exited := make(chan struct{})
	previousRunner := runSessionObservation
	runSessionObservation = func(ctx context.Context, sessionID string) error {
		started <- sessionID
		<-ctx.Done()
		close(exited)
		return nil
	}
	t.Cleanup(func() { runSessionObservation = previousRunner })

	stop := newSessionObserverFactory()("coop_x")

	select {
	case sessionID := <-started:
		assert.Equal(t, "coop_x", sessionID)
	case <-time.After(2 * time.Second):
		t.Fatal("observation was not started")
	}

	stopReturned := make(chan struct{})
	go func() {
		stop()
		close(stopReturned)
	}()
	select {
	case <-stopReturned:
	case <-time.After(2 * time.Second):
		t.Fatal("stop did not return")
	}
	select {
	case <-exited:
	default:
		t.Fatal("stop returned before observation shut down")
	}

	secondStop := make(chan struct{})
	go func() {
		stop()
		close(secondStop)
	}()
	select {
	case <-secondStop:
	case <-time.After(2 * time.Second):
		t.Fatal("second stop call did not return")
	}
}

func TestLauncherFallbackDoesNotOwnObservation(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	previousOptions := options
	options = Options{}
	t.Cleanup(func() { options = previousOptions })
	observationCalls := recordObservationCalls(t)

	rc := &coopRunCmd{language: "node"}
	err := rc.runFallbackWithCommand("/stripe", "one-time-payment", func(session *coop.Session) (string, func(), error) {
		require.NotNil(t, session)
		return "true", nil, nil
	})
	require.NoError(t, err)

	// Observation ownership moved into the TUI process (`stripe coop join`);
	// the launcher must not start it, even asynchronously.
	time.Sleep(50 * time.Millisecond)
	assert.Zero(t, observationCalls.Load())
}

func TestLauncherNewTmuxDoesNotOwnObservation(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	previousOptions := options
	options = Options{}
	t.Cleanup(func() { options = previousOptions })
	observationCalls := recordObservationCalls(t)

	// Let new-session succeed so the pane-0 command is captured, then fail the
	// split so the launcher returns before the real `tmux attach-session` exec.
	var tmuxCalls [][]string
	previousRunTmux := runTmux
	runTmux = func(args ...string) error {
		tmuxCalls = append(tmuxCalls, append([]string(nil), args...))
		switch args[0] {
		case "has-session":
			return errors.New("session not found")
		case "split-window":
			return errors.New("split failed")
		default:
			return nil
		}
	}
	t.Cleanup(func() { runTmux = previousRunTmux })

	rc := &coopRunCmd{language: "node"}
	err := rc.runInNewTmuxWithCommand("/stripe", "one-time-payment", func(session *coop.Session) (string, func(), error) {
		require.NotNil(t, session)
		return "agent", nil, nil
	})
	require.Error(t, err)

	// Pane 0 runs `stripe coop join <session>`: the TUI process owns observation.
	newSessionCall := findTmuxCall(tmuxCalls, "new-session")
	require.NotNil(t, newSessionCall)
	assert.Contains(t, newSessionCall[len(newSessionCall)-1], "coop join")

	time.Sleep(50 * time.Millisecond)
	assert.Zero(t, observationCalls.Load())
}

func TestLauncherTmuxSplitFailureDoesNotStartObservation(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	previousOptions := options
	options = Options{}
	t.Cleanup(func() { options = previousOptions })
	observationCalls := recordObservationCalls(t)

	splitErr := errors.New("split failed")
	previousRunTmux := runTmux
	runTmux = func(args ...string) error {
		if args[0] == "split-window" {
			return splitErr
		}
		return nil
	}
	t.Cleanup(func() { runTmux = previousRunTmux })

	rc := &coopRunCmd{language: "node"}
	err := rc.runInTmuxSplitWithCommand("/stripe", "one-time-payment", func(session *coop.Session) (string, func(), error) {
		require.NotNil(t, session)
		return "agent", nil, nil
	})
	require.ErrorIs(t, err, splitErr)

	// The old launcher started observation before splitting; a failed split
	// must now leave observation entirely unstarted.
	time.Sleep(50 * time.Millisecond)
	assert.Zero(t, observationCalls.Load())
}

func TestJoinLaunchesObservingTUI(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	previousOptions := options
	options = Options{}
	t.Cleanup(func() { options = previousOptions })

	store, err := coop.NewStore(coopConfigFolder())
	require.NoError(t, err)
	require.NoError(t, store.Write(&coop.Session{
		ID:        "coop_join_observer",
		Blueprint: "one-time-payment",
		Status:    coop.SessionActive,
	}))

	var tuiSessionID string
	previousRunTUI := runCoopTUI
	runCoopTUI = func(store *coop.Store, sessionID string) error {
		tuiSessionID = sessionID
		return nil
	}
	t.Cleanup(func() { runCoopTUI = previousRunTUI })

	waitingCalled := false
	var waitingIDs map[string]bool
	previousRunTUIWaiting := runCoopTUIWaiting
	runCoopTUIWaiting = func(store *coop.Store, existingSessionIDs map[string]bool) error {
		waitingCalled = true
		waitingIDs = existingSessionIDs
		return nil
	}
	t.Cleanup(func() { runCoopTUIWaiting = previousRunTUIWaiting })

	jc := newCoopJoinCmd()
	require.NoError(t, jc.runJoinCmd(jc.cmd, []string{"coop_join_observer"}))
	assert.Equal(t, "coop_join_observer", tuiSessionID)
	assert.False(t, waitingCalled)

	waitCmd := newCoopJoinCmd()
	waitCmd.wait = true
	require.NoError(t, waitCmd.runJoinCmd(waitCmd.cmd, nil))
	assert.True(t, waitingCalled)
	assert.True(t, waitingIDs["coop_join_observer"])
}

func TestCoopTUIOptionsIncludeObserver(t *testing.T) {
	// The tui package keeps Model fields unexported, so the installed observer
	// cannot be inspected from this package; observer behavior itself is
	// covered by TestSessionObserverFactoryStartsAndStopsObservation and the
	// TUI launch wiring by TestJoinLaunchesObservingTUI. Guard the production
	// option set shape here: sandbox claim URL plus session observer, both
	// applicable to a model without panicking.
	opts := coopTUIOptions()
	require.Len(t, opts, 2)

	model := &tui.Model{}
	for _, opt := range opts {
		opt(model)
	}
}

// recordObservationCalls swaps runSessionObservation for a counter and
// restores it on cleanup.
func recordObservationCalls(t *testing.T) *atomic.Int32 {
	t.Helper()
	var calls atomic.Int32
	previousRunner := runSessionObservation
	runSessionObservation = func(ctx context.Context, sessionID string) error {
		calls.Add(1)
		return nil
	}
	t.Cleanup(func() { runSessionObservation = previousRunner })
	return &calls
}

// writeObservedSession persists a fresh one-time-payment session fixture.
func writeObservedSession(t *testing.T, store *coop.Store, sessionID string) *coop.Session {
	t.Helper()
	blueprint, err := coop.LoadBlueprint("one-time-payment")
	require.NoError(t, err)
	session := coop.NewSessionFromBlueprint(blueprint, sessionID, nil, nil)
	require.NoError(t, store.Write(session))
	return session
}

// loadPassiveRequestResult reads the persisted passive.request result for a
// node, reporting ok=false while it has not been written yet.
func loadPassiveRequestResult(store *coop.Store, sessionID string, nodeNumber int) (verification.Result, bool) {
	loaded, err := store.Read(sessionID)
	if err != nil {
		return verification.Result{}, false
	}
	node, err := loaded.NodeByNumber(nodeNumber)
	if err != nil || node.VerificationResults == nil {
		return verification.Result{}, false
	}
	for _, result := range node.VerificationResults.Results {
		if result.ID == "passive.request" {
			return result, true
		}
	}
	return verification.Result{}, false
}

func evidenceValue(result verification.Result, key string) string {
	for _, evidence := range result.Evidence {
		if evidence.Key == key {
			return evidence.Value
		}
	}
	return ""
}

func firstNodeWithRequest(session verificationruntime.Session) int {
	for _, node := range session.Nodes {
		if len(node.Requests) > 0 {
			return node.Number
		}
	}
	return 0
}

func writeForeignObserverLease(t *testing.T, storeDir, sessionID string, pid int) string {
	t.Helper()
	leasePath := filepath.Join(storeDir, "coop", sessionID+".json.observer")
	if _, err := os.Stat(filepath.Dir(leasePath)); err != nil {
		leasePath = filepath.Join(storeDir, sessionID+".json.observer")
	}
	content := fmt.Sprintf("%d\n%d\n", pid, time.Now().UnixNano())
	require.NoError(t, os.WriteFile(leasePath, []byte(content), 0600))
	return leasePath
}

func TestObserveSessionStandsByWhileLeaseHeld(t *testing.T) {
	previousOptions := options
	options = Options{APIKey: func() (string, error) { return "", nil }}
	t.Cleanup(func() { options = previousOptions })

	storeDir := t.TempDir()
	store, err := coop.NewStoreAt(storeDir)
	require.NoError(t, err)
	session := writeObservedSession(t, store, "observe_standby")
	requestNode := firstNodeWithRequest(verificationruntime.SessionMetadata(session))
	require.Positive(t, requestNode)

	// A live foreign process (our parent) holds the lease.
	leasePath := writeForeignObserverLease(t, storeDir, session.ID, os.Getppid())

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- observeSession(ctx, store, session.ID) }()

	// The standby observer must not run a provider or write results.
	time.Sleep(150 * time.Millisecond)
	_, ok := loadPassiveRequestResult(store, session.ID, requestNode)
	assert.False(t, ok, "standby observer must not emit results")

	cancel()
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for standby to stop")
	}

	// The foreign lease is untouched.
	pid, _, ok2 := readObserverLease(t, leasePath)
	require.True(t, ok2)
	assert.Equal(t, os.Getppid(), pid)
}

func readObserverLease(t *testing.T, leasePath string) (int, time.Time, bool) {
	t.Helper()
	data, err := os.ReadFile(leasePath)
	if err != nil {
		return 0, time.Time{}, false
	}
	var pid int
	var nanos int64
	if _, err := fmt.Sscanf(string(data), "%d\n%d\n", &pid, &nanos); err != nil {
		return 0, time.Time{}, false
	}
	return pid, time.Unix(0, nanos), true
}

func TestObserveSessionTakesOverDeadOwnerLease(t *testing.T) {
	previousOptions := options
	options = Options{APIKey: func() (string, error) { return "", nil }}
	t.Cleanup(func() { options = previousOptions })

	storeDir := t.TempDir()
	store, err := coop.NewStoreAt(storeDir)
	require.NoError(t, err)
	session := writeObservedSession(t, store, "observe_takeover")
	requestNode := firstNodeWithRequest(verificationruntime.SessionMetadata(session))
	require.Positive(t, requestNode)

	leasePath := writeForeignObserverLease(t, storeDir, session.ID, 99999999)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- observeSession(ctx, store, session.ID) }()

	// The dead owner's lease is reclaimed and observation runs.
	require.Eventually(t, func() bool {
		_, ok := loadPassiveRequestResult(store, session.ID, requestNode)
		return ok
	}, 2*time.Second, 5*time.Millisecond)

	cancel()
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for takeover observer to stop")
	}

	// The lease was released on exit.
	_, statErr := os.Stat(leasePath)
	assert.True(t, os.IsNotExist(statErr))
}

func TestObserveSessionStandbyExitsOnTerminalSession(t *testing.T) {
	previousOptions := options
	options = Options{APIKey: func() (string, error) { return "", nil }}
	t.Cleanup(func() { options = previousOptions })

	storeDir := t.TempDir()
	store, err := coop.NewStoreAt(storeDir)
	require.NoError(t, err)
	session := writeObservedSession(t, store, "observe_standby_done")
	_, err = store.Update(session.ID, func(current *coop.Session) error {
		current.Status = coop.SessionCompleted
		return nil
	})
	require.NoError(t, err)
	writeForeignObserverLease(t, storeDir, session.ID, os.Getppid())

	done := make(chan error, 1)
	go func() { done <- observeSession(context.Background(), store, session.ID) }()
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(2 * time.Second):
		t.Fatal("standby did not exit on terminal session")
	}
}
