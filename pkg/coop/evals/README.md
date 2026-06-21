# Co-op Evals

Co-op evals measure whether agents use the co-op workflow correctly and produce
useful integration outcomes. The runner creates an isolated fixture workspace,
starts a co-op session, launches an agent, drives the human review side, captures
artifacts, and scores protocol/evidence/correctness checks.

Run the default deterministic harness eval:

```sh
scripts/coop-eval.sh
```

The harness currently requires a POSIX shell. It is intended for macOS/Linux
developer machines and CI runners.

Run a specific case:

```sh
scripts/coop-eval.sh --case one-time-payment-debug
```

Run the complex real-agent suite:

```sh
scripts/coop-eval.sh --suite complex --agent command --agent-command 'codex exec --json --dangerously-bypass-approvals-and-sandbox --skip-git-repo-check --ephemeral "$(cat "$COOP_EVAL_PROMPT_FILE")"'
```

`--suite complex` selects cases tagged `complex-blueprint`, including cases that
are skipped by default. `--min-steps N` is available when you want to select
cases mechanically by blueprint size.

Run the external-app fixture suite:

```sh
scripts/coop-eval.sh \
  --suite tag:external-fixture \
  --timeout 60m \
  --agent command \
  --agent-command 'codex exec --json --dangerously-bypass-approvals-and-sandbox --skip-git-repo-check --ephemeral "$(cat "$COOP_EVAL_PROMPT_FILE")"'
```

External-app fixtures are pinned git sources plus small eval-owned overlays; the
upstream app source is cloned into each result directory and is not committed to
this repository. See [EXTERNAL_FIXTURES.md](EXTERNAL_FIXTURES.md) for the
fixture manifest format, current app targets, and the verification roadmap.

Quick rerun recipe for external-app evals with a judge:

```sh
export PATH="/opt/homebrew/Cellar/docker/29.5.3/bin:/opt/homebrew/bin:$PATH"
export STRIPE_SECRET_KEY="$(tr -d '\n' < /tmp/stripe-coop-eval-key)"

colima status || colima start
docker info >/dev/null
docker-compose version
```

Run one canary case first:

```sh
scripts/coop-eval.sh \
  --case hive-one-time-payment-python \
  --timeout 60m \
  --agent command \
  --agent-command 'codex exec --json --dangerously-bypass-approvals-and-sandbox --skip-git-repo-check --ephemeral "$(cat "$COOP_EVAL_PROMPT_FILE")"' \
  --judge command \
  --judge-command 'codex exec --dangerously-bypass-approvals-and-sandbox --skip-git-repo-check --ephemeral "$(cat "$COOP_EVAL_JUDGE_PROMPT_FILE")" > "$COOP_EVAL_JUDGE_OUTPUT_FILE"'
```

Run the full external-app suite:

```sh
scripts/coop-eval.sh \
  --suite tag:external-fixture \
  --timeout 60m \
  --agent command \
  --agent-command 'codex exec --json --dangerously-bypass-approvals-and-sandbox --skip-git-repo-check --ephemeral "$(cat "$COOP_EVAL_PROMPT_FILE")"' \
  --judge command \
  --judge-command 'codex exec --dangerously-bypass-approvals-and-sandbox --skip-git-repo-check --ephemeral "$(cat "$COOP_EVAL_JUDGE_PROMPT_FILE")" > "$COOP_EVAL_JUDGE_OUTPUT_FILE"'
```

Set `COOP_EVAL_RUN_DOCKER=1` when you want fixture smoke scripts to start
Docker services, not just validate Compose configuration and source-level
integration evidence. The runner passes this flag through to agents and
command checks.

Pass `--timeout` to override a case's `timeout_seconds` value for longer
real-agent trials.

Cases with `skip_default: true` are skipped by the default suite unless selected
with `--case` or a matching suite filter. Cases with `disabled: true` are
excluded from default, `all`, tag, and min-step selections; select them
explicitly with `--case` for one-off forensic reruns.

Run with an external agent command:

```sh
scripts/coop-eval.sh \
  --case one-time-payment-node \
  --timeout 30m \
  --agent command \
  --agent-command 'codex exec --json --dangerously-bypass-approvals-and-sandbox --skip-git-repo-check --ephemeral "$(cat "$COOP_EVAL_PROMPT_FILE")"'
```

