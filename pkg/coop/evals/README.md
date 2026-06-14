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

Pass `--timeout` to override a case's `timeout_seconds` value for longer
real-agent trials.

Run with an external agent command:

```sh
scripts/coop-eval.sh \
  --agent command \
  --agent-command 'codex exec --dangerously-bypass-approvals-and-sandbox "$(cat "$COOP_EVAL_PROMPT_FILE")"'
```

The external command runs inside the fixture workspace. The runner puts a
`stripe` shim at the front of `PATH`; the shim calls the candidate binary and
records invocations in the eval artifacts. The runner also sets isolated
`HOME` and `XDG_CONFIG_HOME` directories for the agent shell. Codex auth is
preserved with `CODEX_HOME` when available, but Stripe config and co-op state
should stay inside the eval result directory.
Agent stdout/stderr and workspace diffs redact Stripe API-key-shaped values before
they are written into the final artifact set.

Each run writes:

```text
eval-results/<run-id>/
  summary.json
  summary.md
  <case-id>/
    case.json
    result.json
    command-log.json
    stripe-invocations.log
    coop-run.stdout.json
    agent.stdout.txt
    agent.stderr.txt
    final-session.json
    session-history/
    workspace/
    workspace.diff
    workspace-status.txt
```

Scoring currently covers:

- session completion
- terminal step states
- next-step suggestions
- review evidence
- review-await protocol
- request-changes recovery
- case-defined file and pattern checks

The deterministic `debug` agent is for harness validation and CI-safe protocol
coverage. Real-agent evals should use `--agent command` with an explicit Codex or
Claude command template, case-defined workspace checks, and repeated trials.
