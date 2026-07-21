package coopcmd

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/stripe/stripe-cli/pkg/coop"
	"github.com/stripe/stripe-cli/pkg/coop/helpers"
	"github.com/stripe/stripe-cli/pkg/coop/uicheck"
	"github.com/stripe/stripe-cli/pkg/coop/workflow"
)

type coopDebugAgentCmd struct {
	cmd             *cobra.Command
	session         string
	delay           time.Duration
	simulateOutcome string
	simulateObserve string
	live            bool
}

func newCoopDebugAgentCmd() *coopDebugAgentCmd {
	dc := &coopDebugAgentCmd{delay: 650 * time.Millisecond}
	dc.cmd = &cobra.Command{
		Use:    "debug-agent",
		Short:  "Run a deterministic fake co-op agent",
		Hidden: true,
		RunE:   dc.runDebugAgentCmd,
	}

	// Defaults keep plain --debug-agent runs self-sufficient: gated
	// uiComponent nodes get a synthetic binding and a quick simulated
	// observation, so the gate is exercised without wedging the session.
	dc.cmd.Flags().StringVar(&dc.session, "session", "", "Session ID to drive")
	dc.cmd.Flags().DurationVar(&dc.delay, "delay", dc.delay, "Delay between active and review states")
	dc.cmd.Flags().StringVar(&dc.simulateOutcome, "simulate-outcome", "bind", "Journey outcome binding: bind (synthetic ids) | off")
	dc.cmd.Flags().StringVar(&dc.simulateObserve, "simulate-observe", "after=2s", "Simulated observation: after=<duration> | fail | unavailable | never")
	dc.cmd.Flags().BoolVar(&dc.live, "live", false, "Execute apiRequest nodes for real with the test-mode key and bind real object ids")
	mustMarkFlagHidden(dc.cmd, "delay")
	mustMarkFlagHidden(dc.cmd, "simulate-outcome")
	mustMarkFlagHidden(dc.cmd, "simulate-observe")
	mustMarkFlagHidden(dc.cmd, "live")

	return dc
}

func (dc *coopDebugAgentCmd) runDebugAgentCmd(cmd *cobra.Command, args []string) error {
	if dc.session == "" {
		return fmt.Errorf("--session is required")
	}

	store, err := coop.NewStore(coopConfigFolder())
	if err != nil {
		return fmt.Errorf("creating store: %w", err)
	}

	agent := &coopDebugAgent{
		store:                    store,
		sessionID:                dc.session,
		delay:                    dc.delay,
		pollInterval:             500 * time.Millisecond,
		out:                      os.Stdout,
		waitForNextStepSelection: true,
		simulateBind:             dc.simulateOutcome == "bind",
		observeMode:              dc.simulateObserve,
	}
	if dc.live {
		live, err := newDebugLiveExecutor()
		if err != nil {
			return err
		}
		agent.live = live
	}
	return agent.run(cmd.Context())
}

type coopDebugAgent struct {
	store                    *coop.Store
	sessionID                string
	delay                    time.Duration
	pollInterval             time.Duration
	out                      io.Writer
	waitForNextStepSelection bool

	// simulateBind attaches synthetic outcome bindings to gated uiComponent
	// nodes; observeMode scripts what a fake observer then reports
	// (after=<dur> | fail | unavailable | never). live executes apiRequest
	// nodes for real and binds real object ids instead.
	simulateBind bool
	observeMode  string
	live         *debugLiveExecutor
}

func (a *coopDebugAgent) run(ctx context.Context) error {
	a.logf("debug agent attached to %s", a.sessionID)

	for {
		if err := ctx.Err(); err != nil {
			return err
		}

		session, err := a.store.Read(a.sessionID)
		if err != nil {
			return err
		}

		if session.IsComplete() {
			return a.completeSession(ctx, session)
		}

		if step := firstStepWithState(session, coop.NodeActive); step > 0 {
			if err := a.completeActiveStep(ctx, step); err != nil {
				return err
			}
			continue
		}

		if step := firstStepWithState(session, coop.NodeReview); step > 0 {
			if shouldContinueStepBeforeReview(session, step) {
				sessionStep, stepIndex, _, err := session.StepByNodeNumber(step)
				if err != nil {
					return err
				}
				if next := helpers.NextPendingNodeInStep(session, stepIndex+1, step); next > 0 {
					a.logf("step %q still has pending work; continuing with node %d", sessionStep.Title, next)
					if err := a.startStep(next); err != nil {
						return err
					}
					continue
				}
			}
			if err := a.awaitReview(ctx, step); err != nil {
				return err
			}
			continue
		}

		if step := firstStepWithState(session, coop.NodePending); step > 0 {
			if err := a.startStep(step); err != nil {
				return err
			}
			continue
		}

		if err := a.sleep(ctx, a.pollInterval); err != nil {
			return err
		}
	}
}

