package tui

import (
	"fmt"
	"math"
	"strings"
	"time"

	"charm.land/bubbles/v2/help"
	"charm.land/bubbles/v2/key"
	"charm.land/bubbles/v2/spinner"
	"charm.land/bubbles/v2/textarea"
	"charm.land/bubbles/v2/viewport"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/stripe/stripe-cli/pkg/coop"
	"github.com/stripe/stripe-cli/pkg/coop/workflow"
)

// Model is the root bubbletea model for the co-op TUI.
type Model struct {
	store                   *coop.Store
	sessionID               string
	session                 *coop.Session
	lastVersion             int
	sandboxClaimURL         string
	sandboxClaimURLProvider func() string

	selectionCursor int // node index in work view, completion option index in completion view
	selected        navigationItem
	collapsedSteps  map[int]bool
	expanded        bool
	detailTab       int
	width           int
	height          int
	userMoved       bool

	rejecting       bool // true while the request-changes input is active
	rejectTarget    reviewTarget
	rejectionInput  textarea.Model
	rejectionError  string
	statusMessage   string
	statusExpiresAt time.Time

	keys  keyMap
	help  help.Model
	theme Theme

	viewport viewport.Model
	ready    bool

	// outlineWidthOverride, when > 0, constrains outline rule and wrap widths to a
	// specific column width instead of the full content width. Used by the split
	// workspace so dividers and wrapped text fit the narrow left column.
	outlineWidthOverride int

	spinner        spinner.Model
	err            error
	sdkSnippet     string
	sdkSnippetNode int
	sdkLoading     bool
	sdkLoadingNode int

	waiting            bool
	waitingMessage     string
	existingSessionIDs map[string]bool
	lastUpdateTime     time.Time
	agentIsIdle        bool
	agentPulseSeen     bool
	agentProcessActive bool
	observer           ObserverController

	isDark bool
}

func newThemedSpinner(t Theme) spinner.Model {
	return spinner.New(
		spinner.WithSpinner(spinner.MiniDot),
		spinner.WithStyle(lipgloss.NewStyle().Foreground(t.Purple500)),
	)
}

func newThemedRejectionInput(t Theme) textarea.Model {
	ti := textarea.New()
	ti.Prompt = ""
	ti.Placeholder = "Describe what to change..."
	ti.ShowLineNumbers = false
	ti.EndOfBufferCharacter = 0
	ti.DynamicHeight = true
	ti.MinHeight = 1
	ti.MaxHeight = 3
	// Bubbles treats MaxHeight as an input limit unless MaxContentHeight is set.
	// Keep the viewport compact without truncating longer feedback.
	ti.MaxContentHeight = math.MaxInt
	ti.SetVirtualCursor(false)
	ti.SetWidth(60)
	styles := ti.Styles()
	styles.Focused.Placeholder = lipgloss.NewStyle().Foreground(t.Gray500).Italic(true)
	styles.Focused.Text = lipgloss.NewStyle().Foreground(t.Text)
	styles.Focused.CursorLine = lipgloss.NewStyle()
	styles.Focused.Base = lipgloss.NewStyle()
	styles.Blurred = styles.Focused
	ti.SetStyles(styles)
	return ti
}

func (m *Model) applyTheme(isDark bool) {
	m.isDark = isDark
	m.theme = NewTheme(isDark)
	m.spinner.Style = lipgloss.NewStyle().Foreground(m.theme.Purple500)
	m.rejectionInput.SetStyles(newThemedRejectionInput(m.theme).Styles())
	m.help = newThemedHelp(m.theme)
}

// NewModel creates a TUI model for a known session.
func NewModel(store *coop.Store, sessionID string, opts ...Option) Model {
	t := NewTheme(true)

	m := Model{
		store:          store,
		sessionID:      sessionID,
		spinner:        newThemedSpinner(t),
		rejectionInput: newThemedRejectionInput(t),
		keys:           newKeyMap(),
		help:           newThemedHelp(t),
		theme:          t,
		isDark:         true,
		sdkSnippetNode: -1,
		sdkLoadingNode: -1,
	}
	for _, opt := range opts {
		opt(&m)
	}
	return m
}

// NewWaitingModel creates a TUI model that waits for a new session to appear.
func NewWaitingModel(store *coop.Store, existingSessionIDs map[string]bool, opts ...Option) Model {
	t := NewTheme(true)

	m := Model{
		store:              store,
		spinner:            newThemedSpinner(t),
		rejectionInput:     newThemedRejectionInput(t),
		keys:               newKeyMap(),
		help:               newThemedHelp(t),
		theme:              t,
		isDark:             true,
		sdkSnippetNode:     -1,
		sdkLoadingNode:     -1,
		waiting:            true,
		existingSessionIDs: existingSessionIDs,
	}
	for _, opt := range opts {
		opt(&m)
	}
	return m
}

func (m Model) Init() tea.Cmd {
	if m.waiting {
		return tea.Batch(m.spinner.Tick, tickCmd(), tea.RequestBackgroundColor)
	}
	return tea.Batch(m.loadSession(), m.spinner.Tick, tickCmd(), tea.RequestBackgroundColor)
}

