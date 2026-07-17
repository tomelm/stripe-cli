# Passive Observers Hypothesis

## Status and scope

This branch descends exactly from verification-core commit
`1f9347a43c64a8b24e4a52e53f695baf269c3ba2`. Its treatment is the inert,
transport-neutral package in `pkg/coop/observe`: explicit per-session inputs,
passive `stripe logs tail` and `stripe listen` connection contracts, bounded
startup and observation windows, collector health state, reconnect timing,
health epochs, minimal observations, TUI-facing summaries, and availability
results expressed with verification-core primitives.

The package does not spawn a CLI process, read ambient Stripe login or
configuration, contact Stripe, register a current Co-op runtime, or gate work.
Only an explicitly injected connector can open a stream. No model evaluation
or live qualification has run on this branch.

## Hypothesis

If passive request and event streams expose explicit readiness, structured
failure state, continuous health epochs, and separately accounted activity,
then a later consumer can distinguish healthy zero activity from broken
coverage without treating an environmental collector outage as an integration
failure.

The expected mechanism is deterministic state and coverage accounting around
an injected passive transport. This is a hypothesis about observer primitives,
not evidence that an application is correct or that a model will perform
better.

## Deterministic observable

At the pinned branch commit, model-free tests must demonstrate all of the
following:

1. A collector cannot be constructed without an explicit bounded session ID,
   stream, API key, device name, timeout policy, and jittered backoff policy.
   Config, connector-request, snapshot, summary, and result serialization omit
   the injected key.
2. The only public streams are `logs_tail` and `listen`; the only public
   transport states are `ready`, `retrying`, `unhealthy`, and `stopped`.
3. A connection becomes ready only after `WaitUntilReady` succeeds inside one
   bounded startup window. Cancellation and close cannot create a late ready
   epoch.
4. Transient connect, startup, and stream failures enter `retrying` with capped
   exponential backoff and injected jitter. A permanent classified failure
   enters `unhealthy`; an explicit, coalesced `RetryNow` starts a new attempt.
5. Each reconnect creates a new monotonically numbered health epoch and a gap.
   Snapshots expose only bounded request/event metadata and count malformed or
   source-incompatible observations as dropped.
6. An action-window absence is usable only when one ready epoch continuously
   spans the entire bounded window and that window contains zero observations.
   Zero activity remains visible when coverage is healthy, and never repairs a
   timeout, reconnect, stop, or clock regression.
7. TUI-facing summaries distinguish `healthy_no_activity` from retrying or
   unhealthy limited assurance without embedding terminal or workflow code.
8. Availability maps to verification core as `passed` while ready, `skipped`
   while intentionally stopped, and `unavailable` in the `collector` domain
   while coverage is broken. Only the exact transient environmental tuple
   satisfies `verification.Result.FailsOpen`.
9. Concurrent observation ingestion, snapshots, summaries, epoch reads, and
   availability-result creation pass the Go race detector.

The focused observables are:

```sh
go test ./pkg/coop/observe -count=1
go test -race ./pkg/coop/observe -count=1
go vet ./pkg/coop/observe
go build ./pkg/coop/observe
```

The dependency compatibility observable is:

```sh
go test ./pkg/coop/verification ./pkg/coop/observe -count=1
```

## Expected benefit

If later consumers adopt the contracts correctly, expected benefits are:

- explicit stream health instead of inferring readiness from process startup;
- bounded reconnect behavior whose timing can be reproduced with fake clocks;
- a durable distinction between no observed activity and no trustworthy
  observation coverage;
- minimal, source-specific retained facts that exclude raw payloads, headers,
  query strings, and signing secrets;
- a small presentation-neutral state projection for a later TUI; and
- consistent environmental-unavailability classification through the shared
  verification contract.

These benefits are unmeasured. The branch makes no integration-correctness,
model-quality, runtime-efficiency, or token-efficiency claim.

## Known blind spots

- The injected `Connector` and `Connection` interfaces do not implement a
  subprocess, socket, authentication handshake, or Stripe transport. Real
  adapters require separate qualification and must not fall back to ambient
  login.
- A ready transport does not prove that Stripe will emit every relevant fact,
  that filters are correct, or that an observation corresponds to a particular
  application action.
- `AbsenceUsable` establishes only uninterrupted passive coverage plus zero
  bounded observations. It is not a product assertion and cannot prove that a
  required request or event should have happened.
- Request observations retain normalized paths and event observations retain
  event types; downstream display and retention policy remain separate work.
- Bounded startup depends on connectors honoring context cancellation. A
  connector that violates the interface contract can retain its own blocked
  goroutine even though the supervisor stops waiting.
- Tests use scripted connectors and injected clocks. They do not exercise CLI
  output formats, live reconnect behavior, operating-system signals, proxies,
  account permissions, or network partitions.
