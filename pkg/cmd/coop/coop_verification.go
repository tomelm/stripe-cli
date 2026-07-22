package coopcmd

import (
	"context"
	"fmt"
	"net/url"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/stripe/stripe-cli/pkg/coop"
	"github.com/stripe/stripe-cli/pkg/coop/checkrun"
	"github.com/stripe/stripe-cli/pkg/coop/checks"
	"github.com/stripe/stripe-cli/pkg/coop/workflow"
	"github.com/stripe/stripe-cli/pkg/stripe"
)

// coopEvaluator is intentionally glue, not another verification runtime. It
// compiles the frozen containing step and invokes the one bounded evaluator
// for report, observation, and polling triggers alike.
type coopEvaluator struct {
	catalog         checks.Catalog
	runner          *checkrun.Evaluator
	accountID       string
	readerAccountID string
	stripeReader    *checkrun.StripeReader
	now             func() time.Time
}

func newCoopEvaluator() (*coopEvaluator, error) {
	catalog, err := checks.LoadCatalog()
	if err != nil {
		return nil, err
	}
	var reader checkrun.Reader
	var stripeReader *checkrun.StripeReader
	var readerAccountID string
	apiKey, keyErr := configuredTestKey()
	accountID, accountErr := configuredAccountID()
	if keyErr == nil && accountErr == nil && apiKey != "" && accountID != "" {
		candidate, readerErr := newCoopStripeReader(apiKey, accountID)
		if readerErr == nil {
			reader = candidate
			stripeReader = candidate
			readerAccountID = strings.TrimSpace(accountID)
		}
	}
	return &coopEvaluator{
		catalog: catalog, runner: checkrun.NewEvaluator(reader, catalog),
		accountID: strings.TrimSpace(accountID), readerAccountID: readerAccountID, stripeReader: stripeReader, now: time.Now,
	}, nil
}

func newCoopStripeReader(apiKey, accountID string) (*checkrun.StripeReader, error) {
	client := options.StripeClient
	if client == nil {
		baseURL, err := url.Parse(stripe.DefaultAPIBaseURL)
		if err != nil {
			return nil, err
		}
		client = &stripe.Client{BaseURL: baseURL}
	}
	return checkrun.NewStripeReader(checkrun.StripeReaderConfig{
		Credential: checkrun.NewStripeCredential(apiKey), Client: client, AccountID: accountID,
	})
}

// authorizeAccount returns a pinnable account only after Stripe authenticated
// the exact configured test identity. Authorization failures remain retryable
// and never mutate the session.
func (e *coopEvaluator) authorizeAccount(ctx context.Context) (string, error) {
	if e == nil || e.stripeReader == nil || e.readerAccountID == "" {
		return "", checkrun.ErrUnavailable
	}
	if err := e.stripeReader.Authorize(ctx); err != nil {
		return "", err
	}
	return e.readerAccountID, nil
}

func configuredTestKey() (string, error) {
	if options.TestModeAPIKey == nil {
		return "", fmt.Errorf("test-mode key is not configured")
	}
	return options.TestModeAPIKey()
}

func configuredAccountID() (string, error) {
	if options.AccountID == nil {
		return "", fmt.Errorf("account identity is not configured")
	}
	return options.AccountID()
}

func (e *coopEvaluator) Requirements(session *coop.Session, nodeNumber int) ([]coop.ResourceRequirement, error) {
	plan, node, err := e.plan(session, nodeNumber)
	if err != nil {
		return nil, err
	}
	// UI attempts discover resource identities while the developer exercises
	// the app. Their only report-time requirement is the app URL.
	if node.Type == coop.NodeUIComponent {
		return nil, nil
	}
	plan = plan.ForNode(node.Key)
	var requirements []coop.ResourceRequirement
	seen := make(map[string]bool)
	add := func(role, resourceType string, required bool) {
		if seen[role] {
			return
		}
		seen[role] = true
		requirements = append(requirements, coop.ResourceRequirement{Role: role, Type: resourceType, Required: required})
	}
	for _, resource := range plan.Resources {
		add(resource.Role, resource.ResourceType, resource.Importance == checks.ImportanceBlocking)
	}
	for _, state := range plan.States {
		add(state.Role, state.ResourceType, state.Importance == checks.ImportanceBlocking)
	}
	sort.Slice(requirements, func(i, j int) bool { return requirements[i].Role < requirements[j].Role })
	return requirements, nil
}

