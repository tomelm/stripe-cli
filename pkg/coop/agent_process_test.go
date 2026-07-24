package coop

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestAgentProcessLifecyclePersistsFastExit(t *testing.T) {
	dir := t.TempDir()
	store, err := NewStoreAt(dir)
	require.NoError(t, err)
	require.NoError(t, store.Write(&Session{ID: "fast", Status: SessionActive}))

	lifecycle, err := store.AgentProcessLifecycle("fast")
	require.NoError(t, err)
	assert.Nil(t, lifecycle)

	require.NoError(t, store.StartAgentProcess("fast", "launch-fast"))
	status := 1
	require.NoError(t, store.StopAgentProcess("fast", "launch-fast", &status))

	lifecycle, err = store.AgentProcessLifecycle("fast")
	require.NoError(t, err)
	require.NotNil(t, lifecycle)
	assert.Equal(t, AgentProcessStopped, lifecycle.Phase)
	require.NotNil(t, lifecycle.ExitStatus)
	assert.Equal(t, 1, *lifecycle.ExitStatus)
	assert.False(t, lifecycle.LaunchedAt.IsZero())
	assert.False(t, lifecycle.UpdatedAt.IsZero())

	info, err := os.Stat(filepath.Join(dir, "fast.json.agent-process"))
	require.NoError(t, err)
	assertPrivateAgentProcessFileMode(t, info)
}

func assertPrivateAgentProcessFileMode(t *testing.T, info os.FileInfo) {
	t.Helper()
	if runtime.GOOS == "windows" {
		return
	}
	assert.Equal(t, os.FileMode(0o600), info.Mode().Perm())
}

func TestAgentProcessLifecycleRunningRequiresCurrentLaunch(t *testing.T) {
	store, err := NewStoreAt(t.TempDir())
	require.NoError(t, err)
	require.NoError(t, store.Write(&Session{ID: "current", Status: SessionActive}))
	require.NoError(t, store.StartAgentProcess("current", "launch-one"))
	require.NoError(t, store.MarkAgentProcessRunning("current", "launch-one"))

	lifecycle, err := store.AgentProcessLifecycle("current")
	require.NoError(t, err)
	require.NotNil(t, lifecycle)
	assert.Equal(t, AgentProcessRunning, lifecycle.Phase)

	err = store.MarkAgentProcessRunning("current", "launch-old")
	require.ErrorIs(t, err, ErrAgentProcessSuperseded)
	status := 9
	err = store.StopAgentProcess("current", "launch-old", &status)
	require.ErrorIs(t, err, ErrAgentProcessSuperseded)

	lifecycle, err = store.AgentProcessLifecycle("current")
	require.NoError(t, err)
	assert.Equal(t, "launch-one", lifecycle.LaunchID)
	assert.Equal(t, AgentProcessRunning, lifecycle.Phase)
}

func TestAgentProcessLifecycleFirstStopWins(t *testing.T) {
	store, err := NewStoreAt(t.TempDir())
	require.NoError(t, err)
	require.NoError(t, store.Write(&Session{ID: "stopped", Status: SessionActive}))
	require.NoError(t, store.StartAgentProcess("stopped", "launch"))
	require.NoError(t, store.MarkAgentProcessRunning("stopped", "launch"))

	status := 7
	require.NoError(t, store.StopAgentProcess("stopped", "launch", &status))
	require.NoError(t, store.StopAgentProcess("stopped", "launch", nil))

	lifecycle, err := store.AgentProcessLifecycle("stopped")
	require.NoError(t, err)
	require.NotNil(t, lifecycle)
	require.NotNil(t, lifecycle.ExitStatus)
	assert.Equal(t, 7, *lifecycle.ExitStatus)
}

func TestAgentProcessLifecycleRejectsConcurrentLauncher(t *testing.T) {
	store, err := NewStoreAt(t.TempDir())
	require.NoError(t, err)
	require.NoError(t, store.Write(&Session{ID: "single", Status: SessionActive}))
	require.NoError(t, store.StartAgentProcess("single", "launch-one"))

	err = store.StartAgentProcess("single", "launch-two")
	require.ErrorIs(t, err, ErrAgentProcessAlreadyActive)
}

func TestAgentProcessLifecycleRejectsInvalidAndCorruptRecords(t *testing.T) {
	dir := t.TempDir()
	store, err := NewStoreAt(dir)
	require.NoError(t, err)
	require.NoError(t, store.Write(&Session{ID: "bad", Status: SessionActive}))

	require.Error(t, store.StartAgentProcess("bad", "../launch"))
	status := 256
	require.Error(t, store.StopAgentProcess("bad", "launch", &status))

	require.NoError(t, os.WriteFile(
		filepath.Join(dir, "bad.json.agent-process"),
		[]byte(`{"launch_id":"launch","phase":"running","unknown":true}`),
		0600,
	))
	_, err = store.AgentProcessLifecycle("bad")
	assert.ErrorContains(t, err, "unknown field")

	_, err = store.AgentProcessLifecycle("../bad")
	assert.True(t, errors.Is(err, ErrInvalidSessionID))
}
