package evals

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"time"

	"github.com/stripe/stripe-cli/pkg/coop"
)

func captureSessionHistory(ctx context.Context, store *coop.Store, sessionID, dir string, done chan<- struct{}) {
	defer close(done)
	_ = os.MkdirAll(dir, 0755)
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	lastVersion := -1
	seq := 0
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		session, err := store.Read(sessionID)
		if err != nil || session.Version == lastVersion {
			continue
		}
		lastVersion = session.Version
		seq++
		_ = writeJSON(filepath.Join(dir, fmt.Sprintf("%03d-v%d.json", seq, session.Version)), session)
	}
}

func writeStripeShim(dir, realStripeBin, logPath string) error {
	if err := os.MkdirAll(dir, 0755); err != nil {
		return err
	}
	script := fmt.Sprintf(`#!/usr/bin/env bash
set +e
{
  printf 'time=%%s cwd=%%q args=' "$(date -u +%%Y-%%m-%%dT%%H:%%M:%%SZ)" "$PWD"
  printf '%%q ' "$@"
  printf '\n'
} >> %q
for arg in "$@"; do
  case "$arg" in
    *'card[number]'*|*'card.number'*|*4242424242424242*|*4000000000000002*|*4000000000009995*|*5555555555554444*|*378282246310005*)
      printf 'Full card numbers are disabled inside co-op evals. Use hosted/client-side Stripe collection or a test PaymentMethod ID such as pm_card_visa.\n' >&2
      status=126
      printf 'time=%%s exit=%%s blocked=raw_card_number\n' "$(date -u +%%Y-%%m-%%dT%%H:%%M:%%SZ)" "$status" >> %q
      exit "$status"
      ;;
  esac
done
if [[ "${1:-}" == "login" ]]; then
  printf 'stripe login is disabled inside co-op evals; use local checks or eval-scoped config instead.\n' >&2
  status=126
  printf 'time=%%s exit=%%s blocked=login\n' "$(date -u +%%Y-%%m-%%dT%%H:%%M:%%SZ)" "$status" >> %q
  exit "$status"
fi
if [[ "${1:-}" == "sandbox" && "${2:-}" == "create" && -n "${STRIPE_SECRET_KEY:-}${STRIPE_API_KEY:-}" ]]; then
  printf 'stripe sandbox create is disabled inside co-op evals when STRIPE_SECRET_KEY or STRIPE_API_KEY is already provided; use the eval-provided key.\n' >&2
  status=126
  printf 'time=%%s exit=%%s blocked=sandbox_create_with_provided_key\n' "$(date -u +%%Y-%%m-%%dT%%H:%%M:%%SZ)" "$status" >> %q
  exit "$status"
fi
%q "$@"
status=$?
printf 'time=%%s exit=%%s\n' "$(date -u +%%Y-%%m-%%dT%%H:%%M:%%SZ)" "$status" >> %q
exit "$status"
`, logPath, logPath, logPath, logPath, realStripeBin, logPath)
	if err := os.WriteFile(filepath.Join(dir, "stripe"), []byte(script), 0755); err != nil {
		return err
	}
	return writeBrowserBlockers(dir, logPath)
}

func writeBrowserBlockers(dir, logPath string) error {
	script := fmt.Sprintf(`#!/usr/bin/env bash
name="$(basename "$0")"
{
  printf 'time=%%s browser-blocked=%%s args=' "$(date -u +%%Y-%%m-%%dT%%H:%%M:%%SZ)" "$name"
  printf '%%q ' "$@"
  printf '\n'
} >> %q
printf 'host browser automation is disabled inside co-op evals: %%s\n' "$name" >&2
exit 1
`, logPath)
	for _, name := range []string{
		"open",
		"xdg-open",
		"coop-eval-browser-disabled",
		"google-chrome",
		"google-chrome-stable",
		"google-chrome-beta",
		"google-chrome-canary",
		"Google Chrome",
		"chrome",
		"chromium",
		"chromium-browser",
		"firefox",
		"safari",
		"playwright",
		"puppeteer",
		"selenium",
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(script), 0755); err != nil {
			return err
		}
	}
	return nil
}

func writeCommandLog(resultDir string, records []commandRecord) {
	_ = writeJSON(filepath.Join(resultDir, "command-log.json"), records)
}

func redactSensitiveArtifacts(paths ...string) {
	patterns := []struct {
		pattern     *regexp.Regexp
		replacement []byte
	}{
		{regexp.MustCompile(`(?:rkcs|rk|sk|pk)_(?:test|live)_[A-Za-z0-9_]+`), []byte("[redacted]")},
		{regexp.MustCompile(`whsec_[A-Za-z0-9_]+`), []byte("[redacted]")},
		{regexp.MustCompile(`card\\?\[number\\?\]=[^\s'"]+`), []byte("card[number]=[redacted-card-number]")},
		{regexp.MustCompile(`\b(?:4242424242424242|4000000000000002|4000000000009995|5555555555554444|378282246310005)\b`), []byte("[redacted-card-number]")},
		{regexp.MustCompile(`(/stripecli/auth/)cliauth_[A-Za-z0-9_%-]+`), []byte("$1[redacted]")},
		{regexp.MustCompile(`(confirm_auth(?:\\)?\?t=)[A-Za-z0-9_%-]+`), []byte("$1[redacted]")},
		{regexp.MustCompile(`(secret=)[A-Za-z0-9_%-]+`), []byte("$1[redacted]")},
	}
	for _, path := range paths {
		if path == "" {
			continue
		}
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		redacted := data
		for _, pattern := range patterns {
			redacted = pattern.pattern.ReplaceAll(redacted, pattern.replacement)
		}
		if !bytes.Equal(data, redacted) {
			_ = os.WriteFile(path, redacted, 0600)
		}
	}
}

func redactSensitiveArtifactsInDir(dir string) {
	redactSensitiveArtifactsInDirSkipping(dir, nil)
}

func redactResultArtifacts(resultDir string) {
	redactSensitiveArtifactsInDir(resultDir)
}

func redactSensitiveArtifactsInDirSkipping(dir string, skipDirs map[string]bool) {
	_ = filepath.WalkDir(dir, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if entry.IsDir() && skipDirs[entry.Name()] {
			return filepath.SkipDir
		}
		if entry.IsDir() {
			return nil
		}
		redactSensitiveArtifacts(path)
		return nil
	})
}

func captureWorkspaceDiff(ctx context.Context, workspace, path string) string {
	data, _ := exec.CommandContext(ctx, "git", "-C", workspace, "diff", "--no-ext-diff", "HEAD").CombinedOutput()
	_ = os.WriteFile(path, data, 0644)
	return path
}

func captureWorkspaceStatus(ctx context.Context, workspace, path string) string {
	data, _ := exec.CommandContext(ctx, "git", "-C", workspace, "status", "--short").CombinedOutput()
	_ = os.WriteFile(path, data, 0644)
	return path
}