func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	var cmds []tea.Cmd

	switch msg := msg.(type) {
	case tea.KeyPressMsg:
		return m.handleKey(msg)

	case tea.MouseWheelMsg:
		m.userMoved = true
		var cmd tea.Cmd
		m.viewport, cmd = m.viewport.Update(msg)
		return m, cmd

	case mouseActionMsg:
		return m.handleMouseAction(msg)

	case tea.WindowSizeMsg:
		m.handleWindowSize(msg)
		return m, nil

	case tickMsg:
		m.clearExpiredStatus(time.Now())
		return m, tea.Batch(m.checkForUpdates(), tickCmd())

	case noUpdateMsg:
		m.updateAgentProcessPulse(msg.agentPulseAge, msg.agentPulseOK)
		m.updateAgentIdle(msg.heartbeatAge, msg.heartbeatOK, time.Now())
		return m, nil

	case waitingBaselineMsg:
		if msg.err != nil {
			m.err = fmt.Errorf("failed to snapshot existing sessions: %w", msg.err)
			return m, nil
		}
		m.existingSessionIDs = msg.existingSessionIDs
		return m, nil

	case sessionDiscoveredMsg:
		m.waiting = false
		m.waitingMessage = ""
		m.sessionID = msg.sessionID
		m.resetSessionViewState()
		if m.observer != nil {
			m.observer.Start(msg.sessionID)
		}
		return m, m.loadSession()

	case sessionUpdatedMsg:
		return m.applySessionUpdate(msg)

	case errMsg:
		m.err = msg.err
		return m, nil

	case statusMsg:
		m.setStatus(msg.message, msg.ttl)
		m.resizeViewport()
		m.syncViewport()
		return m, nil

	case sdkSnippetMsg:
		if msg.step == m.sdkLoadingNode {
			m.sdkLoading = false
			m.sdkLoadingNode = -1
		}
		if msg.err == nil && msg.step == m.selectionCursor {
			m.sdkSnippet = msg.snippet
			m.sdkSnippetNode = msg.step
		}
		m.syncViewport()
		return m, nil

	case spinner.TickMsg:
		var cmd tea.Cmd
		m.spinner, cmd = m.spinner.Update(msg)
		m.syncViewport()
		cmds = append(cmds, cmd)
		return m, tea.Batch(cmds...)

	case tea.BackgroundColorMsg:
		m.applyTheme(msg.IsDark())
		m.resizeViewport()
		m.syncViewport()
		return m, nil
	}

	return m.updateRejectionInputIfActive(msg)
}

func (m Model) updateRejectionInputIfActive(msg tea.Msg) (tea.Model, tea.Cmd) {
	if !m.rejecting {
		return m, nil
	}
	return m.updateRejectionInput(msg)
}

func (m Model) applySessionUpdate(msg sessionUpdatedMsg) (tea.Model, tea.Cmd) {
	if msg.session == nil || msg.session.ID != m.sessionID {
		return m, nil
	}
	if m.session != nil && msg.session.ID == m.session.ID && msg.session.Version <= m.lastVersion {
		return m, nil
	}
	wasComplete := m.session != nil && m.session.IsComplete()
	m.session = msg.session
	m.lastVersion = msg.session.Version
	m.lastUpdateTime = time.Now()
	m.agentIsIdle = false

	// Child session completed → return to parent with step marked done.
	if !wasComplete && m.session.IsComplete() && m.session.ParentSessionID != "" {
		return m, m.returnToParent()
	}
	if !wasComplete && m.session.IsComplete() {
		m.resetSelectionState()
		m.clearStatus()
		m.clearRejectionState()
		if m.ready {
			m.viewport.SetYOffset(0)
		}
	}
	if !m.userMoved {
		m.autoScroll()
	}
	m.resizeViewport()
	m.syncViewport()
	return m, nil
}

func (m Model) View() tea.View {
	var content string
	switch {
	case m.err != nil:
		content = m.theme.ErrorStyle.Render(fmt.Sprintf("Error: %s", m.err))
	case !m.ready:
		content = m.spinner.View() + " Loading..."
	case m.waiting:
		content = m.renderWaitingView()
	case m.session == nil:
		content = m.renderWaitingView()
	case m.session.IsComplete():
		content = m.renderCompletionView()
	default:
		header := m.renderHeader()
		content = m.renderPinnedViewport(header, m.renderFooter())
	}

	v := tea.NewView(content)
	v.AltScreen = true
	v.MouseMode = tea.MouseModeCellMotion
	v.OnMouse = func(msg tea.MouseMsg) tea.Cmd {
		if action, ok := m.mouseActionFor(msg.Mouse()); ok {
			return func() tea.Msg {
				return action
			}
		}
		return nil
	}
	v.KeyboardEnhancements.ReportEventTypes = true
	v.ProgressBar = m.progressBar()
	v.Cursor = m.rejectionCursor(content)
	if m.session != nil {
		done := 0
		for _, ch := range m.session.Steps {
			for _, n := range ch.Nodes {
				if n.State == coop.NodeDone || n.State == coop.NodeSkipped {
					done++
				}
			}
		}
		v.WindowTitle = fmt.Sprintf("Co-op: %s (%d/%d)", m.session.Blueprint, done, m.session.TotalNodes())
	} else {
		v.WindowTitle = "Stripe Co-op"
	}
	return v
}

