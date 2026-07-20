package coopcmd

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/stripe/stripe-cli/pkg/coop"
	"github.com/stripe/stripe-cli/pkg/coop/observe"
	"github.com/stripe/stripe-cli/pkg/coop/tui"
	verificationruntime "github.com/stripe/stripe-cli/pkg/coop/verification/runtime"
)

const (
	observerLeaseRefreshInterval = 5 * time.Second
	observerLeaseRetryInterval   = 2 * time.Second
)

var errObserverLeaseLost = errors.New("observer lease lost")

// coopTUIOptions is the production option set for every Co-op TUI launch: the
// TUI process owns passive session observation for its lifetime.
func coopTUIOptions() []tui.Option {
	return []tui.Option{
		tui.WithSandboxClaimURL(coopSandboxClaimURL()),
		tui.WithObserver(newSessionObserverFactory()),
	}
}

// runCoopTUI and runCoopTUIWaiting are the only production TUI launchers.
// They are vars so command tests can intercept the launch.
var runCoopTUI = func(store *coop.Store, sessionID string) error {
	return tui.Run(store, sessionID, coopTUIOptions()...)
}

var runCoopTUIWaiting = func(store *coop.Store, existingSessionIDs map[string]bool) error {
	return tui.RunWaiting(store, existingSessionIDs, coopTUIOptions()...)
}

// newSessionObserverFactory returns the observer the TUI starts per session.
// The returned stop function cancels observation and blocks until shutdown.
func newSessionObserverFactory() tui.ObserverFactory {
	return func(sessionID string) func() {
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		go func() {
			defer close(done)
			_ = runSessionObservation(ctx, sessionID)
		}()
		var once sync.Once
		return func() {
			once.Do(func() {
				cancel()
				<-done
			})
		}
	}
}

// runSessionObservation is a var so tests can record observer ownership.
var runSessionObservation = func(ctx context.Context, sessionID string) error {
	store, err := coop.NewStore(coopConfigFolder())
	if err != nil {
		return err
	}
	return observeSession(ctx, store, sessionID)
}

// observeSession runs passive observation for one session until the session
// reaches a terminal state or ctx is canceled. Results are advisory only.
//
// Only one process observes a session at a time: a second joined TUI stands
// by on the observer lease and takes over if the holder exits or crashes.
func observeSession(ctx context.Context, store *coop.Store, sessionID string) error {
	for {
		if ctx.Err() != nil {
			return nil
		}
		if terminal, err := sessionTerminal(store, sessionID); err == nil && terminal {
			return nil
		}
		held, err := store.AcquireObserverLease(sessionID)
		if err != nil {
			return err
		}
		if !held {
			if !sleepContext(ctx, observerLeaseRetryInterval) {
				return nil
			}
			continue
		}
		err = observeSessionHoldingLease(ctx, store, sessionID)
		_ = store.ReleaseObserverLease(sessionID)
		if errors.Is(err, errObserverLeaseLost) {
			continue
		}
		return err
	}
}

// observeSessionHoldingLease runs the observer while refreshing the lease.
// Losing the lease (e.g. reclaimed after a long system suspend) cancels the
// runner and reports errObserverLeaseLost so the caller re-enters standby.
func observeSessionHoldingLease(ctx context.Context, store *coop.Store, sessionID string) error {
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	lost := make(chan struct{})
	refreshDone := make(chan struct{})
	go func() {
		defer close(refreshDone)
		ticker := time.NewTicker(observerLeaseRefreshInterval)
		defer ticker.Stop()
		for {
			select {
			case <-runCtx.Done():
				return
			case <-ticker.C:
				if held, err := store.AcquireObserverLease(sessionID); err != nil || !held {
					close(lost)
					cancel()
					return
				}
			}
		}
	}()

	providerConfig := passiveProviderConfig()
	var connector observe.Connector
	if providerConfig.UnavailableReason == "" {
		connector = observe.NewStripeConnector()
	}
	runner := verificationruntime.New(
		store,
		observe.NewProvider(store, providerConfig, connector),
		verificationruntime.WithCredentials(providerConfig.APIKey),
	)
	err := runner.Run(runCtx, sessionID)
	cancel()
	<-refreshDone
	select {
	case <-lost:
		return errObserverLeaseLost
	default:
	}
	return err
}

func sessionTerminal(store *coop.Store, sessionID string) (bool, error) {
	session, err := store.Read(sessionID)
	if err != nil {
		return false, err
	}
	return session.Status != coop.SessionActive || session.IsComplete(), nil
}

// sleepContext waits for d and reports false when ctx ended first.
func sleepContext(ctx context.Context, d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

// passiveProviderConfig resolves collector credentials from the CLI profile.
// Missing credentials and live-mode keys both leave observation advisory-
// unavailable without opening a stream; nothing here triggers a login flow.
func passiveProviderConfig() observe.ProviderConfig {
	providerConfig := observe.ProviderConfig{}
	switch {
	case options.APIKey == nil:
		providerConfig.UnavailableReason = observe.UnavailableMissingCredentials
	default:
		apiKey, err := options.APIKey()
		switch {
		case err != nil || apiKey == "":
			providerConfig.UnavailableReason = observe.UnavailableMissingCredentials
		case observe.IsLiveModeAPIKey(apiKey):
			providerConfig.UnavailableReason = observe.UnavailableLiveCredentials
		default:
			providerConfig.APIKey = apiKey
		}
	}
	if options.DeviceName != nil {
		providerConfig.DeviceName, _ = options.DeviceName()
	}
	if options.AccountID != nil {
		providerConfig.AccountID, _ = options.AccountID()
	}
	return providerConfig
}
