#!/usr/bin/env bash
set -euo pipefail

test -f docker-compose.yml
docker compose config >/dev/null

grep -R --exclude-dir=.git --exclude='*.md' "stripe" application >/dev/null
grep -R --exclude-dir=.git --exclude-dir=vendor "STRIPE_SECRET_KEY" . >/dev/null
grep -R --exclude-dir=.git --exclude-dir=vendor -E "checkout.session.completed|account.updated|payment_intent.succeeded" application >/dev/null

if [[ "${COOP_EVAL_RUN_DOCKER:-0}" == "1" ]]; then
  docker compose up --build -d nginx mysql php-fpm
  trap 'docker compose down --remove-orphans >/dev/null 2>&1 || true' EXIT
  for _ in $(seq 1 90); do
    if curl -fsS http://127.0.0.1 >/dev/null; then
      exit 0
    fi
    sleep 2
  done
  curl -fsS http://127.0.0.1 >/dev/null
fi