func (m Model) progressBar() *tea.ProgressBar {
	if m.err != nil {
		return tea.NewProgressBar(tea.ProgressBarError, 100)
	}
	if m.waiting || m.session == nil {
		return tea.NewProgressBar(tea.ProgressBarIndeterminate, 0)
	}
	total := 0
	done := 0
	for _, ch := range m.session.Steps {
		for _, n := range ch.Nodes {
			if n.State == coop.NodeSkipped {
				continue
			}
			total++
			if n.State == coop.NodeDone {
				done++
			}
		}
	}
	if total == 0 {
		return tea.NewProgressBar(tea.ProgressBarNone, 0)
	}
	value := done * 100 / total
	state := tea.ProgressBarDefault
	if m.agentIdle() {
		state = tea.ProgressBarWarning
	}
	return tea.NewProgressBar(state, value)
}

func (m Model) rejectionCursor(content string) *tea.Cursor {
	if !m.rejecting {
		return nil
	}
	lines := strings.Split(content, "\n")
	for y, line := range lines {
		plain := ansi.Strip(line)
		const prefix = "Request changes: "
		idx := strings.Index(plain, prefix)
		if idx < 0 {
			continue
		}
		cursor := m.rejectionInput.Cursor()
		if cursor == nil {
			cursor = tea.NewCursor(0, 0)
		}
		if cursor.Y == 0 {
			cursor.X += lipgloss.Width(plain[:idx+len(prefix)])
		} else {
			cursor.X += lipgloss.Width(plain[:idx])
		}
		cursor.Y += y
		cursor.Shape = tea.CursorBar
		cursor.Color = m.theme.Purple500
		cursor.Blink = true
		return cursor
	}
	return nil
}

// --- State management ---

func (m *Model) resetSessionViewState() {
	m.resetSelectionState()
	m.clearRejectionState()
	m.clearStatus()
	m.clearSDKSnippetState()
	m.agentPulseSeen = false
	m.agentProcessActive = false
	m.agentIsIdle = false
}

func (m *Model) resetSelectionState() {
	m.selectionCursor = 0
	m.selected = navigationItem{}
	m.collapsedSteps = nil
	m.expanded = false
	m.userMoved = false
}

func (m *Model) clearRejectionState() {
	m.rejecting = false
	m.rejectTarget = reviewTarget{}
	m.rejectionInput.SetValue("")
	m.rejectionInput.Blur()
	m.rejectionError = ""
}

func (m *Model) clearStatus() {
	m.statusMessage = ""
	m.statusExpiresAt = time.Time{}
}

func (m *Model) clearSDKSnippetState() {
	m.sdkSnippet = ""
	m.sdkSnippetNode = -1
	m.sdkLoading = false
	m.sdkLoadingNode = -1
}

func (m *Model) handleWindowSize(msg tea.WindowSizeMsg) {
	m.width = msg.Width
	m.height = msg.Height
	if !m.ready {
		m.viewport = viewport.New(viewport.WithWidth(msg.Width), viewport.WithHeight(10))
		m.viewport.MouseWheelEnabled = true
		m.viewport.MouseWheelDelta = 3
		m.viewport.FillHeight = true
		m.viewport.SoftWrap = true
		m.ready = true
	}
	m.resizeViewport()
	m.syncViewport()
}

func (m *Model) resizeViewport() {
	if !m.ready || m.height == 0 {
		return
	}
	headerH := lipgloss.Height(m.renderHeader()) + 1
	footerH := lipgloss.Height(m.renderFooter()) + 1
	if m.session != nil && m.session.IsComplete() {
		footerH = lipgloss.Height(m.renderCompletionFooter()) + 1
	}
	vpHeight := m.height - headerH - footerH - terminalScrollGuard
	if vpHeight < minViewportHeight {
		vpHeight = minViewportHeight
	}
	m.viewport.SetWidth(m.width)
	m.viewport.SetHeight(vpHeight)
	m.viewport.YPosition = lipgloss.Height(m.renderHeader())
	if m.rejecting {
		m.rejectionInput.SetWidth(m.requestChangesInputWidth())
		m.rejectionInput.SetHeight(m.rejectionInputHeight())
	}
}

func (m Model) rejectionInputHeight() int {
	if m.height <= 0 {
		return 3
	}
	// Rejection mode renders only the textarea, an optional error, and the
	// one-line action footer. Always reserve at least one visible input row.
	available := m.footerHeightBudget() - 1
	if m.rejectionError != "" {
		available--
	}
	return max(1, min(3, available))
}

func (m *Model) syncViewport() {
	if !m.ready || m.session == nil {
		return
	}
	if m.session.IsComplete() {
		content := m.renderCompletionBody()
		m.viewport.SetContent(content)
		m.ensureCompletionCursorVisible()
		return
	}
	m.ensureValidNavigationSelection()
	content := m.renderStepList()
	m.viewport.SetContent(content)
	if !m.userMoved {
		m.scrollToCursor()
	}
}

func (m *Model) scrollToCursor() {
	targetLine := m.selectedContentLine()
	m.viewport.EnsureVisible(targetLine, 0, 0)

	vpTop := m.viewport.YOffset()
	visibleHeight := m.viewport.Height()
	if m.viewport.TotalLineCount() > visibleHeight && visibleHeight >= 3 {
		visibleHeight -= 2
	}
	vpBottom := vpTop + visibleHeight
	scrollThreshold := vpBottom - 2
	if m.session != nil && m.session.IsComplete() {
		scrollThreshold = vpBottom
	}

	if targetLine < vpTop {
		m.viewport.SetYOffset(targetLine)
	} else if targetLine >= scrollThreshold {
		offset := targetLine - visibleHeight/2
		if offset < 0 {
			offset = 0
		}
		m.viewport.SetYOffset(offset)
	}
}

