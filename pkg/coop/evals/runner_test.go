package evals

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
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

func TestEvalEnvDisablesBrowserAuth(t *testing.T) {
	env := evalEnv("/tmp/xdg", "/tmp/home", "/tmp/shim", "/tmp/stripe", "/tmp/stripe.log", 4242)

	require.Contains(t, env, "SSH_TTY=coop-eval")
	require.Contains(t, env, "SSH_CONNECTION=coop-eval")
	require.Contains(t, env, "SSH_CLIENT=coop-eval")
	require.Contains(t, env, "BROWSER=coop-eval-browser-disabled")
}

func TestStripeShimBlocksLoginAndBrowserOpen(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "stripe.log")
	realCalledPath := filepath.Join(dir, "real-called")
	realStripePath := filepath.Join(dir, "real-stripe")
	realStripeScript := "#!/usr/bin/env bash\nprintf '%s\\n' \"$@\" >> " + strconv.Quote(realCalledPath) + "\n"
	require.NoError(t, os.WriteFile(realStripePath, []byte(realStripeScript), 0755))
	require.NoError(t, writeStripeShim(dir, realStripePath, logPath))

	stripePath := filepath.Join(dir, "stripe")
	loginCmd := exec.Command(stripePath, "login", "--non-interactive")
	loginOutput, err := loginCmd.CombinedOutput()
	require.Error(t, err)
	require.Contains(t, string(loginOutput), "stripe login is disabled inside co-op evals")
	require.NoFileExists(t, realCalledPath)

	whoamiCmd := exec.Command(stripePath, "whoami")
	require.NoError(t, whoamiCmd.Run())
	realCalled, err := os.ReadFile(realCalledPath)
	require.NoError(t, err)
	require.Equal(t, "whoami\n", string(realCalled))

	openCmd := exec.Command(filepath.Join(dir, "open"), "https://dashboard.stripe.com")
	openOutput, err := openCmd.CombinedOutput()
	require.Error(t, err)
	require.Contains(t, string(openOutput), "browser opening is disabled inside co-op evals")

	logData, err := os.ReadFile(logPath)
	require.NoError(t, err)
	require.Contains(t, string(logData), "blocked=login")
	require.Contains(t, string(logData), "browser-blocked=open")
}

func TestRedactSensitiveArtifactsRedactsAuthURLs(t *testing.T) {
	path := filepath.Join(t.TempDir(), "artifact.txt")
	artifact := `key=sk_test_123
browser_url=https://dashboard.stripe.com/stripecli/confirm_auth?t=confirmSecret123
escaped_url=https://dashboard.stripe.com/stripecli/confirm_auth\?t=escapedSecret123
next_step=stripe login --complete 'https://dashboard.stripe.com/stripecli/auth/cliauth_abc123?secret=pollSecret123'
`
	require.NoError(t, os.WriteFile(path, []byte(artifact), 0644))

	redactSensitiveArtifacts(path)

	data, err := os.ReadFile(path)
	require.NoError(t, err)
	redacted := string(data)
	require.NotContains(t, redacted, "sk_test_123")
	require.NotContains(t, redacted, "confirmSecret123")
	require.NotContains(t, redacted, "escapedSecret123")
	require.NotContains(t, redacted, "cliauth_abc123")
	require.NotContains(t, redacted, "pollSecret123")
	require.Contains(t, redacted, "confirm_auth?t=[redacted]")
	require.Contains(t, redacted, `confirm_auth\?t=[redacted]`)
	require.Contains(t, redacted, "/stripecli/auth/[redacted]?secret=[redacted]")
}

func TestRedactResultArtifactsSkipsWorkspace(t *testing.T) {
	resultDir := t.TempDir()
	artifactPath := filepath.Join(resultDir, "agent.stderr.txt")
	workspacePath := filepath.Join(resultDir, "workspace", "server.js")
	require.NoError(t, os.WriteFile(artifactPath, []byte("https://dashboard.stripe.com/stripecli/confirm_auth?t=confirmSecret123\n"), 0644))
	require.NoError(t, os.MkdirAll(filepath.Dir(workspacePath), 0755))
	require.NoError(t, os.WriteFile(workspacePath, []byte("const key = 'sk_test_123'\n"), 0644))

	redactResultArtifacts(resultDir)

	artifactData, err := os.ReadFile(artifactPath)
	require.NoError(t, err)
	require.NotContains(t, string(artifactData), "confirmSecret123")
	require.Contains(t, string(artifactData), "confirm_auth?t=[redacted]")

	workspaceData, err := os.ReadFile(workspacePath)
	require.NoError(t, err)
	require.Contains(t, string(workspaceData), "sk_test_123")
}
