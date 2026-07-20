package runtime

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/stripe/stripe-cli/pkg/coop"
	core "github.com/stripe/stripe-cli/pkg/coop/verification"
)

type providerFunc func(context.Context, Session, Emit) error

func (provider providerFunc) Run(ctx context.Context, session Session, emit Emit) error {
	return provider(ctx, session, emit)
}

func TestRunnerConcurrentStoreUpdates(t *testing.T) {
	t.Parallel()

	store, session := runtimeTestStore(t)
	const emitterCount = core.MaxResultsPerNode + 8
	provider := providerFunc(func(_ context.Context, opened Session, emit Emit) error {
		assert.Equal(t, session.ID, opened.ID)
		var group sync.WaitGroup
		group.Add(emitterCount)
		for index := 0; index < emitterCount; index++ {
			index := index
			go func() {
				defer group.Done()
				assert.NoError(t, emit(1, runtimePassedResult(index, "concurrent")))
			}()
		}
		group.Wait()
		return nil
	})
	runner := New(store, provider)

	require.NoError(t, runner.Run(context.Background(), session.ID))
	loaded, err := store.Read(session.ID)
	require.NoError(t, err)
	node, err := loaded.NodeByNumber(1)
	require.NoError(t, err)
	require.NotNil(t, node.VerificationResults)
	require.Len(t, node.VerificationResults.Results, core.MaxResultsPerNode)
	for index, result := range node.VerificationResults.Results {
		assert.Equal(t, core.ResultID(fmt.Sprintf("result-%02d", index)), result.ID)
	}
}

func TestRunnerStopsProvidersOnCancellation(t *testing.T) {
	t.Parallel()

	store, session := runtimeTestStore(t)
	started := make(chan struct{})
	stopped := make(chan struct{})
	provider := providerFunc(func(ctx context.Context, _ Session, _ Emit) error {
		close(started)
		<-ctx.Done()
		close(stopped)
		return ctx.Err()
	})
	runner := New(store, provider)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- runner.Run(ctx, session.ID) }()
	requireClosed(t, started)
	cancel()
	require.NoError(t, receiveError(t, done))
	requireClosed(t, stopped)
}

func TestRunnerReturnsWhenProviderSeesSessionComplete(t *testing.T) {
	t.Parallel()

	// Terminal detection belongs to the provider: it polls the session as
	// part of its work and returns once the session leaves the active state.
	// The runner just waits for it.
	store, session := runtimeTestStore(t)
	started := make(chan struct{})
	provider := providerFunc(func(ctx context.Context, provided Session, _ Emit) error {
		close(started)
		for {
			current, err := store.Read(provided.ID)
			if err == nil && (current.Status != coop.SessionActive || current.IsComplete()) {
				return nil
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(5 * time.Millisecond):
			}
		}
	})
	runner := New(store, provider)

	done := make(chan error, 1)
	go func() { done <- runner.Run(context.Background(), session.ID) }()
	requireClosed(t, started)
	_, err := store.Update(session.ID, func(current *coop.Session) error {
		current.Status = coop.SessionCompleted
		return nil
	})
	require.NoError(t, err)
	require.NoError(t, receiveError(t, done))
}

func TestRunnerNeverSerializesRawCredentials(t *testing.T) {
	t.Parallel()

	const credential = "credential-owned-by-this-process"
	directory := t.TempDir()
	store, err := coop.NewStoreAt(directory)
	require.NoError(t, err)
	session := testSession("credential_session")
	require.NoError(t, store.Write(session))

	provider := providerFunc(func(_ context.Context, _ Session, emit Emit) error {
		result := runtimePassedResult(1, "used "+credential)
		result.Evidence = []core.Evidence{
			{Key: "safe_value", Class: core.EvidenceSafe, Value: credential},
			{Key: "secret_value", Class: core.EvidenceSensitive, Value: credential},
		}
		return emit(1, result)
	})
	runner := New(store, provider, WithCredentials(credential))
	require.NoError(t, runner.Run(context.Background(), session.ID))

	data, err := os.ReadFile(filepath.Join(directory, session.ID+".json"))
	require.NoError(t, err)
	assert.NotContains(t, string(data), credential)
	assert.Contains(t, string(data), "[redacted]")
}