func (m *Model) ensureCompletionCursorVisible() {
	line, ok := m.completionLineForCursor()
	if !ok {
		return
	}
	m.viewport.EnsureVisible(line, 0, 0)
}

func (m Model) completionLineForCursor() (int, bool) {
	if m.selectionCursor < 0 {
		return 0, false
	}
	for line, suggestion := range m.completionSuggestionLines() {
		if suggestion == m.selectionCursor {
			return line, true
		}
	}
	return 0, false
}

func (m Model) selectedContentLine() int {
	if m.session != nil && !m.session.IsComplete() {
		selectedLine := -1
		for line, item := range m.navigationContentLines() {
			if m.navigationItemSelected(item) && (selectedLine == -1 || line < selectedLine) {
				selectedLine = line
			}
		}
		if selectedLine >= 0 {
			return selectedLine
		}
	}
	content := m.renderStepList()
	if m.session != nil && m.session.IsComplete() {
		content = m.renderCompletionBody()
	}
	lines := strings.Split(content, "\n")
	for i, line := range lines {
		if strings.Contains(line, cursorMarker) {
			return i
		}
	}
	return 0
}

func (m *Model) autoScroll() {
	if m.session == nil {
		return
	}
	for i := range m.session.Steps {
		if m.stepReviewReady(i) {
			m.selectStep(i)
			m.expanded = false
			return
		}
	}
	idx := 0
	for i := range m.session.Steps {
		for j := range m.session.Steps[i].Nodes {
			if m.session.Steps[i].Nodes[j].State == coop.NodeReview && m.reviewIsActionable(idx+1) {
				m.selectNode(idx)
				m.expanded = false
				return
			}
			idx++
		}
	}
	_, activeNum := m.session.ActiveNode()
	if activeNum > 0 {
		m.selectNode(activeNum - 1)
	}
}

func (m Model) handleKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	if m.rejecting {
		return m.handleRejectionKey(msg)
	}
	// In the completion view only suggestion navigation, selection, quit, and
	// claim-open are meaningful. Gate everything else so work-view keys (expand,
	// collapse, confirm, follow, etc.) can't leak in and mutate hidden state or
	// fire stray commands against a completion cursor reinterpreted as a node.
	if m.session != nil && m.session.IsComplete() {
		return m.handleCompletionKey(msg)
	}
	if msg.IsRepeat && (key.Matches(msg, m.keys.Confirm) || key.Matches(msg, m.keys.Reject) || key.Matches(msg, m.keys.Copy) || key.Matches(msg, m.keys.OpenClaim)) {
		return m, nil
	}

	if next, cmd, ok := m.handleNavigationKey(msg); ok {
		return next, cmd
	}
	if next, cmd, ok := m.handleViewportKey(msg); ok {
		return next, cmd
	}
	return m.handleActionKey(msg)
}

// handleCompletionKey handles the limited key set valid in the completion view.
func (m Model) handleCompletionKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	switch {
	case key.Matches(msg, m.keys.Up):
		m.moveCursorUp()
		m.resizeViewport()
		m.syncViewport()
		return m, nil
	case key.Matches(msg, m.keys.Down):
		m.moveCursorDown()
		m.resizeViewport()
		m.syncViewport()
		return m, nil
	case key.Matches(msg, m.keys.Enter):
		return m.handleEnter()
	case key.Matches(msg, m.keys.OpenClaim):
		if _, ok := m.selectedAppSurface(); ok {
			return m, m.openSelectedApp()
		}
		if claimURL := m.sandboxClaimLink(); claimURL != "" {
			return m, openBrowserCmd(claimURL)
		}
		return m, nil
	case key.Matches(msg, m.keys.Quit):
		return m, tea.Quit
	}
	return m, nil
}

func (m Model) handleNavigationKey(msg tea.KeyPressMsg) (Model, tea.Cmd, bool) {
	switch {
	case key.Matches(msg, m.keys.Up):
		m.moveCursorUp()
		m.resizeViewport()
		m.syncViewport()
		return m, nil, true
	case key.Matches(msg, m.keys.Down):
		m.moveCursorDown()
		m.resizeViewport()
		m.syncViewport()
		return m, nil, true
	case key.Matches(msg, m.keys.Left):
		if m.collapseSelectedStep() {
			m.userMoved = true
			m.expanded = false
			m.resizeViewport()
			m.syncViewport()
		}
		return m, nil, true
	case key.Matches(msg, m.keys.Right):
		if m.expandSelectedStep() {
			m.userMoved = true
			m.resizeViewport()
			m.syncViewport()
		}
		return m, nil, true
	}
	return m, nil, false
}

func (m Model) handleViewportKey(msg tea.KeyPressMsg) (Model, tea.Cmd, bool) {
	switch {
	case key.Matches(msg, m.keys.PageUp):
		m.userMoved = true
		m.viewport.PageUp()
		return m, nil, true
	case key.Matches(msg, m.keys.PageDown):
		m.userMoved = true
		m.viewport.PageDown()
		return m, nil, true
	case key.Matches(msg, m.keys.Top):
		m.userMoved = true
		if items := m.navigationItems(); len(items) > 0 {
			m.selectNavigationItem(items[0])
		}
		m.resizeViewport()
		m.syncViewport()
		m.viewport.GotoTop()
		return m, nil, true
	case key.Matches(msg, m.keys.Bottom):
		m.userMoved = true
		if items := m.navigationItems(); len(items) > 0 {
			m.selectNavigationItem(items[len(items)-1])
		}
		m.resizeViewport()
		m.syncViewport()
		m.viewport.GotoBottom()
		return m, nil, true
	}
	return m, nil, false
}

