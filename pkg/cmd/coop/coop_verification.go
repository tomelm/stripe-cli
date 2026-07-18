package coopcmd

import (
	"context"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/stripe/stripe-cli/pkg/coop"
	"github.com/stripe/stripe-cli/pkg/coop/resourcecheck"
	verificationruntime "github.com/stripe/stripe-cli/pkg/coop/verification/runtime"
	"github.com/stripe/stripe-cli/pkg/stripe"
)

type coopVerificationCmd struct {
	cmd                  *cobra.Command
	session              string
	resourceInputs       []string
	applicationResources []string
	valueInputs          []string
	windowStart          string
	windowEnd            string
	deadline             time.Duration
	apiBaseURL           string
	providers            []verificationruntime.Provider
	credentials          []string
}

func newCoopVerificationCmd() *coopVerificationCmd {
	verificationCmd := &coopVerificationCmd{}
	verificationCmd.cmd = &cobra.Command{
		Use:   "verify",
		Short: "Run advisory verification providers for a Co-op session",
		Long: `Runs bounded, read-only verification providers for a Co-op session.

Resource identities can be supplied directly or marked as originating from an
application record. Without exact identities, provide a bounded RFC3339 action
window and any declaration inputs needed to discover one unambiguous resource.
Missing credentials or API support produce advisory unavailable results.`,
		Example: `  stripe coop verify --session=coop_123 \
    --application-resource checkout=checkout.session:cs_123
  stripe coop verify --session=coop_123 \
    --window-start=2026-07-18T20:00:00Z --window-end=2026-07-18T20:10:00Z \
    --value currency='"usd"'`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return verificationCmd.run(cmd.Context())
		},
	}
	verificationCmd.cmd.Flags().StringVar(&verificationCmd.session, "session", "", "Session ID")
	verificationCmd.cmd.Flags().StringArrayVar(&verificationCmd.resourceInputs, "resource", nil, "Exact Stripe resource as key=type:id")
	verificationCmd.cmd.Flags().StringArrayVar(&verificationCmd.applicationResources, "application-resource", nil, "Application-record Stripe resource as key=type:id")
	verificationCmd.cmd.Flags().StringArrayVar(&verificationCmd.valueInputs, "value", nil, "Declaration input as key=<JSON scalar>")
	verificationCmd.cmd.Flags().StringVar(&verificationCmd.windowStart, "window-start", "", "Action-window start in RFC3339 format")
	verificationCmd.cmd.Flags().StringVar(&verificationCmd.windowEnd, "window-end", "", "Action-window end in RFC3339 format")
	verificationCmd.cmd.Flags().DurationVar(&verificationCmd.deadline, "deadline", 10*time.Second, "Overall resource-verification deadline")
	verificationCmd.cmd.Flags().StringVar(&verificationCmd.apiBaseURL, "api-base", stripe.DefaultAPIBaseURL, "Sets the API base URL")
	mustMarkFlagHidden(verificationCmd.cmd, "api-base")
	mustMarkFlagRequired(verificationCmd.cmd, "session")
	return verificationCmd
}