The external command runs on the host with its current directory set to the
fixture workspace. The runner puts a `stripe` shim at the front of `PATH`; the
shim calls the candidate binary and records invocations in the eval artifacts.
For Codex implementation agents, include `--json` in `--agent-command` so the
runner can read token-count events from `agent.stdout.txt`. The recorded
`implementation_token_usage` includes the entire implementation agent run,
including project/context scanning and every co-op step. It intentionally
excludes the LLM judge, smoke-test commands, and deterministic harness work.
When a fixture has Docker or Compose config, the app runtime is expected to run
through that containerized tooling for dependency installs, package checks,
framework CLIs, migrations, tests, and server checks. The agent command itself
still runs on the host, but the shim directory blocks common host app-runtime
commands such as `composer`, `php`, `python`, `pip`, `node`, `npm`, `ruby`, and
`go` from Docker-backed fixture workspaces so agents use the fixture services
instead. The runner also sets isolated `HOME` and `XDG_CONFIG_HOME` directories
for the agent shell. Codex auth is preserved with `CODEX_HOME` when available,
but Stripe config and co-op state should stay inside the eval result directory.
Command agents are wrapped with `scripts/coop-eval-agent-sandbox.sh` by default
when that script is available. On macOS it prevents host browser executables
such as Google Chrome from launching during evals, avoiding desktop profile,
autofill, and Keychain prompts. Use `--disable-agent-sandbox` only when the
case deliberately tests browser-launch behavior.
Agent stdout/stderr and workspace diffs redact Stripe API-key-shaped values before
they are written into the final artifact set.
Result artifacts, including retained workspaces, are sanitized before the run
finishes so shared reports do not leak eval keys or auth URLs. The agent
process receives an explicit eval environment instead of inheriting the full
host shell environment.

Run with an optional LLM judge:

```sh
scripts/coop-eval.sh \
  --case hive-one-time-payment-python \
  --agent command \
  --agent-command 'codex exec --json --dangerously-bypass-approvals-and-sandbox --skip-git-repo-check --ephemeral "$(cat "$COOP_EVAL_PROMPT_FILE")"' \
  --judge command \
  --judge-command 'codex exec --dangerously-bypass-approvals-and-sandbox --skip-git-repo-check --ephemeral "$(cat "$COOP_EVAL_JUDGE_PROMPT_FILE")" > "$COOP_EVAL_JUDGE_OUTPUT_FILE"'
```

The judge is advisory by default. It writes a `judge` block and `llm_judge`
score, but deterministic checks still decide pass/fail. Use `--judge-required`
when you want the judge to act as a veto gate with `--judge-min-score`.
Judge commands must return strict JSON to stdout or
`$COOP_EVAL_JUDGE_OUTPUT_FILE`. They should exit nonzero only for adapter or
infrastructure failures, not for a negative verdict.

Each run writes:

```text
eval-results/<run-id>/
  summary.json
  summary.md
  summary.html
  <case-id>/
    case.json
    result.json
    command-log.json
    stripe-invocations.log
    coop-run.stdout.json
    agent.stdout.txt
    agent.stderr.txt
    final-session.json
    fixture.json
    judge-prompt.txt
    judge-output.json
    judge.stdout.txt
    judge.stderr.txt
    session-history/
    checks/
    workspace/
    workspace.diff
    workspace-status.txt
```

`summary.json`, each case's `result.json`, and `summary.md` include
`implementation_token_usage` when the implementation agent emitted
machine-readable token usage. If the field is unavailable, rerun with Codex
`--json` on the implementation `--agent-command`.

`summary.html` is a self-contained, minimally interactive report. It includes
run scorecards, failed checks, judge findings, recorded command outcomes, and the
agent's final co-op step evidence from `final-session.json`.

To regenerate a report for one run:

```sh
go run ./pkg/coop/evals/cmd/coop-eval-report \
  --results-dir eval-results/<run-id> \
  --output eval-results/<run-id>/summary.html \
  --portable
```

To compare multiple runs and annotate fixes between them:

```sh
go run ./pkg/coop/evals/cmd/coop-eval-report \
  --results-dir eval-results/<before-run> \
  --results-dir eval-results/<after-run> \
  --fixes coop-eval-fixes.json \
  --output coop-eval-report.html \
  --portable
```

`--portable` writes relative artifact links instead of absolute `file://` links.
The runner uses portable links for each run's `summary.html` automatically.

The optional fixes file can be either an array or an object with a `fixes`
array:

```json
{
  "fixes": [
    {
      "title": "Tighten app integration guidance",
      "before_run": "20260614-234701",
      "after_run": "20260615-022036",
      "cases": ["hive-one-time-payment-python"],
      "summary": "The rerun used the existing app checkout flow instead of a standalone Stripe demo.",
      "changes": ["Use app-owned order data", "Fulfill through signed webhook events"]
    }
  ]
}
```

Scoring currently covers:

- session completion
- terminal step states
- next-step suggestions
- review evidence
- review-await protocol
- request-changes recovery
- case-defined file and pattern checks
- eval hygiene, including raw card-number avoidance, host-browser avoidance,
  provided-key usage, eval-port forwarding, and async-event evidence
- external command checks for fixture-specific smoke, Docker, and functional
  verification

The deterministic `debug` agent is for harness validation and CI-safe protocol
coverage. Real-agent evals should use `--agent command` with an explicit Codex or
Claude command template, case-defined workspace checks, and repeated trials.