func (a *coopDebugAgent) startStep(step int) error {
	session, err := a.store.Read(a.sessionID)
	if err != nil {
		return err
	}
	node, err := session.NodeByNumber(step)
	if err != nil {
		return err
	}
	if node.State != coop.NodePending {
		return nil
	}

	resp, err := workflow.NewService(a.store).StartWork(a.sessionID, step, "Debug agent working: "+node.Title)
	if err != nil {
		return err
	}
	if !resp.OK {
		return fmt.Errorf("%s", resp.Error)
	}
	a.logf("step %d active: %s", step, node.Title)
	return nil
}

func (a *coopDebugAgent) completeActiveStep(ctx context.Context, step int) error {
	if err := a.sleep(ctx, a.delay); err != nil {
		return err
	}

	session, err := a.store.Read(a.sessionID)
	if err != nil {
		return err
	}
	node, err := session.NodeByNumber(step)
	if err != nil {
		return err
	}
	if node.State != coop.NodeActive {
		return nil
	}

	service := workflow.NewService(a.store)
	resp, err := service.ReportCheck(a.sessionID, step, "Debug agent deterministic check", true)
	if err != nil {
		return err
	}
	if !resp.OK {
		return fmt.Errorf("%s", resp.Error)
	}
	input := workflow.ReportWorkInput{
		File:  "debug/" + safeDebugFileName(node.Key) + ".txt",
		Lines: "1-1",
		Note:  "Deterministic debug agent completed " + node.Title,
	}
	if a.live != nil && node.Type == coop.NodeAPIRequest && node.Request != nil {
		result, err := a.live.execute(ctx, session, step, node)
		if err != nil {
			return fmt.Errorf("live request for node %d: %w", step, err)
		}
		input.Note = "Live debug agent executed " + strings.ToUpper(node.Request.Method) + " " + node.Request.Path
		if id := result.Get("id").String(); id != "" {
			input.Note += " -> " + id
		}
	}
	if expectation, ok := uicheck.DeriveExpectation(session, step); ok && expectation.Gated() {
		switch {
		case a.live != nil:
			// App-minted journeys deliberately return no id: the object does
			// not exist until the developer walks the app, so the entry URL is
			// the whole binding and discovery supplies the object later.
			id, journeyURL := a.live.bindingFor(expectation)
			input.JourneyURL = journeyURL
			if id != "" {
				input.Outcome = &workflow.OutcomeInput{Role: expectation.Role, ID: id}
				a.logf("step %d bound live outcome %s=%s", step, expectation.Role, id)
			} else if journeyURL != "" {
				a.logf("step %d journey starts at %s (%s discovered after the walk)", step, journeyURL, expectation.Role)
			}
		case a.simulateBind:
			input.Outcome = &workflow.OutcomeInput{
				Role: expectation.Role,
				ID:   fmt.Sprintf("%sdebug_%06d", expectation.IDPrefix, step),
			}
			input.JourneyURL = "https://example.com/debug-journey"
		}
	}
	resp, err = service.ReportWork(a.sessionID, step, input, false)
	if err != nil {
		return err
	}
	if !resp.OK {
		return fmt.Errorf("%s", resp.Error)
	}
	if input.Outcome != nil && a.live == nil {
		a.scheduleSimulatedObservation(ctx, step, input.Outcome.ID)
	}
	a.logf("step %d %s: %s", step, resp.State, node.Title)
	return nil
}