func (verificationCmd *coopVerificationCmd) run(ctx context.Context) error {
	references, err := parseResourceInputs(verificationCmd.resourceInputs, verificationCmd.applicationResources)
	if err != nil {
		return err
	}
	values, err := parseVerificationValues(verificationCmd.valueInputs)
	if err != nil {
		return err
	}
	window, err := parseActionWindow(verificationCmd.windowStart, verificationCmd.windowEnd)
	if err != nil {
		return err
	}
	if verificationCmd.deadline <= 0 || verificationCmd.deadline > resourcecheck.MaxProviderRuntime {
		return fmt.Errorf("--deadline must be positive and no greater than %s", resourcecheck.MaxProviderRuntime)
	}
	if err := stripe.ValidateAPIBaseURL(verificationCmd.apiBaseURL); err != nil {
		return err
	}

	store, err := coop.NewStore(coopConfigFolder())
	if err != nil {
		return fmt.Errorf("creating store: %w", err)
	}

	apiKey := ""
	if options.TestModeAPIKey != nil {
		apiKey, _ = options.TestModeAPIKey()
	}
	accountID := ""
	if options.AccountID != nil {
		accountID, _ = options.AccountID()
	}
	account := resourcecheck.AccountContext{Mode: resourcecheck.ModeTest, AccountID: accountID}
	var reader resourcecheck.Reader
	if accountID != "" {
		baseURL, parseErr := url.Parse(verificationCmd.apiBaseURL)
		if parseErr != nil {
			return parseErr
		}
		stripeReader, readerErr := resourcecheck.NewStripeReader(resourcecheck.StripeReaderConfig{
			Credential: resourcecheck.NewStripeCredential(apiKey),
			Client:     &stripe.Client{BaseURL: baseURL},
			Account:    account,
		})
		if readerErr == nil {
			reader = stripeReader
		}
	}

	credentials := append([]string(nil), verificationCmd.credentials...)
	if apiKey != "" {
		credentials = append(credentials, apiKey)
	}
	runner := verificationruntime.New(store, verificationruntime.WithCredentials(credentials...))
	resourceProvider := resourcecheck.NewRuntimeProvider(resourcecheck.RuntimeProviderConfig{
		Reader: reader, Account: account, References: references, ActionWindow: window, Values: values, Deadline: verificationCmd.deadline,
	})
	if err := runner.Register(resourceProvider); err != nil {
		return err
	}
	for _, provider := range verificationCmd.providers {
		if err := runner.Register(provider); err != nil {
			return err
		}
	}
	return runner.Run(ctx, verificationCmd.session)
}

func parseResourceInputs(explicit, application []string) (map[string]resourcecheck.ResourceReference, error) {
	references := make(map[string]resourcecheck.ResourceReference, len(explicit)+len(application))
	parse := func(values []string, origin resourcecheck.ReferenceOrigin) error {
		for _, value := range values {
			key, encoded, ok := strings.Cut(value, "=")
			if !ok || key == "" || encoded == "" {
				return fmt.Errorf("resource reference %q must use key=type:id", value)
			}
			resourceType, id, ok := strings.Cut(encoded, ":")
			if !ok || resourceType == "" || id == "" {
				return fmt.Errorf("resource reference %q must use key=type:id", value)
			}
			if _, exists := references[key]; exists {
				return fmt.Errorf("resource reference key %q was supplied more than once", key)
			}
			ref, err := resourcecheck.NewResourceRef(resourcecheck.ResourceType(resourceType), id)
			if err != nil {
				return fmt.Errorf("resource reference %q is invalid", key)
			}
			references[key] = resourcecheck.ResourceReference{Resource: ref, Origin: origin}
		}
		return nil
	}
	if err := parse(explicit, resourcecheck.ReferenceExplicit); err != nil {
		return nil, err
	}
	if err := parse(application, resourcecheck.ReferenceApplicationRecord); err != nil {
		return nil, err
	}
	return references, nil
}

func parseVerificationValues(inputs []string) (map[string]resourcecheck.JSONScalar, error) {
	values := make(map[string]resourcecheck.JSONScalar, len(inputs))
	for _, input := range inputs {
		key, raw, ok := strings.Cut(input, "=")
		if !ok || key == "" || raw == "" {
			return nil, fmt.Errorf("verification value %q must use key=<JSON scalar>", input)
		}
		if _, exists := values[key]; exists {
			return nil, fmt.Errorf("verification value %q was supplied more than once", key)
		}
		value, err := resourcecheck.ParseJSONScalar([]byte(raw))
		if err != nil {
			return nil, fmt.Errorf("verification value %q must be a JSON scalar", key)
		}
		values[key] = value
	}
	return values, nil
}

func parseActionWindow(startValue, endValue string) (*resourcecheck.CreationWindow, error) {
	if startValue == "" && endValue == "" {
		return nil, nil
	}
	if startValue == "" || endValue == "" {
		return nil, fmt.Errorf("--window-start and --window-end must be supplied together")
	}
	start, err := time.Parse(time.RFC3339Nano, startValue)
	if err != nil {
		return nil, fmt.Errorf("--window-start must use RFC3339")
	}
	end, err := time.Parse(time.RFC3339Nano, endValue)
	if err != nil {
		return nil, fmt.Errorf("--window-end must use RFC3339")
	}
	if !end.After(start) || end.Sub(start) > resourcecheck.MaxCreationWindow {
		return nil, fmt.Errorf("action window must be positive and no greater than %s", resourcecheck.MaxCreationWindow)
	}
	return &resourcecheck.CreationWindow{Start: start, End: end}, nil
}