func TestRunnerNilProviderIsNoop(t *testing.T) {
	t.Parallel()

	store, session := runtimeTestStore(t)
	counting := &countingStore{Store: store}
	runner := New(counting, nil)

	require.NoError(t, runner.Run(context.Background(), session.ID))
	assert.Equal(t, 1, counting.reads)
	assert.Equal(t, 0, counting.updates)
}

type countingStore struct {
	Store
	reads   int
	updates int
}

func (s *countingStore) Read(id string) (*coop.Session, error) {
	s.reads++
	return s.Store.Read(id)
}

func (s *countingStore) Update(id string, fn func(*coop.Session) error) (*coop.Session, error) {
	s.updates++
	return s.Store.Update(id, fn)
}

func runtimeTestStore(t *testing.T) (*coop.Store, *coop.Session) {
	t.Helper()
	store, err := coop.NewStoreAt(t.TempDir())
	require.NoError(t, err)
	session := testSession("runtime_session")
	require.NoError(t, store.Write(session))
	return store, session
}

func testSession(id string) *coop.Session {
	return &coop.Session{
		SchemaVersion: coop.CurrentSessionSchemaVersion,
		ID:            id,
		Blueprint:     "test-blueprint",
		Status:        coop.SessionActive,
		Steps: []coop.SessionStep{{
			StepDefinition: coop.StepDefinition{Key: "step", Title: "Step"},
			Nodes: []coop.SessionNode{{
				NodeDefinition: coop.NodeDefinition{Key: "node", Title: "Node"},
				State:          coop.NodeActive,
			}},
		}},
	}
}

func runtimePassedResult(index int, detail string) core.Result {
	id := core.ResultID(fmt.Sprintf("result-%02d", index))
	return core.Result{
		ID:      id,
		CheckID: core.CheckID("check-" + string(id)),
		Source:  core.SourceCLI,
		Status:  core.StatusPassed,
		Detail:  detail,
	}
}

func requireClosed(t *testing.T, channel <-chan struct{}) {
	t.Helper()
	select {
	case <-channel:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for channel close")
	}
}

func receiveError(t *testing.T, channel <-chan error) error {
	t.Helper()
	select {
	case err := <-channel:
		return err
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for runner")
		return nil
	}
}

// TestRunnerProviderSessionCopiesAreIndependent guards cloneSession's deep-copy
// guarantee: independent copies derived from the one Session a provider
// receives must not share backing arrays, even when mutated concurrently.
func TestRunnerProviderSessionCopiesAreIndependent(t *testing.T) {
	t.Parallel()

	store, err := coop.NewStoreAt(t.TempDir())
	require.NoError(t, err)
	session := testSession("clone_session")
	session.Steps[0].Nodes[0].Events = []string{"seed"}
	require.NoError(t, store.Write(session))

	var barrier sync.WaitGroup
	barrier.Add(2)
	clones := make([]Session, 2)
	provider := providerFunc(func(_ context.Context, opened Session, _ Emit) error {
		assert.Equal(t, []string{"seed"}, opened.Nodes[0].Events)
		for index := 0; index < 2; index++ {
			index := index
			go func() {
				defer barrier.Done()
				clone := cloneSession(opened)
				clone.Nodes[0].Events = append(clone.Nodes[0].Events, fmt.Sprintf("changed-%d", index))
				clones[index] = clone
			}()
		}
		barrier.Wait()
		return nil
	})
	runner := New(store, provider)

	require.NoError(t, runner.Run(context.Background(), session.ID))
	assert.Equal(t, []string{"seed", "changed-0"}, clones[0].Nodes[0].Events)
	assert.Equal(t, []string{"seed", "changed-1"}, clones[1].Nodes[0].Events)
	assert.Equal(t, []string{"seed"}, session.Steps[0].Nodes[0].Events)
}
