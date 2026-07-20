// Package tui implements the bubbletea-based terminal UI for co-op mode.
package tui

import (
	tea "charm.land/bubbletea/v2"

	"github.com/stripe/stripe-cli/pkg/coop"
)

type Option func(*Model)

func WithSandboxClaimURL(claimURL string) Option {
	return func(m *Model) {
		m.sandboxClaimURL = claimURL
	}
}

// programRunner abstracts tea.Program for tests that cannot open a TTY.
type programRunner interface {
	Run() (tea.Model, error)
}

var newProgram = func(model tea.Model) programRunner {
	return tea.NewProgram(model)
}

// Run launches the fullscreen co-op TUI for a known session. The TUI process
// owns passive observation for the session: it starts before the program and
// stops when the program exits.
func Run(store *coop.Store, sessionID string, opts ...Option) error {
	model := NewModel(store, sessionID, opts...)
	if model.observer != nil {
		model.observer.Watch(sessionID)
		defer model.observer.Stop()
	}
	_, err := newProgram(model).Run()
	return err
}

// RunWaiting launches the TUI in "waiting" mode — it polls for a new session
// to appear (ignoring the provided existing session IDs) and transitions once
// found. Passive observation starts when the session is discovered and stops
// when the program exits.
func RunWaiting(store *coop.Store, existingSessionIDs map[string]bool, opts ...Option) error {
	model := NewWaitingModel(store, existingSessionIDs, opts...)
	if model.observer != nil {
		defer model.observer.Stop()
	}
	_, err := newProgram(model).Run()
	return err
}