func (m Model) handleActionKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	switch {
	case key.Matches(msg, m.keys.Quit):
		return m, tea.Quit
	case key.Matches(msg, m.keys.Expand):
		m.expanded = !m.expanded
		m.resizeViewport()
		m.syncViewport()
		if m.expanded {
			return m, m.fetchSnippetIfNeeded()
		}
		return m, nil
	case key.Matches(msg, m.keys.Enter):
		return m.handleEnter()
	case key.Matches(msg, m.keys.Tab):
		if m.expanded {
			m.detailTab = (m.detailTab + 1) % len(detailSections)
			m.syncViewport()
			return m, m.fetchSnippetIfNeeded()
		}
		return m, nil
	case key.Matches(msg, m.keys.Escape):
		if m.expanded {
			m.expanded = false
			m.resizeViewport()
			m.syncViewport()
		}
		return m, nil
	case key.Matches(msg, m.keys.Follow):
		m.userMoved = false
		m.autoScroll()
		m.setStatus("Following the current review step", 3*time.Second)
		m.resizeViewport()
		m.syncViewport()
		return m, nil
	case key.Matches(msg, m.keys.Confirm):
		return m, m.handleConfirm()
	case key.Matches(msg, m.keys.OpenClaim):
		if _, ok := m.selectedAppSurface(); ok {
			return m, m.openSelectedApp()
		}
		if claimURL := m.sandboxClaimLink(); claimURL != "" {
			return m, openBrowserCmd(claimURL)
		}
		return m, nil
	case key.Matches(msg, m.keys.Copy):
		if command := m.selectedReviewCommand(); command != "" {
			m.setStatus("Copied review command.", 3*time.Second)
			m.resizeViewport()
			m.syncViewport()
			return m, tea.SetClipboard(command)
		}
		return m, nil
	case key.Matches(msg, m.keys.Reject):
		return m, m.startReject()
	}
	return m, nil
}

func (m *Model) moveCursorUp() {
	if m.session != nil && m.session.IsComplete() {
		suggestions := m.getCompletionSuggestions()
		if len(suggestions) == 0 {
			return
		}
		if m.selectionCursor > 0 {
			m.selectionCursor--
		} else {
			m.selectionCursor = len(suggestions) - 1
		}
	} else {
		items := m.navigationItems()
		if len(items) == 0 {
			return
		}
		idx := m.selectedNavigationIndex()
		if idx <= 0 {
			return
		}
		m.selectNavigationItem(items[idx-1])
		m.userMoved = true
	}
}

func (m *Model) moveCursorDown() {
	if m.session != nil && m.session.IsComplete() {
		suggestions := m.getCompletionSuggestions()
		if len(suggestions) == 0 {
			return
		}
		if m.selectionCursor < len(suggestions)-1 {
			m.selectionCursor++
		} else {
			m.selectionCursor = 0
		}
	} else {
		items := m.navigationItems()
		if len(items) == 0 {
			return
		}
		idx := m.selectedNavigationIndex()
		if idx < 0 || idx >= len(items)-1 {
			return
		}
		m.selectNavigationItem(items[idx+1])
		m.userMoved = true
	}
}

func (m Model) handleEnter() (tea.Model, tea.Cmd) {
	if m.session != nil && m.session.IsComplete() {
		suggestions := m.getCompletionSuggestions()
		if m.selectionCursor < len(suggestions) {
			selected := suggestions[m.selectionCursor]
			cmd := m.selectCompletionOption()

			switch selected.id {
			case "deploy", "deploy-update":
				waitCmd := m.enterWaitingMode("Waiting for agent to start the guided deploy flow...")
				return m, tea.Batch(cmd, waitCmd)
			case "add-integration":
				waitCmd := m.enterWaitingMode("Waiting for agent to ask which Stripe feature to add...")
				return m, tea.Batch(cmd, waitCmd)
			default:
				if selected.id == "summarize" {
					m.statusMessage = "Waiting for agent to write STRIPE.md..."
					m.syncViewport()
				}
				return m, cmd
			}
		}
		return m, nil
	}
	m.expanded = !m.expanded
	m.resizeViewport()
	m.syncViewport()
	if m.expanded {
		return m, m.fetchSnippetIfNeeded()
	}
	return m, nil
}

func (m *Model) enterWaitingMode(message string) tea.Cmd {
	m.waiting = true
	m.waitingMessage = message
	m.session = nil
	m.resetSessionViewState()
	m.existingSessionIDs = nil
	return m.snapshotWaitingBaseline()
}

