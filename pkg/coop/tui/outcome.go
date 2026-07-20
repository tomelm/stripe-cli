package tui

import (
	"context"
	"fmt"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/stripe/stripe-cli/pkg/coop"
	"github.com/stripe/stripe-cli/pkg/coop/uicheck"
	"github.com/stripe/stripe-cli/pkg/coop/workflow"
)

// OutcomeObserver runs one observation pass over a session's bound journey
// outcomes, persisting changes through the store. *uicheck.Checker satisfies
// it. The TUI schedules passes on its own message chain — deliberately NOT
// the 500ms session tick, which pauses while the terminal is unfocused
// (which is exactly when the human is off paying in a browser).
type OutcomeObserver interface {
	Watch(ctx context.Context, store uicheck.SessionStore, sessionID string, now func() time.Time) (bool, error)
}

// WithOutcomeObserver enables background journey-outcome observation.
func WithOutcomeObserver(observer OutcomeObserver) Option {
	return func(m *Model) {
		m.outcomeObserver = observer
	}
}

// WithWorkflowOptions threads options (e.g. the confirm-time verifier) into
// every workflow service the TUI constructs.
func WithWorkflowOptions(opts ...workflow.Option) Option {
	return func(m *Model) {
		m.workflowOpts = opts
	}
}

// workflowService builds the workflow service the TUI acts through.
func (m Model) workflowService() *workflow.Service {
	return workflow.NewService(m.store, m.workflowOpts...)
}

// outcomeTickMsg schedules the next observation pass.
type outcomeTickMsg time.Time

// outcomeCheckedMsg reports a finished observation pass.
type outcomeCheckedMsg struct {
	changed bool
}

const (
	outcomeWatchActive = 3 * time.Second
	outcomeWatchSlow   = 10 * time.Second
	outcomeWatchIdle   = 30 * time.Second
	// outcomeActiveWindow is how long after report-work the fast cadence
	// holds — the human is most likely mid-journey right then.
	outcomeActiveWindow = 30 * time.Minute
	outcomePassTimeout  = 15 * time.Second
)

func outcomeTickCmd(interval time.Duration) tea.Cmd {
	return tea.Tick(interval, func(t time.Time) tea.Msg {
		return outcomeTickMsg(t)
	})
}

// observeOutcomesCmd runs one observation pass off the UI goroutine.
func (m Model) observeOutcomesCmd() tea.Cmd {
	observer := m.outcomeObserver
	store := m.store
	sessionID := m.sessionID
	if observer == nil || store == nil || sessionID == "" {
		return nil
	}
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), outcomePassTimeout)
		defer cancel()
		changed, _ := observer.Watch(ctx, store, sessionID, time.Now)
		return outcomeCheckedMsg{changed: changed}
	}
}

// handleOutcomeTick decides whether a pass is due and re-arms the chain.
func (m *Model) handleOutcomeTick() tea.Cmd {
	if m.outcomeObserver == nil {
		return nil
	}
	if m.session == nil || !m.sessionHasWatchableOutcome() {
		return outcomeTickCmd(outcomeWatchIdle)
	}
	return m.observeOutcomesCmd()
}

func (m *Model) handleOutcomeChecked(msg outcomeCheckedMsg) tea.Cmd {
	interval := m.outcomeWatchInterval()
	if msg.changed {
		// Pick the change up immediately instead of waiting for the session tick.
		return tea.Batch(m.checkForUpdates(), outcomeTickCmd(interval))
	}
	return outcomeTickCmd(interval)
}

func (m Model) sessionHasWatchableOutcome() bool {
	if m.session == nil {
		return false
	}
	for i := range m.session.Steps {
		for j := range m.session.Steps[i].Nodes {
			node := &m.session.Steps[i].Nodes[j]
			if node.State != coop.NodeReview || node.UIOutcome == nil {
				continue
			}
			if node.UIOutcome.Status == coop.UIOutcomePending || node.UIOutcome.Status == coop.UIOutcomeUnavailable {
				return true
			}
		}
	}
	return false
}

