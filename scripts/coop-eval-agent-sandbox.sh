#!/usr/bin/env bash
set -euo pipefail

if [[ $# -eq 0 ]]; then
  printf 'usage: %s <agent command> [args...]\n' "$(basename "$0")" >&2
  exit 64
fi

if [[ "$(uname -s)" != "Darwin" ]] || ! command -v sandbox-exec >/dev/null 2>&1; then
  exec "$@"
fi

profile="$(mktemp "${TMPDIR:-/tmp}/coop-eval-agent-sandbox.XXXXXX")"
trap 'rm -f "$profile"' EXIT

cat >"$profile" <<'PROFILE'
(version 1)
(allow default)

; Keep eval agents away from host browsers. Browser automation can touch the
; developer's desktop browser profile and trigger macOS Keychain prompts.
(deny process-exec
  (literal "/usr/bin/open")
  (literal "/usr/bin/osascript")
  (subpath "/Applications/Arc.app")
  (subpath "/Applications/Brave Browser.app")
  (subpath "/Applications/Chromium.app")
  (subpath "/Applications/Firefox.app")
  (subpath "/Applications/Google Chrome.app")
  (subpath "/Applications/Google Chrome Canary.app")
  (subpath "/Applications/Microsoft Edge.app")
  (subpath "/Applications/Safari.app")
  (subpath "/System/Applications/Safari.app"))
PROFILE

exec /usr/bin/sandbox-exec -f "$profile" "$@"
