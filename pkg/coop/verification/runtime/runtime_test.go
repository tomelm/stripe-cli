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
	runner := New(store, WithPollInterval(5*time.Millisecond))
	for index := 0; index < core.MaxResultsPerNode+8; index++ {
		index := index
		require.NoError(t, runner.Register(providerFunc(func(_ context.Context, opened Session, emit Emit) error {
			assert.Equal(t, session.ID, opened.ID)
			return emit(1, runtimePassedResult(index, "concurrent"))
		})))
	}

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
	runner := New(store, WithPollInterval(5*time.Millisecond))
	require.NoError(t, runner.Register(providerFunc(func(ctx context.Context, _ Session, _ Emit) error {
		close(started)
		<-ctx.Done()
		close(stopped)
		return ctx.Err()
	})))

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- runner.Run(ctx, session.ID) }()
	requireClosed(t, started)
	cancel()
	require.NoError(t, receiveError(t, done))
	requireClosed(t, stopped)
}

func TestRunnerStopsProvidersWhenSessionCompletes(t *testing.T) {
	t.Parallel()

	store, session := runtimeTestStore(t)
	started := make(chan struct{})
	stopped := make(chan struct{})
	runner := New(store, WithPollInterval(5*time.Millisecond))
	require.NoError(t, runner.Register(providerFunc(func(ctx context.Context, _ Session, _ Emit) error {
		close(started)
		<-ctx.Done()
		close(stopped)
		return ctx.Err()
	})))

	done := make(chan error, 1)
	go func() { done <- runner.Run(context.Background(), session.ID) }()
	requireClosed(t, started)
	_, err := store.Update(session.ID, func(current *coop.Session) error {
		current.Status = coop.SessionCompleted
		return nil
	})
	require.NoError(t, err)
	require.NoError(t, receiveError(t, done))
	requireClosed(t, stopped)
}

func TestRunnerNeverSerializesRawCredentials(t *testing.T) {
	t.Parallel()

	const credential = "credential-owned-by-this-process"
	directory := t.TempDir()
	store, err := coop.NewStoreAt(directory)
	require.NoError(t, err)
	session := testSession("credential_session")
	require.NoError(t, store.Write(session))

	runner := New(store, WithCredentials(credential))
	require.NoError(t, runner.Register(providerFunc(func(_ context.Context, _ Session, emit Emit) error {
		result := runtimePassedResult(1, "used "+credential)
		result.Evidence = []core.Evidence{
			{Key: "safe_value", Class: core.EvidenceSafe, Value: credential},
			{Key: "secret_value", Class: core.EvidenceSensitive, Value: credential},
		}
		return emit(1, result)
	})))
	require.NoError(t, runner.Run(context.Background(), session.ID))

	data, err := os.ReadFile(filepath.Join(directory, session.ID+".json"))
	require.NoError(t, err)
	assert.NotContains(t, string(data), credential)
	assert.Contains(t, string(data), "[redacted]")
}

func TestRunnerRejectsNilProvider(t *testing.T) {
	t.Parallel()

	store, _ := runtimeTestStore(t)
	assert.Error(t, New(store).Register(nil))
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

func TestRunnerProviderSessionCopiesAreIndependent(t *testing.T) {
	t.Parallel()

	store, session := runtimeTestStore(t)
	var barrier sync.WaitGroup
	barrier.Add(2)
	runner := New(store)
	for index := 0; index < 2; index++ {
		require.NoError(t, runner.Register(providerFunc(func(_ context.Context, opened Session, _ Emit) error {
			opened.Nodes[0].Events = append(opened.Nodes[0].Events, "changed")
			barrier.Done()
			return nil
		})))
	}
	require.NoError(t, runner.Run(context.Background(), session.ID))
	barrier.Wait()
}