func (m Model) outcomeWatchInterval() time.Duration {
	if m.session == nil {
		return outcomeWatchIdle
	}
	interval := outcomeWatchIdle
	now := time.Now()
	for i := range m.session.Steps {
		for j := range m.session.Steps[i].Nodes {
			node := &m.session.Steps[i].Nodes[j]
			if node.State != coop.NodeReview || node.UIOutcome == nil {
				continue
			}
			status := node.UIOutcome.Status
			if status != coop.UIOutcomePending && status != coop.UIOutcomeUnavailable {
				continue
			}
			if node.UIOutcome.ReportedAt != nil && now.Sub(*node.UIOutcome.ReportedAt) < outcomeActiveWindow {
				return outcomeWatchActive
			}
			interval = outcomeWatchSlow
		}
	}
	return interval
}

// --- review-card rendering ---

// reviewOutcomeLines renders the journey-outcome rows for the review card.
// Placed directly after the confirmation checks so height truncation drops
// agent metadata before it drops the gate explanation.
func (m Model) reviewOutcomeLines(nodeNumbers []int) []string {
	if m.session == nil {
		return nil
	}
	var lines []string
	for _, nodeNumber := range nodeNumbers {
		node, err := m.session.NodeByNumber(nodeNumber)
		if err != nil || node.Type != coop.NodeUIComponent {
			continue
		}
		outcome := node.UIOutcome
		if outcome == nil {
			if m.nodeIsAttestationTier(nodeNumber) {
				lines = append(lines,
					m.theme.DimmedStyle.Render("No API-observable outcome for this step."),
					m.theme.AttentionStyle.Render(`Attest: press a — "I completed this journey myself"`))
			}
			continue
		}
		switch outcome.Status {
		case coop.UIOutcomePending:
			lines = append(lines, m.theme.AttentionStyle.Render("Your turn: complete the journey in your browser"))
			if outcome.JourneyURL != "" {
				lines = append(lines, m.theme.MutedStyle.Render("Open: ")+outcome.JourneyURL+m.theme.DimmedStyle.Render("  o open"))
			}
			watching := fmt.Sprintf("Watching %s for %s", shortObjectID(outcome.ObjectID), outcome.Expect)
			lines = append(lines, m.spinner.View()+" "+m.theme.MutedStyle.Render(watching))
		case coop.UIOutcomeObserved:
			summary := "✓ Observed: " + observedSummary(outcome)
			lines = append(lines, m.theme.SuccessStyle.Render(summary))
		case coop.UIOutcomeFailed:
			lines = append(lines, m.theme.ErrorStyle.Render("✗ Outcome: "+outcome.Detail))
			lines = append(lines, m.theme.MutedStyle.Render("r request changes so the agent can redo this step"))
		case coop.UIOutcomeUnavailable:
			lines = append(lines, m.theme.DimmedStyle.Render("Outcome check unavailable: "+outcome.Detail))
			lines = append(lines, m.theme.AttentionStyle.Render(`Attest: press a — "I completed this journey myself"`))
		case coop.UIOutcomeAttested:
			label := "✓ Attested by you"
			if outcome.ResolvedAt != nil {
				label += " · " + outcome.ResolvedAt.Local().Format("15:04")
			}
			lines = append(lines, m.theme.SuccessStyle.Render(label))
		}
	}
	return lines
}

func observedSummary(outcome *coop.UIOutcome) string {
	parts := []string{}
	if outcome.Type != "" {
		parts = append(parts, outcome.Type)
	}
	parts = append(parts, shortObjectID(outcome.ObjectID))
	for _, evidence := range outcome.Evidence {
		if evidence.Key == "journey_url" {
			continue
		}
		parts = append(parts, evidence.Key+"="+evidence.Value)
	}
	summary := strings.Join(parts, " · ")
	if outcome.ResolvedAt != nil {
		summary += " · " + outcome.ResolvedAt.Local().Format("15:04")
	}
	return summary
}

