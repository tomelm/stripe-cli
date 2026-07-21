package coopcmd

import (
	"context"

	"github.com/stripe/stripe-cli/pkg/coop/tui"
	"github.com/stripe/stripe-cli/pkg/coop/uicheck"
	"github.com/stripe/stripe-cli/pkg/coop/workflow"
)

// coopTUIOptions builds the standard TUI options: the sandbox claim URL plus
// the journey-outcome observer and confirm-time verifier. The checker is
// always attached — with no usable test key its reader is nil and every check
// resolves unavailable, which fails open to explicit human attestation.
//
// Returns a cleanup for the request-log stream, which owns a websocket.
func coopTUIOptions(ctx context.Context) ([]tui.Option, func()) {
	checker := uicheck.NewChecker(newOutcomeReader())
	stop := func() {}
	if source := startRequestLogSource(ctx); source != nil {
		checker = checker.WithRequestLog(source)
		stop = func() {
			_ = source.Close()
		}
	}
	return []tui.Option{
		tui.WithSandboxClaimURL(coopSandboxClaimURL()),
		tui.WithOutcomeObserver(checker),
		tui.WithWorkflowOptions(
			workflow.WithUIVerifier(checker),
			workflow.WithAppEntryProber(uicheck.NewAppEntryProbe()),
		),
	}, stop
}

// startRequestLogSource brings up the request-log stream that tells us HOW a
// journey settled — a browser confirming with a publishable key versus a
// server-side call with a secret one. It is strictly an enrichment: every
// failure path (no key, no device name, a capped session, or Stripe refusing
// the stream for this user agent) returns nil and verification proceeds on
// object state alone. Never disguise the user agent to get access.
func startRequestLogSource(ctx context.Context) *uicheck.StreamingRequestLogSource {
	if options.TestModeAPIKey == nil {
		return nil
	}
	apiKey, err := options.TestModeAPIKey()
	if err != nil || apiKey == "" {
		return nil
	}
	deviceName := "stripe-coop"
	if options.DeviceName != nil {
		if name, err := options.DeviceName(); err == nil && name != "" {
			// A distinct device name keeps the observer from competing with a
			// developer's own `stripe logs tail` session for the account's cap.
			deviceName = name + "-coop"
		}
	}
	source := uicheck.NewStreamingRequestLogSource(apiKey, deviceName, newOutcomeReader())
	if err := source.Start(ctx); err != nil {
		return nil
	}
	return source
}
