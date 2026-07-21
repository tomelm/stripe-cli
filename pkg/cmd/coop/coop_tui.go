package coopcmd

import (
	"github.com/stripe/stripe-cli/pkg/coop/tui"
	"github.com/stripe/stripe-cli/pkg/coop/uicheck"
	"github.com/stripe/stripe-cli/pkg/coop/workflow"
)

// coopTUIOptions builds the standard TUI options: the sandbox claim URL plus
// the journey-outcome observer and confirm-time verifier. The checker is
// always attached — with no usable test key its reader is nil and every check
// resolves unavailable, which fails open to explicit human attestation.
func coopTUIOptions() []tui.Option {
	checker := uicheck.NewChecker(newOutcomeReader())
	return []tui.Option{
		tui.WithSandboxClaimURL(coopSandboxClaimURL()),
		tui.WithOutcomeObserver(checker),
		tui.WithWorkflowOptions(
			workflow.WithUIVerifier(checker),
			workflow.WithAppEntryProber(uicheck.NewAppEntryProbe()),
		),
	}
}
