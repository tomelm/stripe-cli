package evals

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestWorkspacePathContainsPatternMatchesRootAndNestedGlobs(t *testing.T) {
	workspace := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(workspace, "server.js"), []byte("stripe.checkout.sessions.create({ mode: 'payment' })"), 0644))
	require.NoError(t, os.MkdirAll(filepath.Join(workspace, "lib"), 0755))
	require.NoError(t, os.WriteFile(filepath.Join(workspace, "lib", "webhooks.js"), []byte("stripe.webhooks.constructEvent(body, sig, secret)"), 0644))

	require.True(t, workspacePathContainsPattern(workspace, "**/*.js", `checkout\.sessions\.create`))
	require.True(t, workspacePathContainsPattern(workspace, "**/*.js", `webhooks\.constructEvent`))
}

func TestWorkspacePathContainsPatternSkipsGeneratedDependencyTrees(t *testing.T) {
	workspace := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(workspace, "node_modules", "stripe"), 0755))
	require.NoError(t, os.WriteFile(filepath.Join(workspace, "node_modules", "stripe", "index.js"), []byte("stripe.checkout.sessions.create({})"), 0644))

	require.False(t, workspacePathContainsPattern(workspace, "**/*.js", `checkout\.sessions\.create`))
}