func (m *Model) handleConfirm() tea.Cmd {
	if m.session == nil {
		return nil
	}
	target, ok := m.selectedConfirmationTarget()
	if !ok {
		return nil
	}
	refs, err := m.attemptRefs(target.nodeNumbers)
	if err != nil {
		m.setStatus(err.Error(), 5*time.Second)
		return nil
	}
	readiness, err := workflow.ReviewAttemptsReadiness(m.session, refs)
	if err != nil {
		m.setStatus(err.Error(), 0)
		m.resizeViewport()
		m.syncViewport()
		return nil
	}
	if len(readiness.Blocking) > 0 {
		m.setStatus("Co-op found a contradiction that needs agent changes.", 0)
		m.resizeViewport()
		m.syncViewport()
		return nil
	}
	session, err := workflow.NewService(m.store).ConfirmReviewAttempts(m.session.ID, refs)
	if err != nil {
		m.setStatus(err.Error(), 5*time.Second)
		m.resizeViewport()
		m.syncViewport()
		return nil
	}
	m.session = session
	m.lastVersion = m.session.Version
	if target.kind == "node" && len(target.nodeNumbers) > 0 {
		m.selectNode(target.nodeNumbers[0] - 1)
	}
	m.userMoved = false
	if readiness.Incomplete {
		m.setStatus("Confirmed with limited automatic coverage. Agent can continue.", 5*time.Second)
	} else {
		m.setStatus("Confirmed. Agent can continue.", 5*time.Second)
	}
	m.clearRejectionState()
	if m.session.IsComplete() {
		m.resetSelectionState()
		m.clearStatus()
	} else {
		m.autoScroll()
	}
	m.resizeViewport()
	m.syncViewport()
	if m.session.IsComplete() && m.session.ParentSessionID != "" {
		return m.returnToParent()
	}
	return nil
}

func (m *Model) startReject() tea.Cmd {
	if m.session == nil {
		return nil
	}
	if target, ok := m.selectedRejectionTarget(); ok {
		m.rejecting = true
		m.rejectTarget = target
		m.rejectionInput.SetValue("")
		m.rejectionInput.Placeholder = m.requestChangesPlaceholder(target)
		m.rejectionError = ""
		m.clearStatus()
		m.resizeViewport()
		m.syncViewport()
		return m.rejectionInput.Focus()
	}
	return nil
}

func (m Model) handleRejectionKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	switch {
	case key.Matches(msg, m.keys.Escape):
		m.clearRejectionState()
		m.setStatus("Request changes canceled.", 3*time.Second)
		m.resizeViewport()
		m.syncViewport()
		return m, nil
	case key.Matches(msg, m.keys.Submit):
		m.handleReject(strings.TrimSpace(m.rejectionInput.Value()))
		return m, nil
	}
	return m.updateRejectionInput(msg)
}

func (m Model) updateRejectionInput(msg tea.Msg) (tea.Model, tea.Cmd) {
	var cmd tea.Cmd
	m.rejectionInput, cmd = m.rejectionInput.Update(msg)
	m.rejectionError = ""
	m.resizeViewport()
	m.syncViewport()
	return m, cmd
}

func (m *Model) handleReject(note string) {
	if m.session == nil {
		return
	}
	if note == "" {
		m.rejectionError = "Add a short note so the agent knows what to change."
		m.resizeViewport()
		m.syncViewport()
		return
	}
	if !m.reviewTargetStillValid(m.rejectTarget) {
		m.clearRejectionState()
		m.setStatus("Review target changed. Request changes canceled.", 4*time.Second)
		m.resizeViewport()
		m.syncViewport()
		return
	}
	target := m.rejectTarget
	refs, err := m.attemptRefs(target.nodeNumbers)
	if err != nil {
		m.rejectionError = err.Error()
		return
	}
	session, err := workflow.NewService(m.store).RequestChangesAttempts(m.session.ID, refs, note)
	if err != nil {
		m.rejectionError = fmt.Sprintf("Could not send feedback: %v", err)
		m.resizeViewport()
		m.syncViewport()
		return
	}
	m.session = session
	m.lastVersion = m.session.Version
	if target.kind == "node" && len(target.nodeNumbers) > 0 {
		m.selectNode(target.nodeNumbers[0] - 1)
	}
	m.userMoved = false
	m.clearRejectionState()
	m.setStatus("Feedback sent. Waiting for agent...", 5*time.Second)
	m.resizeViewport()
	m.syncViewport()
}

func (m Model) attemptRefs(nodeNumbers []int) ([]workflow.AttemptRef, error) {
	refs := make([]workflow.AttemptRef, 0, len(nodeNumbers))
	for _, nodeNumber := range nodeNumbers {
		node, err := m.session.NodeByNumber(nodeNumber)
		if err != nil {
			return nil, err
		}
		attempt := node.CurrentAttempt()
		if attempt == nil {
			return nil, fmt.Errorf("node %d no longer has an open review attempt", nodeNumber)
		}
		refs = append(refs, workflow.AttemptRef{Node: nodeNumber, Attempt: attempt.Number})
	}
	return refs, nil
}

type appSurfaceSelection struct {
	node    int
	attempt int
	url     string
	title   string
	opened  bool
}

