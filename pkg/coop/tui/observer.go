package tui

import "sync"

// ObserverFactory starts passive observation for one session and returns a
// stop function that blocks until observation has fully shut down. Factories
// must not block; they spawn their own goroutine.
type ObserverFactory func(sessionID string) (stop func())

// WithObserver injects the passive-observation factory owned by the TUI
// process. A nil factory disables observation.
func WithObserver(factory ObserverFactory) Option {
	return func(m *Model) {
		if factory != nil {
			m.observer = &observerController{factory: factory}
		}
	}
}

// observerController owns at most one running session observer. It is held by
// pointer on the Model so bubbletea value copies share it.
type observerController struct {
	factory ObserverFactory

	mu        sync.Mutex
	sessionID string
	stop      func()
}

// Watch starts observation for sessionID, first stopping any observer that
// belongs to a different session. Watching the same session twice is a no-op.
func (controller *observerController) Watch(sessionID string) {
	if controller == nil || sessionID == "" {
		return
	}
	controller.mu.Lock()
	defer controller.mu.Unlock()
	if controller.sessionID == sessionID {
		return
	}
	if controller.stop != nil {
		controller.stop()
	}
	controller.sessionID = sessionID
	controller.stop = controller.factory(sessionID)
}

// Stop halts the current observer, blocking until shutdown completes.
func (controller *observerController) Stop() {
	if controller == nil {
		return
	}
	controller.mu.Lock()
	defer controller.mu.Unlock()
	if controller.stop != nil {
		controller.stop()
		controller.stop = nil
		controller.sessionID = ""
	}
}
