# Passive Observers Hypothesis

## Status and scope

This branch descends exactly from verification-core commit
`1f9347a43c64a8b24e4a52e53f695baf269c3ba2`. Its treatment is the inert
package in `pkg/coop/observe`: explicit per-session inputs, concrete passive
adapters over the CLI's existing `logs tail` and `listen` transports, bounded
startup and observation windows, collector health state, reconnect timing,
health epochs, minimal observations, TUI-facing summaries, and availability
results expressed with verification-core primitives. Narrow shared-transport
seams in `pkg/stripe`, `pkg/websocket`, `pkg/logtailing`, and `pkg/proxy` permit
an explicit direct HTTP/WebSocket transport, synchronous event ownership,
cancellable readiness, a passive-only 4 MiB inbound-message limit, and ordinary
disconnect notification. Existing command callers retain their default ambient
transport selection and unlimited-message default.

The adapters use only the API key, device, account, event filter, and API base
explicitly supplied for that session. The concrete adapter supplies direct
HTTP and WebSocket transports that do not consult proxy environment variables
or `STRIPE_CLI_UNIX_SOCKET`; it does not spawn a CLI process or read ambient
Stripe login or configuration. It shadows inherited Stripe telemetry with a
no-op client before authentication. Alternate API bases are parsed against
exact host/scheme/path rules, and WebSocket downgrade is allowed only with an
explicit loopback HTTP API base. The existing Co-op runtime does not instantiate
the adapter, and no live Stripe connection, model evaluation, or live
qualification has run on this branch.

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
   bounded startup window. WebSocket readiness uses a new channel generation
   after every disconnect; cancellation and close cannot report a stale or late
   ready epoch. A successfully connected session resets the authorization retry
   counter before a later session expiry.
4. Transient connect, startup, and stream failures enter `retrying` with capped
   exponential backoff and injected jitter. A permanent classified failure
   enters `unhealthy`; an explicit, coalesced `RetryNow` starts a new attempt.
5. Each reconnect creates a new monotonically numbered health epoch and a gap.
   Snapshots expose only bounded request/event metadata. A malformed or unknown
   WebSocket frame, known frame from the wrong stream or session mode, malformed
   inner log/event payload, or source-incompatible observation ends the current
   epoch as a bounded transient stream gap; it can never leave an absence window
   usable.
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
10. The concrete connector invokes the in-process Stripe logs-tail/listen
    transports with explicit credentials, treats an internal reconnect as a
    supervisor-visible coverage gap, strips request query/fragment data,
    prevents the listen signing secret and duplicate marshaled payloads from
    entering adapter output, defensively ignores any readiness data, limits one
    inbound WebSocket message to 4 MiB before JSON decoding, and turns oversized
    messages or bounded-buffer overflow into transient collector unavailability.
    Its authentication context cannot invoke an inherited telemetry client.
11. A hermetic local API and real WebSocket exercise the production session,
    readiness, ordinary disconnect, malformed outer frame, malformed inner
    event, malformed nested request, wrong-family frame, cancellation, drain,
    and supervisor-gap paths while hostile ambient proxy/Unix-socket variables
    and a counting telemetry client are set. Runner return also terminates an
    injected stream that omits a redundant output-channel close. No external
    network is used.

The focused observables are:

