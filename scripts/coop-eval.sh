#!/usr/bin/env bash
set -euo pipefail

repo_root="$(cd "$(dirname "$0")/.." && pwd)"
cd "$repo_root"

go run ./pkg/coop/evals/cmd/coop-eval "$@"