func shortObjectID(id string) string {
	if len(id) <= 24 {
		return id
	}
	return id[:21] + "…"
}

// nodeIsAttestationTier reports whether the node's journey has no
// machine-checkable outcome (derived, cached per render — derivation is pure
// and cheap for the handful of review nodes on screen).
func (m Model) nodeIsAttestationTier(nodeNumber int) bool {
	expectation, ok := uicheck.DeriveExpectation(m.session, nodeNumber)
	return ok && expectation.Tier == uicheck.TierAttestation
}

// --- confirm gating & attest interactions ---

// outcomeBlockReason returns a human message when confirming the target is
// blocked by an unresolved journey outcome, or "" when confirm may proceed.
func (m Model) outcomeBlockReason(nodeNumbers []int) string {
	if m.session == nil {
		return ""
	}
	for _, nodeNumber := range nodeNumbers {
		node, err := m.session.NodeByNumber(nodeNumber)
		if err != nil || node.Type != coop.NodeUIComponent || node.UIOutcome == nil {
			continue
		}
		switch node.UIOutcome.Status {
		case coop.UIOutcomePending:
			reason := fmt.Sprintf("Confirm is locked: complete the journey in your browser (watching %s for %s)",
				shortObjectID(node.UIOutcome.ObjectID), node.UIOutcome.Expect)
			if node.UIOutcome.JourneyURL != "" {
				reason += " — press o to open it"
			}
			return reason
		case coop.UIOutcomeFailed:
			return "Confirm is locked: " + node.UIOutcome.Detail + " — press r to request changes"
		}
	}
	return ""
}

// handleAttest records an explicit human attestation for attestable nodes in
// the selected review target.
func (m *Model) handleAttest() tea.Cmd {
	if m.session == nil {
		return nil
	}
	target, ok := m.selectedReviewTarget()
	if !ok {
		return nil
	}
	if !m.targetHasAttestableNode(target.nodeNumbers) {
		// Explain the refusal instead of silently ignoring the keypress when
		// the journey has a live machine check.
		if reason := m.outcomeBlockReason(target.nodeNumbers); reason != "" {
			m.setStatus("This journey has a machine-checkable outcome — complete it in your browser instead of attesting.", 6*time.Second)
			m.resizeViewport()
			m.syncViewport()
		}
		return nil
	}
	session, err := m.workflowService().AttestOutcome(m.session.ID, target.nodeNumbers)
	if err != nil {
		m.setStatus(err.Error(), 6*time.Second)
		return nil
	}
	m.session = session
	m.lastVersion = session.Version
	m.setStatus("Attestation recorded. Press c to confirm.", 6*time.Second)
	m.resizeViewport()
	m.syncViewport()
	return nil
}

func (m Model) targetHasAttestableNode(nodeNumbers []int) bool {
	if m.session == nil {
		return false
	}
	for _, nodeNumber := range nodeNumbers {
		node, err := m.session.NodeByNumber(nodeNumber)
		if err != nil || node.Type != coop.NodeUIComponent || node.State != coop.NodeReview {
			continue
		}
		if node.UIOutcome == nil {
			if m.nodeIsAttestationTier(nodeNumber) {
				return true
			}
			continue
		}
		if node.UIOutcome.Status == coop.UIOutcomeUnavailable {
			return true
		}
	}
	return false
}

// selectedJourneyURL returns the journey URL for the selected review target,
// preferred by the contextual open key over the sandbox claim URL.
func (m Model) selectedJourneyURL() string {
	if m.session == nil {
		return ""
	}
	target, ok := m.selectedReviewTarget()
	if !ok {
		return ""
	}
	for _, nodeNumber := range target.nodeNumbers {
		node, err := m.session.NodeByNumber(nodeNumber)
		if err != nil || node.UIOutcome == nil {
			continue
		}
		if node.UIOutcome.Status == coop.UIOutcomePending && node.UIOutcome.JourneyURL != "" {
			return node.UIOutcome.JourneyURL
		}
	}
	return ""
}
