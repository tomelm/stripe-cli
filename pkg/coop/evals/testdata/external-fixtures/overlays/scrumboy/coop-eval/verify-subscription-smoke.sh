#!/usr/bin/env bash
set -euo pipefail

test -f docker-compose.yml
docker compose config >/dev/null

grep -R --exclude-dir=.git --exclude-dir=data --exclude='*.md' "stripe" . >/dev/null
grep -R --exclude-dir=.git --exclude-dir=data "STRIPE_SECRET_KEY" . >/dev/null
grep -R --exclude-dir=.git --exclude-dir=data "customer.subscription" . >/dev/null

if [[ "${COOP_EVAL_RUN_DOCKER:-0}" == "1" ]]; then
  docker compose up --build -d
  trap 'docker compose down --remove-orphans >/dev/null 2>&1 || true' EXIT
  for _ in $(seq 1 60); do
    if curl -fsS http://127.0.0.1:8080 >/dev/null; then
      exit 0
    fi
    sleep 2
  done
  curl -fsS http://127.0.0.1:8080 >/dev/null
fi
