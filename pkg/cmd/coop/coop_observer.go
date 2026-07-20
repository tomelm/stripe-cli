package coopcmd

import (
	"context"
	"sync"

	"github.com/stripe/stripe-cli/pkg/coop"
	"github.com/stripe/stripe-cli/pkg/coop/observe"
	"github.com/stripe/stripe-cli/pkg/coop/tui"
	verificationruntime "github.com/stripe/stripe-cli/pkg/coop/verification/runtime"
)

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
func observeSession(ctx context.Context, store *coop.Store, sessionID string) error {
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
	return runner.Run(ctx, sessionID)
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