func (e *coopEvaluator) Evaluate(ctx context.Context, input workflow.EvaluationInput) (workflow.Evaluation, error) {
	if input.Session == nil || input.Session.StripeAccountID == "" {
		return e.accountUnavailable(
			"Automatic verification is unavailable because this session has no pinned Stripe account.",
			"Configure test-mode Stripe authentication and retry this command.",
		), nil
	}
	if e.accountID == "" {
		return e.accountUnavailable(
			"Automatic verification is unavailable because the active Stripe account could not be identified.",
			"Configure test-mode Stripe authentication and try again.",
		), nil
	}
	if input.Session.StripeAccountID != e.accountID {
		return workflow.Evaluation{Results: []coop.CheckResult{{
			ID: "automatic.account-scope", Kind: coop.CheckCoverage, Importance: coop.CheckRequired,
			Status: coop.CheckUnavailable,
			Detail: "Automatic verification is unavailable because the active Stripe account differs from this session.",
			Repair: "Switch back to the Stripe account used when this Co-op session started.", UpdatedAt: e.now().UTC(),
		}}}, nil
	}
	plan, node, err := e.plan(input.Session, input.NodeNumber)
	if err != nil {
		return workflow.Evaluation{}, err
	}
	// An app UI is the human-facing review surface for its containing step.
	// Re-evaluate that step's ordinary checks on the UI attempt while keeping
	// every non-UI attempt node-scoped.
	if node.Type != coop.NodeUIComponent {
		plan = plan.ForNode(node.Key)
	}
	observedAt := e.now().UTC()
	base := checkrun.Input{
		Plan: plan, Session: input.Session, NodeNumber: input.NodeNumber,
		AttemptNumber: input.Attempt, ObservedAt: observedAt,
		State: &checkrun.StateObservation{EventType: input.EventType, ResourceID: input.ResourceID},
	}
	report, err := e.runner.Evaluate(ctx, base)
	if err != nil {
		return workflow.Evaluation{}, err
	}
	for _, gap := range plan.CoverageGaps {
		report.Results = append(report.Results, coop.CheckResult{
			ID: gap.ID, Kind: coop.CheckCoverage, Importance: coop.CheckAdvisory,
			Status: coop.CheckUnavailable, Detail: boundedVerificationText(gap.Reason),
			Repair: boundedVerificationText(gap.Repair), UpdatedAt: observedAt,
		})
	}
	return workflow.Evaluation{Results: report.Results, Bindings: report.Bindings}, nil
}

func (e *coopEvaluator) accountUnavailable(detail, repair string) workflow.Evaluation {
	return workflow.Evaluation{Results: []coop.CheckResult{{
		ID: "automatic.account-scope", Kind: coop.CheckCoverage, Importance: coop.CheckRequired,
		Status: coop.CheckUnavailable, Detail: detail, Repair: repair, UpdatedAt: e.now().UTC(),
	}}}
}

func (e *coopEvaluator) plan(session *coop.Session, nodeNumber int) (checks.StepPlan, *coop.SessionNode, error) {
	if e == nil || session == nil {
		return checks.StepPlan{}, nil, fmt.Errorf("verification session is required")
	}
	step, stepIndex, _, err := session.StepByNodeNumber(nodeNumber)
	if err != nil {
		return checks.StepPlan{}, nil, err
	}
	node, err := session.NodeByNumber(nodeNumber)
	if err != nil {
		return checks.StepPlan{}, nil, err
	}
	var plan checks.StepPlan
	if node.Type == coop.NodeUIComponent {
		plan, err = checks.CompileUIReviewStep(e.catalog, session, stepIndex, node.Key)
	} else {
		plan, err = checks.CompileStep(e.catalog, *step)
	}
	return plan, node, err
}

func boundedVerificationText(value string) string {
	value = strings.TrimSpace(value)
	if len(value) <= coop.MaxCheckResultDetailBytes {
		return value
	}
	end := coop.MaxCheckResultDetailBytes - 3
	for !utf8.ValidString(value[:end]) {
		end--
	}
	return value[:end] + "..."
}

var _ workflow.Evaluator = (*coopEvaluator)(nil)