func (m Model) selectedAppSurface() (appSurfaceSelection, bool) {
	if m.session == nil {
		return appSurfaceSelection{}, false
	}
	var candidates []int
	seen := map[int]bool{}
	addCandidate := func(candidate int) {
		if candidate > 0 && !seen[candidate] {
			seen[candidate] = true
			candidates = append(candidates, candidate)
		}
	}
	// Prefer the concrete outline node. Selecting a UI node must show and open
	// that node's submitted surface, even when multiple UI nodes share a step.
	if index, found := m.selectedNodeIndex(); found {
		addCandidate(index + 1)
	}
	if stepIndex, found := m.selectedStepIndex(); found {
		current := 0
		for index := range m.session.Steps {
			for range m.session.Steps[index].Nodes {
				current++
				if index == stepIndex {
					addCandidate(current)
				}
			}
		}
	}
	if target, found := m.selectedReviewTarget(); found {
		for _, candidate := range target.nodeNumbers {
			addCandidate(candidate)
		}
	}
	// Keep the handoff visible while the agent continues later work. A user
	// should not need to rediscover the earlier UI node before pressing "o".
	for _, surface := range m.exerciseReadyAppSurfaces() {
		addCandidate(surface.node)
	}
	for _, candidate := range candidates {
		if surface, ok := m.appSurfaceForNode(candidate); ok {
			return surface, true
		}
	}
	return appSurfaceSelection{}, false
}

func (m Model) appSurfaceForNode(nodeNumber int) (appSurfaceSelection, bool) {
	node, err := m.session.NodeByNumber(nodeNumber)
	if err != nil || node.Type != coop.NodeUIComponent || node.State != coop.NodeReview {
		return appSurfaceSelection{}, false
	}
	attempt := node.CurrentAttempt()
	if attempt == nil || attempt.AppSurface == nil || attempt.AppSurface.URL == "" {
		return appSurfaceSelection{}, false
	}
	return appSurfaceSelection{
		node: nodeNumber, attempt: attempt.Number, url: attempt.AppSurface.URL, title: node.Title,
		opened: attempt.AppSurface.OpenedAt != nil,
	}, true
}

func (m Model) exerciseReadyAppSurfaces() []appSurfaceSelection {
	if m.session == nil {
		return nil
	}
	var surfaces []appSurfaceSelection
	nodeNumber := 0
	for stepIndex := range m.session.Steps {
		for range m.session.Steps[stepIndex].Nodes {
			nodeNumber++
			if surface, ok := m.appSurfaceForNode(nodeNumber); ok {
				surfaces = append(surfaces, surface)
			}
		}
	}
	return surfaces
}

func (m Model) appExerciseCallout() string {
	surfaces := m.exerciseReadyAppSurfaces()
	if len(surfaces) == 0 {
		return ""
	}
	surface, ok := m.selectedAppSurface()
	if !ok {
		surface = surfaces[0]
	}
	state := "Ready to exercise"
	if surface.opened {
		state = "UI review in progress"
	}
	label := state + ": "
	if surface.title != "" {
		label += surface.title + " — "
	}
	label += surface.url + "  (press o, optional)"
	if len(surfaces) > 1 {
		label += fmt.Sprintf("  · %d app surfaces ready; select a UI node to choose", len(surfaces))
	}
	return safeEvidenceText(label)
}

func (m *Model) openSelectedApp() tea.Cmd {
	selection, ok := m.selectedAppSurface()
	if !ok {
		return nil
	}
	appURL, err := workflow.NewService(m.store).MarkAppOpened(m.session.ID, selection.node, selection.attempt)
	if err != nil {
		m.setStatus("Could not record app open: "+err.Error(), 5*time.Second)
		return nil
	}
	// MarkAppOpened commits the timestamp before this command invokes the OS.
	if session, readErr := m.store.Read(m.session.ID); readErr == nil {
		m.session = session
		m.lastVersion = session.Version
	}
	m.setStatus("Opening the app. Exercise the visible flow; request changes immediately if it is wrong, or confirm when the step is ready.", 5*time.Second)
	return openBrowserCmd(appURL)
}

type reviewTarget struct {
	title       string
	kind        string
	nodeNumbers []int
	stepIndex   int
}

func (m Model) selectedReviewTarget() (reviewTarget, bool) {
	if m.session == nil {
		return reviewTarget{}, false
	}
	if m.selected.kind == navigationStep {
		stepIndex := m.selected.stepIndex
		if !m.stepReviewReady(stepIndex) {
			return reviewTarget{}, false
		}
		ch := m.session.Steps[stepIndex]
		var nodeNumbers []int
		step := 0
		for i := range m.session.Steps {
			for j := range m.session.Steps[i].Nodes {
				step++
				if i == stepIndex && m.session.Steps[i].Nodes[j].State == coop.NodeReview {
					nodeNumbers = append(nodeNumbers, step)
				}
			}
		}
		if len(nodeNumbers) == 0 {
			return reviewTarget{}, false
		}
		return reviewTarget{title: ch.Title, kind: "step", nodeNumbers: nodeNumbers, stepIndex: stepIndex}, true
	}
	nodeIndex, ok := m.selectedNodeIndex()
	if !ok {
		return reviewTarget{}, false
	}
	nodeNumber := nodeIndex + 1
	node, err := m.session.NodeByNumber(nodeNumber)
	if err != nil || node.State != coop.NodeReview {
		return reviewTarget{}, false
	}
	_, stepIndex, _, err := m.session.StepByNodeNumber(nodeNumber)
	if err != nil {
		return reviewTarget{}, false
	}
	// A submitted app surface is independently reviewable while the agent
	// continues a sibling. Confirming it does not finish the containing step.
	if node.Type != coop.NodeUIComponent && !m.session.StepReadyForReview(stepIndex) {
		return reviewTarget{}, false
	}
	return reviewTarget{title: node.Title, kind: "node", nodeNumbers: []int{nodeNumber}, stepIndex: stepIndex}, true
}

