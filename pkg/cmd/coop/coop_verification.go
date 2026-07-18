package coopcmd

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/stripe/stripe-cli/pkg/coop"
	verificationruntime "github.com/stripe/stripe-cli/pkg/coop/verification/runtime"
)

type coopVerificationCmd struct {
	cmd         *cobra.Command
	session     string
	providers   []verificationruntime.Provider
	credentials []string
}

func newCoopVerificationCmd() *coopVerificationCmd {
	verificationCmd := &coopVerificationCmd{}
	verificationCmd.cmd = &cobra.Command{
		Use:   "verify",
		Short: "Run verification providers for a Co-op session",
		RunE: func(cmd *cobra.Command, args []string) error {
			store, err := coop.NewStore(coopConfigFolder())
			if err != nil {
				return fmt.Errorf("creating store: %w", err)
			}
			runner := verificationruntime.New(store, verificationruntime.WithCredentials(verificationCmd.credentials...))
			for _, provider := range verificationCmd.providers {
				if err := runner.Register(provider); err != nil {
					return err
				}
			}
			return runner.Run(cmd.Context(), verificationCmd.session)
		},
	}
	verificationCmd.cmd.Flags().StringVar(&verificationCmd.session, "session", "", "Session ID")
	mustMarkFlagRequired(verificationCmd.cmd, "session")
	return verificationCmd
}
