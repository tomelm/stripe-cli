# Stripe Resource Checks Hypothesis

## Status and scope

This branch descends exactly from verification core
`1f9347a43c64a8b24e4a52e53f695baf269c3ba2`. Its treatment is the isolated
`pkg/coop/resourcecheck` package: read-only, test-mode existence, scalar-field,
account-context, and cross-resource linkage checks that emit verification-core
results.

The package is not wired into the existing Co-op workflow. It accepts no API
key, performs no concrete HTTP request, mutates no Stripe object, and applies no
completion or retry policy. No model evaluation or live Stripe run occurred on
this branch.

## Hypothesis

If resource verification uses one explicit test-mode account context, an
injected read-only metadata boundary, fixed result semantics, and bounded
redacted evidence, Co-op can corroborate Stripe resource state without
conflating collector outages, safety violations, evidence gaps, or integration
contradictions.

## Deterministic observable

Model-free tests must demonstrate that:

1. Check and result IDs validate before reader I/O.
2. A direct or bounded-list existence check passes only for a normalized
   test-mode resource in the expected account.
3. Field checks compare normalized values in memory while retaining only the
   field path and `match`, `mismatch`, or `absent` outcome.
4. Account checks fail safely for live-mode metadata and fail as integration
   contradictions for a different account.
5. Linkage checks compare the source reference and then fetch the declared
   target under the same account context.
6. Authorization and malformed metadata never fail open; only a CLI-owned,
   transient collector outage satisfies `Result.FailsOpen()`.
7. Durable results contain fixed details and bounded fingerprints, never raw
   field values, upstream errors, account IDs, resource IDs, request bodies, or
   credentials.
8. Concurrent use passes the race detector when the injected reader is safe.

The focused observables are:

```sh
go test -race ./pkg/coop/resourcecheck -count=1
go test ./pkg/coop/verification ./pkg/coop/resourcecheck -count=1
go vet ./pkg/coop/resourcecheck
```

## Result semantics

- A corroborated resource, field, account, or linkage is `passed`.
- Authoritative absence, field mismatch, wrong account, or link mismatch is
  `failed / integration`.
- A live-mode resource is `failed / safety`.
- Missing read authority is `unavailable / safety` and non-transient.
- Malformed or oversized normalized metadata is
  `unavailable / collector` and non-transient.
- A bounded list with `has_more=true` that did not contain the target is
  `not_observed / coverage`.
- A declared transient source outage or deadline is
  `unavailable / collector / transient=true`; it remains indeterminate and is
  the only fail-open tuple.

These are producer classifications only. This branch contains no policy that
retries, suppresses, gates, or converts results.

## Expected benefit

- Concrete Stripe state can be corroborated independently of model prose.
- Explicit account and mode checks reduce accidental cross-account or live-mode
  claims.
- Stable check IDs and redacted evidence can be retained and compared by later
  evaluator branches.
- The injected reader keeps credential ownership and transport policy outside
  the check package.
- One-page bounded list checks expose incomplete coverage rather than treating
  it as authoritative absence.

## Blind spots

- A passing check establishes observed Stripe state, not that the candidate
  application created it or caused it correctly.
- Normalization quality and API authorization remain reader responsibilities.
- Scalar string equality does not implement numeric, temporal, set, or nested
  semantic comparison.
- A direct fetch may race with external mutation; the package has no causal
  protocol or observation epoch.
- Fingerprints conceal identifiers in retained output but are not encryption or
  access control.
- No real Stripe account, network fault, pagination sequence, or model behavior
  is qualified here.

## Expected cost

Each direct check performs at most one fetch, except linkage, which performs at
most two. A list check requests exactly one page capped at 100 resources and
never paginates or retries. Validation and comparison are linear in bounded
normalized fields/links. There is no model-token cost on this branch.

## Fail-open and fail-closed behavior

Invalid configuration, IDs, types, fields, account context, or list limits fail
before reader I/O. Live-mode metadata fails closed as a safety contradiction.
Unauthorized reads and malformed responses are indeterminate but never fail
open. Only the exact verification-core transient collector tuple returns true
from `FailsOpen`; the result remains `unavailable` rather than becoming a pass.

Raw reader errors are never retained. Unknown reader failures use a fixed,
non-transient collector-unavailable result. The package does not catch panics or
weaken the injected reader contract.

## Model-free qualification corpus

Known-good cases cover existence, bounded list membership, field equality,
test-mode account ownership, two-resource linkage, deterministic redacted
serialization, and concurrent use. Targeted mutants cover not-found,
incomplete list coverage, duplicate/oversized list output, malformed identities
and metadata, live mode, wrong account, field absence/mismatch, link
absence/mismatch, unauthorized, transient, deadline, canceled, and unknown
reader failures, plus invalid configuration that must stop before I/O.

Qualification stops if any result violates verification-core validation, any
secret/raw value appears in deterministic output, any non-transient or safety
case fails open, any call exceeds its bounded read count, or the race suite
fails.

## Future matched-evaluation criteria

No model evaluation is authorized on this product branch. After model-free
qualification and separate approval, `tomer/coop-evals-resource-checks` should
compare exactly one resource-check treatment against the frozen baseline. The
evaluator branch must invoke authenticated binaries externally and must not
merge this package. Both arms require identical cases, fixtures, account scope,
models, reasoning, schedule, reviewer, and judge; all live assertions and
cleanup rules must be registered before any model call.

## Explicit non-goals

This branch does not implement observers, webhook delivery or replay probes,
static analysis, tracing, application-state probes, write APIs, fixture setup,
pagination loops, retries, stale-result policy, circuit breakers, completion
sweeps, reopening, workflow gates, evaluator code, model runs, UI integration,
or automatic session adoption. It does not modify blueprint JSON.

## Claim boundary

This branch makes no integration-correctness, causality, model-quality, or
efficiency claim. Its only claim is that the model-free package behavior above
passes its deterministic tests against injected readers.