```sh
go test ./pkg/coop/observe -count=1
go test -race ./pkg/coop/observe -count=1
go vet ./pkg/coop/observe
go build ./pkg/coop/observe
go test -race ./pkg/coop/observe -run 'TestStripeConnectorProduction(Oversized|Malformed|LogsRejects|ListenRejects)' -count=10
go test -race ./pkg/websocket ./pkg/logtailing ./pkg/proxy ./pkg/stripe -count=1
go test -race ./pkg/logtailing ./pkg/proxy -run 'TestRunSuccessfulExpiredSessionsResetAuthorizationAttempts' -count=3
go vet ./pkg/websocket ./pkg/logtailing ./pkg/proxy ./pkg/stripe
go build ./pkg/websocket ./pkg/logtailing ./pkg/proxy ./pkg/stripe
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
  query strings, signing secrets, and duplicate marshaled payloads;
- a small presentation-neutral state projection for a later TUI; and
- consistent environmental-unavailability classification through the shared
  verification contract.

These benefits are unmeasured. The branch makes no integration-correctness,
model-quality, runtime-efficiency, or token-efficiency claim.

## Known blind spots

- A hermetic local API and real WebSocket exercise the production session,
  readiness, ordinary-disconnect, malformed-frame and malformed-event,
  cancellation, and shutdown paths. Live Stripe authentication, permissions,
  server-driven session expiry, and Internet network behavior remain
  unqualified.
- A ready transport does not prove that Stripe will emit every relevant fact,
  that filters are correct, or that an observation corresponds to a particular
  application action.
- `AbsenceUsable` establishes only uninterrupted passive coverage plus zero
  bounded observations. It is not a product assertion and cannot prove that a
  required request or event should have happened.
- Request observations retain normalized paths and event observations retain
  event types; downstream display and retention policy remain separate work.
- Bounded startup still depends on third-party connector implementations
  honoring context cancellation. The concrete connector drains until its owned
  runner terminates; an unrelated injected connector can violate that contract.
- Tests use scripted connectors, injected clocks, and a hermetic local
  production-path server. They do not exercise live account permissions,
  Internet partitions, or a real Stripe session.
- The passive adapters cap one inbound WebSocket message at 4 MiB before JSON
  decoding. That is a conservative transport-safety bound, not evidence that a
  near-limit payload is cheap or valid; a legitimate larger Stripe message
  would deliberately break coverage and require separate limit review.
- This branch does not correlate request and event streams, application state,
  blueprint obligations, resources, or durable application state.

## Expected cost

The existing Co-op runtime does not import this package, so current runtime and
model-token cost is zero. A future adopter would keep one supervisor goroutine
and a small fixed set of owned per-attempt transport goroutines per passive
stream, plus bounded channel/snapshot state, minimal last-observation metadata,
health-epoch history, and caller-opened coverage tokens. WebSocket event
handling is synchronous with the owned reader, and shutdown keeps draining
until producers terminate; it does not create a goroutine per received event.
Observation ingestion and snapshots are constant-time apart from lock
contention; retained epoch storage grows linearly with session reconnects,
while unfinished coverage-window storage is capped at 64 tokens per supervisor.

Reconnect delay doubles from the configured initial delay, applies symmetric
injected jitter, and never exceeds the configured maximum. Startup and action
observation windows are each capped at 24 hours by validation. No quantitative
latency or memory improvement is claimed.

## Fail-open and fail-closed behavior

Configuration validation fails closed before I/O for missing explicit
credentials, unknown streams, event filters absent from the pinned event
snapshot, malformed bounded text, unbounded durations, or invalid
backoff/jitter parameters. Exact API-base parsing also fails closed before a
credential-bearing request, and plaintext WebSocket downgrade requires an
explicit loopback base. Malformed connector contracts become non-transient
`connector_invalid`. Malformed or unknown WebSocket content, malformed inner
log/event payloads, malformed nested request fields, wrong-family or wrong-mode
frames, and source-incompatible observations retain neither raw payload nor raw
error; they end the ready epoch as a transient `stream_closed` gap, so any
spanning coverage window is unusable for absence. A WebSocket message above the
passive 4 MiB limit follows the same no-retention gap path.

Environmental connection unavailability, startup timeout, stream closure, or
bounded observation-buffer overflow produces a CLI-owned, transient,
`unavailable` verification result in the `collector` failure domain. That exact
tuple is recognized by the inherited `verification.Result.FailsOpen` primitive.
Authentication rejection and connector-contract errors are non-transient and
do not match it. An intentional stop is `skipped`, and a ready collector is
`passed`.

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
- a permanent authentication failure recovered only by manual retry;
- concrete logs-tail/listen element adaptation that discards query strings and
  omits duplicate raw payloads and readiness secrets, plus a
  supervisor-visible reconnect boundary;
- a hermetic local API and real WebSocket ordinary-disconnect path that ignores
  hostile ambient proxy and Unix-socket configuration;
- malformed outer WebSocket JSON, malformed inner event content, and
  source-incompatible elements producing a health gap that invalidates a
  spanning zero-activity window;
- webhook/v2 frames on logs-tail, request-log frames on listen, and v2 frames
  outside the declared listen mode producing the same no-retention gap;
- malformed nested request fields returning an error rather than panicking;
- an oversized real WebSocket message rejected before JSON decoding, with no
  retained observation and an unusable spanning absence window;
- four successive successfully connected then expired sessions reauthorizing
  for ordinary logs-tail and listen callers, plus passive telemetry shadowing; and
- concurrent snapshots, summaries, epoch reads, result creation, and
  observation ingestion under the race detector.

The targeted failure corpus includes:

- missing explicit credentials, wrong-stream and unknown listen filters,
  overlong timeouts, and invalid jitter samples;
- deceptive API-base hosts, production plaintext downgrade, canceled failed
  WebSocket setup, repeated stop, stale readiness after reconnect, and runner
  return without an output-channel close;
- query-bearing request paths, wrong-stream facts, and empty observations;
- malformed or unknown WebSocket frames, malformed inner event content, and
  source-incompatible elements that must not preserve usable absence;
- known but wrong-stream/wrong-mode frames and non-string nested request fields;
- an inbound WebSocket message one byte above the passive 4 MiB limit;
- startup timeout and a canceled readiness wait followed by a late signal;
- a reconnect inside an action window, an expired observation window, a clock
  regression, reuse of a consumed window token, and buffered facts immediately
  preceding a terminal gap;
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

Before a model call, the concrete adapters must independently live-qualify
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
- process execution, IPC, daemon management, Docker, credentials on disk,
  environment-variable login, or ambient configuration;
- raw request/event payload retention, headers, response bodies, query strings,
  signing secrets, or a rendered TUI; or
- adoption by the existing Co-op runtime.

## Claim boundary

Only deterministic model-free contract behavior is claimed. No live Stripe
stream, application fixture, hidden verifier, model agent, reviewer, or judge
was exercised. This branch does not establish that any integration is correct,
that absence corresponds to a missing product action, or that a future agent
will use passive evidence well.
