#!/usr/bin/env bash
set -euo pipefail

test -f docker-compose.yml
docker compose config >/dev/null

test -f backend/.env
test -f frontend/.env.local

grep -R "stripe" backend frontend >/dev/null
grep -R "checkout.session.completed" backend frontend >/dev/null
grep -R "STRIPE_SECRET_KEY" backend frontend >/dev/null
grep -R "STRIPE_WEBHOOK_SECRET" backend frontend >/dev/null

if [[ "${COOP_EVAL_RUN_DOCKER:-0}" == "1" ]]; then
  docker compose up --build -d backend frontend
  trap 'docker compose down --remove-orphans >/dev/null 2>&1 || true' EXIT
  for _ in $(seq 1 60); do
    if curl -fsS http://127.0.0.1:8000/docs >/dev/null; then
      exit 0
    fi
    sleep 2
  done
  curl -fsS http://127.0.0.1:8000/docs >/dev/null
fi
