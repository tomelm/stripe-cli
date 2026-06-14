# External App Fixtures

External fixtures evaluate co-op against existing applications instead of tiny
from-scratch test projects. The intent is to measure whether an agent can
understand a real codebase, adapt the blueprint to that app's existing domain
model, and prove that the Stripe integration works end to end.

The upstream application source is not committed to this repository. Each eval
case clones a pinned git ref into its result directory, applies a small
eval-owned overlay, initializes a fresh local git baseline, runs the agent, and
then runs fixture-specific checks.

## Manifest Format

External fixtures live in `pkg/coop/evals/testdata/external-fixtures/<id>.json`.

```json
{
  "id": "hive-marketplace",
  "description": "FastAPI + Next.js marketplace",
  "source": {
    "type": "git",
    "url": "https://github.com/codebasics/hive-marketplace.git",
    "ref": "4a6c5e8d1cec41d5174b3bf570eaf4383c3de5ec"
  },
  "overlay": "overlays/hive-marketplace",
  "docker": {
    "compose_files": ["docker-compose.yml"],
    "services": ["backend", "frontend"],
    "default_url": "http://localhost:3000"
  }
}
```

The runner treats `case.fixture` as a local fixture directory first. If no local
directory exists, it loads `<external-fixtures-dir>/<fixture>.json` and clones the
pinned source. Overlays are copied after the upstream source and are part of the
fixture baseline, not agent work.

## Starter Targets

| Fixture | App Shape | Stripe Scenario |
| --- | --- | --- |
| `hive-marketplace` | FastAPI + Next.js buyer/seller marketplace with cart, products, orders, and mock payment code | One-time Checkout and Connect marketplace |
| `scrumboy` | Go project-management app with users, projects, roles, and Docker Compose | Recurring subscriptions and entitlements |
| `easyappointments` | PHP scheduling app with providers, customers, services, and appointments | Connect platform booking payments |

`aws-containers/retail-store-sample-app` is a good later target for a heavier
microservice-style one-time-payment eval, but it is intentionally not in the
starter suite.

## Verification Direction

The first implementation adds the runner support, pinned manifests, overlays,
static correctness checks, Docker Compose validation, and smoke scripts. The next
step is to harden each smoke script into a true live verification:

- Start the app through Docker with eval-owned project names and non-conflicting
  ports.
- Seed app domain data through HTTP or app-native scripts.
- Exercise the user flow over HTTP rather than launching a browser.
- Use the eval-provided Stripe key from `STRIPE_SECRET_KEY` or `STRIPE_API_KEY`.
- Verify Stripe objects with the Stripe API.
- Send signed webhook events to the local app and assert persisted app state.
- Fail if the agent implements a separate demo app instead of the existing app
  flow.

Hosted Checkout is the preferred card-collection path. Evals must not pass full
card numbers to Stripe APIs.

## Running

Run all external fixture cases:

```sh
scripts/coop-eval.sh \
  --suite tag:external-fixture \
  --timeout 60m \
  --agent command \
  --agent-command 'codex exec --dangerously-bypass-approvals-and-sandbox --skip-git-repo-check --ephemeral "$(cat "$COOP_EVAL_PROMPT_FILE")"'
```

Run one case:

```sh
scripts/coop-eval.sh --case hive-one-time-payment-python --timeout 60m ...
```

The smoke scripts default to static and Docker Compose config checks. Set
`COOP_EVAL_RUN_DOCKER=1` in the runner environment to let scripts start Docker
services where the fixture supports it.