// selectedConfirmationTarget expands a ready containing step into one atomic
// confirmation batch. A UI submitted before its siblings finish remains an
// exact node target so the developer can review it without blocking the agent.
func (m Model) selectedConfirmationTarget() (reviewTarget, bool) {
	target, ok := m.selectedReviewTarget()
	if !ok || target.kind == "step" || !m.stepReviewReady(target.stepIndex) {
		return target, ok
	}

	step := 0
	var nodeNumbers []int
	for i := range m.session.Steps {
		for j := range m.session.Steps[i].Nodes {
			step++
			if i == target.stepIndex && m.session.Steps[i].Nodes[j].State == coop.NodeReview {
				nodeNumbers = append(nodeNumbers, step)
			}
		}
	}
	if len(nodeNumbers) <= 1 {
		return target, true
	}
	return reviewTarget{
		title:       m.session.Steps[target.stepIndex].Title,
		kind:        "step",
		nodeNumbers: nodeNumbers,
		stepIndex:   target.stepIndex,
	}, true
}

func (m Model) confirmationIsAvailable(target reviewTarget) bool {
	refs, err := m.attemptRefs(target.nodeNumbers)
	if err != nil {
		// Keep the action visible for recovered or partially populated local
		// session data. The atomic workflow call will return the precise error.
		return true
	}
	readiness, err := workflow.ReviewAttemptsReadiness(m.session, refs)
	// Selection already guarantees a review node. If a synthetic or recovered
	// session lacks enough attempt metadata for the richer projection, keep the
	// action visible and let the atomic service return a durable explanation.
	return err != nil || len(readiness.Blocking) == 0
}

func (m Model) selectedRejectionTarget() (reviewTarget, bool) {
	if target, ok := m.selectedReviewTarget(); ok {
		return target, true
	}
	if m.session == nil {
		return reviewTarget{}, false
	}
	nodeIndex, ok := m.selectedNodeIndex()
	if !ok {
		return reviewTarget{}, false
	}
	nodeNumber := nodeIndex + 1
	node, err := m.session.NodeByNumber(nodeNumber)
	if err != nil || node.State != coop.NodeReview || node.CurrentAttempt() == nil {
		return reviewTarget{}, false
	}
	_, stepIndex, _, err := m.session.StepByNodeNumber(nodeNumber)
	if err != nil {
		return reviewTarget{}, false
	}
	return reviewTarget{title: node.Title, kind: "node", nodeNumbers: []int{nodeNumber}, stepIndex: stepIndex}, true
}

func (m Model) reviewIsActionable(nodeNumber int) bool {
	if m.session == nil {
		return false
	}
	node, err := m.session.NodeByNumber(nodeNumber)
	if err != nil || node.State != coop.NodeReview {
		return false
	}
	if node.Type == coop.NodeUIComponent {
		return true
	}
	_, stepIndex, _, err := m.session.StepByNodeNumber(nodeNumber)
	return err == nil && m.session.StepReadyForReview(stepIndex)
}

func (m Model) reviewTargetStillValid(target reviewTarget) bool {
	if m.session == nil || len(target.nodeNumbers) == 0 {
		return false
	}
	for _, nodeNumber := range target.nodeNumbers {
		node, err := m.session.NodeByNumber(nodeNumber)
		if err != nil || node.State != coop.NodeReview {
			return false
		}
	}
	if target.stepIndex < 0 || target.stepIndex >= len(m.session.Steps) {
		return false
	}
	return target.kind == "node" || m.session.StepReadyForReview(target.stepIndex)
}

func (m Model) selectedReviewCommand() string {
	target, ok := m.selectedConfirmationTarget()
	if !ok {
		return ""
	}
	return m.reviewCommandLabel(target.nodeNumbers)
}

func (m *Model) setStatus(message string, ttl time.Duration) {
	m.statusMessage = message
	if ttl <= 0 {
		m.statusExpiresAt = time.Time{}
		return
	}
	m.statusExpiresAt = time.Now().Add(ttl)
}

func (m *Model) clearExpiredStatus(now time.Time) {
	if !m.statusExpiresAt.IsZero() && now.After(m.statusExpiresAt) {
		m.statusMessage = ""
		m.statusExpiresAt = time.Time{}
	}
}

func (m *Model) updateAgentIdle(heartbeatAge time.Duration, heartbeatOK bool, now time.Time) {
	if m.session == nil || m.session.IsComplete() {
		m.agentIsIdle = false
		return
	}
	if m.agentProcessActive {
		m.agentIsIdle = false
		return
	}
	if m.agentPulseSeen {
		m.agentIsIdle = true
		return
	}
	if !heartbeatOK {
		m.agentIsIdle = false
		return
	}
	if heartbeatAge >= 0 && heartbeatAge < 5*time.Second {
		m.agentIsIdle = false
		return
	}
	if m.lastUpdateTime.IsZero() {
		m.agentIsIdle = false
		return
	}
	m.agentIsIdle = now.Sub(m.lastUpdateTime) > 2*time.Minute
}

func (m *Model) updateAgentProcessPulse(age time.Duration, ok bool) {
	if m.session == nil || m.session.IsComplete() {
		m.agentProcessActive = false
		return
	}
	if !ok {
		return
	}
	if age >= 0 {
		m.agentPulseSeen = true
	}
	m.agentProcessActive = age >= 0 && age < coop.AgentProcessPulseFreshFor
}