- This branch does not correlate request and event streams, application state,
  blueprint obligations, resources, or durable application state.

## Expected cost

The existing Co-op runtime does not import this package, so current runtime and
model-token cost is zero. A future adopter would keep one supervisor goroutine
and a small fixed set of per-attempt goroutines per passive stream, plus bounded
snapshot state, minimal last-observation metadata, health-epoch history, and
caller-opened coverage tokens. Observation ingestion and snapshots are
constant-time apart from lock contention; retained epoch storage grows linearly
with session reconnects, while unfinished coverage-window storage is capped at
64 tokens per supervisor.

Reconnect delay doubles from the configured initial delay, applies symmetric
injected jitter, and never exceeds the configured maximum. Startup and action
observation windows are each capped at 24 hours by validation. No quantitative
latency or memory improvement is claimed.

## Fail-open and fail-closed behavior

Configuration validation fails closed before I/O for missing explicit
credentials, unknown streams, invalid filters, malformed bounded text,
unbounded durations, or invalid backoff/jitter parameters. Malformed connector
failures become non-transient `connector_invalid`; malformed observations are
dropped and counted rather than retained.

Environmental connection unavailability, startup timeout, or stream closure
produces a CLI-owned, transient, `unavailable` verification result in the
`collector` failure domain. That exact tuple is recognized by the inherited
`verification.Result.FailsOpen` primitive. Authentication rejection and
connector-contract errors are non-transient and do not match it. An intentional
stop is `skipped`, and a ready collector is `passed`.

No result advances, blocks, retries, reopens, completes, or suppresses a
workflow. Retry timing is collector transport recovery only. The inherited
fail-open predicate remains a factual classification and never converts
unavailability into a pass.

## Model-free qualification corpus

The known-good corpus includes:

- explicit listen configuration with bounded event filters and exact
  credential injection into a fake connector;
- immediately ready logs-tail and listen connections;
- healthy zero-activity and healthy observed-activity summaries;
- valid minimal request and event observations;
- a complete zero-activity action window inside one health epoch;
- a transient stream disconnect followed by deterministic backoff and a new
  ready epoch;
- a permanent authentication failure recovered only by manual retry; and
- concurrent snapshots, summaries, epoch reads, result creation, and
  observation ingestion under the race detector.

The targeted failure corpus includes:

- missing explicit credentials, wrong-stream filters, overlong timeouts, and
  invalid jitter samples;
- query-bearing request paths, wrong-stream facts, and empty observations;
- startup timeout and a canceled readiness wait followed by a late signal;
- a reconnect inside an action window, an expired observation window, a clock
  regression, and reuse of a consumed window token;
- permanent collector failure that must not satisfy fail-open; and
- environmental retrying state that must satisfy only verification core's
  exact fail-open tuple.

Qualification stops on any known-good failure, accepted targeted mutant,
credential leak, late readiness epoch, usable absence across a health gap, or
race-detector finding.

## Criteria for a future matched evaluation

No model evaluation is authorized during this cleanup cycle. A future matched
evaluation is justified only after this branch passes model-free qualification
and separate approval creates `tomer/coop-evals-passive-observers` from
`tomer/coop-vanilla-baseline-evals`. That evaluator branch must invoke
authenticated control and treatment binaries externally, compare exactly this
treatment against its declared control, freeze identical cases, fixtures,
blueprint digests, prompts, model settings, reviewer/judge budgets, credentials,
and schedules, and retain no product implementation.

Before a model call, a real connector adapter must independently qualify
readiness, bounded cancellation, reconnect classification, credential handling,
and minimal observation parsing against known-good and failure fixtures. Live
infrastructure failures must be reported separately from integration outcomes.

## Explicit non-goals

This branch does not contain or define:

- Stripe resource, account-context, field, or cross-node linkage checks;
- signed webhook delivery, forged/replay probes, forwarding, or any active
  request to an application;
- workflow retries, stale-result handling, completion sweeps, circuit breakers,
  reopening, or gating policy;
- Tree-sitter grammars, query packs, static-flow analysis, tracing, or causal
  correlation;
- blueprint metadata, canonical blueprint edits, App Roles, lifecycle
  semantics, or App Map;
- evaluator cases, fixtures, hidden verifiers, reviewer/judge code, model
  runners, reports, or evaluation results;
- process execution, IPC, daemon management, Docker, network clients,
  credentials on disk, environment-variable login, or ambient configuration;
- raw request/event payload retention, headers, response bodies, query strings,
  signing secrets, or a rendered TUI; or
- adoption by the existing Co-op runtime.

## Claim boundary

Only deterministic model-free contract behavior is claimed. No live Stripe
stream, application fixture, hidden verifier, model agent, reviewer, or judge
was exercised. This branch does not establish that any integration is correct,
that absence corresponds to a missing product action, or that a future agent
will use passive evidence well.