// scheduleSimulatedObservation plays the observer for headless runs: after
// the configured delay it persists a scripted observation through the same
// compare-and-set path the real observer uses, so the TUI renders production
// behavior with zero network.
func (a *coopDebugAgent) scheduleSimulatedObservation(ctx context.Context, step int, boundID string) {
	mode := strings.TrimSpace(a.observeMode)
	if mode == "" || mode == "never" {
		return
	}
	delay := time.Second
	observation := uicheck.Observation{}
	switch {
	case strings.HasPrefix(mode, "after="):
		parsed, err := time.ParseDuration(strings.TrimPrefix(mode, "after="))
		if err != nil {
			a.logf("invalid --simulate-observe %q: %v", mode, err)
			return
		}
		delay = parsed
		observation = uicheck.Observation{
			Status: coop.UIOutcomeObserved,
			Detail: "debug simulated observation",
			Evidence: []coop.UIOutcomeEvidence{
				{Key: "status", Value: "complete"},
				{Key: "payment_status", Value: "paid"},
			},
		}
	case mode == "fail":
		observation = uicheck.Observation{
			Status:   coop.UIOutcomeFailed,
			Detail:   "debug simulated failure: the journey did not complete",
			Evidence: []coop.UIOutcomeEvidence{{Key: "status", Value: "expired"}},
		}
	case mode == "unavailable":
		observation = uicheck.Observation{
			Status: coop.UIOutcomeUnavailable,
			Detail: "debug simulated unavailable check",
		}
	default:
		a.logf("unknown --simulate-observe mode %q", mode)
		return
	}
	go func() {
		if err := a.sleep(ctx, delay); err != nil {
			return
		}
		applied, err := uicheck.ApplyObservation(a.store, a.sessionID, step, boundID, observation, time.Now())
		if err != nil {
			a.logf("simulated observation for step %d failed: %v", step, err)
			return
		}
		if applied {
			a.logf("simulated observation applied to step %d (%s)", step, observation.Status)
		}
	}()
}

func (a *coopDebugAgent) awaitReview(ctx context.Context, step int) error {
	session, err := a.store.Read(a.sessionID)
	if err != nil {
		return err
	}
	stepDefinition, stepIndex, _, err := session.StepByNodeNumber(step)
	if err != nil {
		return err
	}
	a.logf("waiting for step review: %s", stepDefinition.Title)
	return a.awaitStepReview(ctx, stepIndex)
}

func (a *coopDebugAgent) awaitStepReview(ctx context.Context, stepIndex int) error {
	for {
		if err := a.store.WriteHeartbeat(a.sessionID); err != nil {
			return err
		}
		if err := a.sleep(ctx, a.pollInterval); err != nil {
			_ = a.store.RemoveHeartbeat(a.sessionID)
			return err
		}

		session, err := a.store.Read(a.sessionID)
		if err != nil {
			_ = a.store.RemoveHeartbeat(a.sessionID)
			return err
		}
		if active := session.FirstActiveNodeInStep(stepIndex); active > 0 {
			_ = a.store.RemoveHeartbeat(a.sessionID)
			a.logf("step requested changes; rerunning from step %d", active)
			return nil
		}
		if !session.StepHasReview(stepIndex) {
			_ = a.store.RemoveHeartbeat(a.sessionID)
			a.logf("step review released")
			return nil
		}
	}
}

func (a *coopDebugAgent) completeSession(ctx context.Context, session *coop.Session) error {
	if session.Status != coop.SessionCompleted || session.NextSteps == nil || len(session.NextSteps.Suggestions) == 0 {
		suggestions := helpers.BuildSuggestions(session, helpers.DetectProjectEnvironment())
		a.logf("all steps complete; showing next steps")
		if err := helpers.ShowSuggestions(a.store, session, suggestions, ""); err != nil {
			if isVersionConflict(err) {
				return nil
			}
			return err
		}
	}

	if !a.waitForNextStepSelection {
		return nil
	}

	for {
		if err := a.sleep(ctx, a.pollInterval); err != nil {
			return err
		}

		session, err := a.store.Read(a.sessionID)
		if err != nil {
			return err
		}
		if session.NextSteps == nil || session.NextSteps.Selected == "" {
			continue
		}

		selected := session.NextSteps.Selected
		session.NextSteps.Selected = ""
		if err := a.store.Write(session); err != nil && !isVersionConflict(err) {
			return err
		}
		a.logf("next step selected: %s", selected)
		return nil
	}
}

func firstStepWithState(session *coop.Session, state coop.NodeState) int {
	step := 0
	for i := range session.Steps {
		for j := range session.Steps[i].Nodes {
			step++
			if session.Steps[i].Nodes[j].State == state {
				return step
			}
		}
	}
	return 0
}

func shouldContinueStepBeforeReview(session *coop.Session, step int) bool {
	_, stepIndex, _, err := session.StepByNodeNumber(step)
	if err != nil {
		return false
	}
	return !session.StepReadyForReview(stepIndex)
}

func safeDebugFileName(key string) string {
	if key == "" {
		return "step"
	}
	return strings.NewReplacer("/", "_", "\\", "_", " ", "_").Replace(key)
}

func isVersionConflict(err error) bool {
	return err != nil && strings.Contains(err.Error(), "version conflict")
}

func (a *coopDebugAgent) sleep(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func (a *coopDebugAgent) logf(format string, args ...interface{}) {
	if a.out == nil {
		return
	}
	fmt.Fprintf(a.out, "[debug-agent] "+format+"\n", args...)
}
