package coopcmd

import (
	"context"
	"fmt"
	"sync"

	"github.com/spf13/cobra"

	"github.com/stripe/stripe-cli/pkg/coop"
	"github.com/stripe/stripe-cli/pkg/coop/observe"
	verificationruntime "github.com/stripe/stripe-cli/pkg/coop/verification/runtime"
	"github.com/stripe/stripe-cli/pkg/stripe"
)

type coopVerificationCmd struct {
	cmd         *cobra.Command
	session     string
	providers   []verificationruntime.Provider
	credentials []string
	apiBaseURL  string
	noWSS       bool
}

func newCoopVerificationCmd() *coopVerificationCmd {
	verificationCmd := &coopVerificationCmd{}
	verificationCmd.cmd = &cobra.Command{
		Use:   "verify",
		Short: "Run verification providers for a Co-op session",
		RunE: func(cmd *cobra.Command, args []string) error {
			return verificationCmd.run(cmd.Context())
		},
	}
	verificationCmd.cmd.Flags().StringVar(&verificationCmd.session, "session", "", "Session ID")
	verificationCmd.cmd.Flags().StringVar(&verificationCmd.apiBaseURL, "api-base", stripe.DefaultAPIBaseURL, "Stripe API base for verification collectors")
	verificationCmd.cmd.Flags().BoolVar(&verificationCmd.noWSS, "no-wss", false, "Use ws:// for an explicit loopback API base")
	mustMarkFlagRequired(verificationCmd.cmd, "session")
	return verificationCmd
}

func (verificationCmd *coopVerificationCmd) run(ctx context.Context) error {
	store, err := coop.NewStore(coopConfigFolder())
	if err != nil {
		return fmt.Errorf("creating store: %w", err)
	}
	return runCoopVerification(
		ctx,
		store,
		verificationCmd.session,
		verificationCmd.apiBaseURL,
		verificationCmd.noWSS,
		verificationCmd.providers,
		verificationCmd.credentials,
	)
}

func runCoopVerification(
	ctx context.Context,
	store *coop.Store,
	sessionID string,
	apiBaseURL string,
	noWSS bool,
	registered []verificationruntime.Provider,
	credentials []string,
) error {
	providerConfig := observe.ProviderConfig{}
	if options.APIKey == nil {
		providerConfig.UnavailableReason = observe.UnavailableMissingCredentials
	} else {
		apiKey, err := options.APIKey()
		if err != nil || apiKey == "" {
			providerConfig.UnavailableReason = observe.UnavailableMissingCredentials
		} else {
			providerConfig.APIKey = apiKey
		}
	}
	if options.DeviceName != nil {
		providerConfig.DeviceName, _ = options.DeviceName()
	}
	if options.AccountID != nil {
		providerConfig.AccountID, _ = options.AccountID()
	}

	var connector observe.Connector
	if providerConfig.UnavailableReason == "" {
		created, err := observe.NewStripeConnector(observe.StripeConnectorOptions{APIBaseURL: apiBaseURL, NoWSS: noWSS})
		if err != nil {
			providerConfig.UnavailableReason = observe.UnavailableCollector
		} else {
			connector = created
		}
	}

	allCredentials := append([]string(nil), credentials...)
	allCredentials = append(allCredentials, providerConfig.APIKey)
	runner := verificationruntime.New(store, verificationruntime.WithCredentials(allCredentials...))
	if err := runner.Register(observe.NewProvider(store, providerConfig, connector)); err != nil {
		return err
	}
	for _, provider := range registered {
		if err := runner.Register(provider); err != nil {
			return err
		}
	}
	return runner.Run(ctx, sessionID)
}

var runOwnedCoopVerification = func(ctx context.Context, sessionID string) error {
	store, err := coop.NewStore(coopConfigFolder())
	if err != nil {
		return err
	}
	return runCoopVerification(ctx, store, sessionID, stripe.DefaultAPIBaseURL, false, nil, nil)
}

func startOwnedCoopVerification(session *coop.Session) func() {
	if session == nil {
		return func() {}
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = runOwnedCoopVerification(ctx, session.ID)
	}()
	var once sync.Once
	return func() {
		once.Do(func() {
			cancel()
			<-done
		})
	}
}
