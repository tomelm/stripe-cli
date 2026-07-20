package tui

import (
	"sync"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/stripe/stripe-cli/pkg/coop"
)

// observerRecorder is a fake ObserverFactory that records start/stop events.
type observerRecorder struct {
	mu     sync.Mutex
	events []string
}

func (r *observerRecorder) record(event string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, event)
}

func (r *observerRecorder) snapshot() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.events...)
}

func (r *observerRecorder) factory(sessionID string) func() {
	r.record("start " + sessionID)
	return func() { r.record("stop " + sessionID) }
}

// stubProgram satisfies programRunner without opening a TTY.
type stubProgram struct {
	runFunc func() (tea.Model, error)
}

func (p stubProgram) Run() (tea.Model, error) { return p.runFunc() }

func swapNewProgram(t *testing.T, factory func(model tea.Model) programRunner) {
	t.Helper()
	orig := newProgram
	newProgram = factory
	t.Cleanup(func() { newProgram = orig })
}

func newTestStore(t *testing.T) *coop.Store {
	t.Helper()
	store, err := coop.NewStoreAt(t.TempDir())
	require.NoError(t, err)
	return store
}

func TestWithObserverNilFactoryIsNoop(t *testing.T) {
	store := newTestStore(t)

	m := NewModel(store, "coop_x", WithObserver(nil))
	assert.Nil(t, m.observer)

	// Nil controllers are safe to call.
	var controller *observerController
	controller.Watch("coop_x")
	controller.Stop()

	// The Run path works without an observer via the stubbed program.
	ran := false
	swapNewProgram(t, func(model tea.Model) programRunner {
		return stubProgram{runFunc: func() (tea.Model, error) {
			ran = true
			return model, nil
		}}
	})
	require.NoError(t, Run(store, "coop_x", WithObserver(nil)))
	assert.True(t, ran)
}

func TestObserverControllerWatchStartsOncePerSession(t *testing.T) {
	rec := &observerRecorder{}
	controller := &observerController{factory: rec.factory}

	controller.Watch("a")
	controller.Watch("a")

	assert.Equal(t, []string{"start a"}, rec.snapshot())
}

func TestObserverControllerWatchSwitchingSessionsStopsPrevious(t *testing.T) {
	rec := &observerRecorder{}
	controller := &observerController{factory: rec.factory}

	controller.Watch("a")
	controller.Watch("b")

	assert.Equal(t, []string{"start a", "stop a", "start b"}, rec.snapshot())
}

func TestObserverControllerStopIsIdempotent(t *testing.T) {
	rec := &observerRecorder{}
	controller := &observerController{factory: rec.factory}

	controller.Watch("a")
	controller.Stop()
	controller.Stop()

	assert.Equal(t, []string{"start a", "stop a"}, rec.snapshot())

	// Watch after Stop restarts observation.
	controller.Watch("a")
	assert.Equal(t, []string{"start a", "stop a", "start a"}, rec.snapshot())
}

func TestRunStartsObserverBeforeProgramAndStopsOnExit(t *testing.T) {
	rec := &observerRecorder{}
	swapNewProgram(t, func(model tea.Model) programRunner {
		return stubProgram{runFunc: func() (tea.Model, error) {
			rec.record("program-run")
			return model, nil
		}}
	})

	store := newTestStore(t)
	require.NoError(t, Run(store, "coop_x", WithObserver(rec.factory)))

	assert.Equal(t, []string{"start coop_x", "program-run", "stop coop_x"}, rec.snapshot())
}

func TestRunWaitingStopsObserverStartedByDiscovery(t *testing.T) {
	rec := &observerRecorder{}
	swapNewProgram(t, func(model tea.Model) programRunner {
		return stubProgram{runFunc: func() (tea.Model, error) {
			updated, _ := model.Update(sessionDiscoveredMsg{sessionID: "coop_y"})
			rec.record("program-run")
			return updated, nil
		}}
	})

	store := newTestStore(t)
	require.NoError(t, RunWaiting(store, nil, WithObserver(rec.factory)))

	assert.Equal(t, []string{"start coop_y", "program-run", "stop coop_y"}, rec.snapshot())
}

func TestSessionDiscoveredMsgStartsObservation(t *testing.T) {
	rec := &observerRecorder{}
	store := newTestStore(t)
	m := NewWaitingModel(store, nil, WithObserver(rec.factory))

	result, _ := m.Update(sessionDiscoveredMsg{sessionID: "coop_a"})
	assert.Equal(t, []string{"start coop_a"}, rec.snapshot())

	updated, ok := result.(Model)
	require.True(t, ok)
	assert.Equal(t, "coop_a", updated.sessionID)

	// A second discovery (e.g. returning to a parent session) switches
	// observation to the new session.
	result, _ = updated.Update(sessionDiscoveredMsg{sessionID: "coop_b"})
	assert.Equal(t, []string{"start coop_a", "stop coop_a", "start coop_b"}, rec.snapshot())

	updated, ok = result.(Model)
	require.True(t, ok)
	assert.Equal(t, "coop_b", updated.sessionID)
}
