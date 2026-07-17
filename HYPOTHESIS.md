# Verification Core Hypothesis

## Status and scope

This branch descends from the frozen candidate at
`d96ee4806bd9df1da2b7516efe4431d1cd0607a2`. Its treatment is the inert Go
package in `pkg/coop/verification`: stable result and check IDs, a versioned
result envelope, status and evidence vocabulary, failure domains, validation,
deterministic serialization, and policy-neutral indeterminate/fail-open
classification primitives.

The existing Co-op runtime does not import this package. The branch therefore
changes no current check, session, completion, or UI behavior.

No model evaluation has run on this branch. Unit tests and static analysis
exercise only the data contract. They do not establish that a Stripe
integration is correct, and this branch makes no correctness, quality,
efficiency, token, or runtime improvement claim.

## Hypothesis

If independently developed verification producers share one strict,
versioned, policy-neutral result contract, then later feature branches can
exchange and retain evidence without inventing incompatible meanings for
pass, failure, evidence gaps, collector outages, or identity.

The expected mechanism is earlier deterministic rejection of malformed or
ambiguous results plus byte-stable retained output. This is a hypothesis about
a shared primitive, not evidence that any future check or workflow will be
better.

## Deterministic observable

At the pinned branch commit, model-free tests must demonstrate all of the
following:

1. The documented stable-ID grammar accepts known-good IDs and rejects empty,
   malformed, uppercase, slash-containing, or overlength IDs.
2. Results with valid IDs, enums, status/domain relationships, and unique
   evidence keys validate; targeted contract mutants return an error.
3. `not_observed` and `unavailable` are indeterminate, while `passed`,
   `failed`, and `skipped` are not.
4. `FailsOpen` is true only for a CLI-owned, transient, `unavailable` result
   in the `collector` failure domain. Changing any member of that tuple makes
   the predicate false.
5. Equivalent result/evidence multisets serialize to identical compact JSON,
   sorted by result ID and evidence key, without reordering caller-owned
   slices.
6. Duplicate result IDs and unsupported schema versions fail validation.

The focused observable is:

```sh
go test -race ./pkg/coop/verification -count=1
```

The compatibility observable is:

```sh
go test ./pkg/coop/... -count=1
go vet ./pkg/coop/...
```

## Expected benefit

If later consumers adopt the contract correctly, expected benefits are:

- stable identities for manifests, reports, and cross-run comparisons;
- explicit separation of failures from missing or unavailable evidence;
- consistent evidence-handling labels without pretending those labels redact
  values;
- deterministic retained JSON that is easier to digest, diff, and audit;
- a narrow factual collector-outage classification that policy branches can
  consume without embedding policy in check producers.

These benefits remain unmeasured. In particular, this inert branch cannot
improve an evaluation outcome by itself.

## Known blind spots

- The contract does not execute checks or establish that evidence is true,
  complete, fresh, or causally related to an application action.
- Evidence classes do not redact, encrypt, authorize display, or prevent a
  producer from supplying sensitive material.
- Stable-ID allocation and cross-producer namespace governance are documented
  expectations, not a central registry.
- Deterministic JSON does not provide signatures, authentication, provenance,
  storage durability, or compatibility with non-Go consumers.
- The tests do not qualify production-scale result volumes or adversarially
  large strings.
- The package has no timestamps, coverage epochs, retry state, stale-result
  semantics, or workflow decisions.
- No live Stripe request, application fixture, hidden verifier, model agent,
  reviewer, or judge is exercised here.
- Only schema version 1 is implemented; future migration behavior is not yet
  qualified.

## Expected token and runtime cost

The current branch adds no model calls and consumes zero runtime model tokens.
Because the package is not imported, it adds no current Co-op execution cost.

For a future consumer, validation is linear in result and evidence count.
Deterministic serialization additionally sorts results and each result's
evidence, for expected cost of `O(R log R + sum(E_i log E_i))`, and copies the
result/evidence slices before sorting. Wire size and any prompt-token cost grow
linearly with the serialized fields a consumer chooses to expose. No numeric
prompt-token saving is claimed.

The focused test suite is expected to finish in seconds on a development
machine. Any future matched evaluation must freeze and report added serialized
bytes, implementation/reviewer tokens, turns, and wall-clock time rather than
assuming this primitive is free.

## Fail-open and fail-closed behavior

Contract validation and deterministic serialization fail closed: malformed
IDs, unknown enum values, inconsistent status/domain combinations, duplicate
evidence keys, duplicate result IDs, and unsupported schema versions return an
error and produce no serialized result set.

Indeterminate outcomes never become passes. `not_observed` means evidence was
absent or ambiguous; `unavailable` means its source could not be evaluated.
`skipped` is intentional non-execution and is not indeterminate.

`Result.FailsOpen` is only a factual classification primitive. It recognizes
the exact transient CLI collector-outage tuple described above, but it does
not advance a workflow, suppress a result, retry a check, or convert the
result to `passed`. Any future decision to continue despite that result
belongs on the separately isolated workflow-policy branch.

## Model-free qualification corpus

The retained known-good corpus in `verification_test.go` includes:

- opaque IDs using dot, underscore, colon, and hyphen separators;
- a valid passed CLI result with identifier and fingerprint evidence;
- a valid transient CLI collector outage that is indeterminate and matches
  the narrow fail-open predicate;
- an intentionally skipped agent result;
- equivalent result sets supplied in different result/evidence orders;
- a valid empty versioned result set and a JSON decode/validate round trip.

The targeted mutant corpus includes:

- missing, uppercase, separator-leading/trailing, whitespace-containing,
  slash-containing, and overlength IDs;
- missing result/check IDs and unknown source or status values;
- passed results with a failure domain or transient marker;
- failed results without a failure domain;
- unknown evidence classes and duplicate evidence keys;
- targeted mutations covering every member of the recognized fail-open tuple;
- duplicate result IDs and unsupported schema versions.

Qualification stops if a known-good fails, a targeted mutant is accepted, the
serializer mutates caller-owned ordering, or two equivalent multisets produce
different bytes.

## Criteria for a future matched evaluation

There is no useful standalone model evaluation for this inert shared core: a
model sees no behavior change until a concrete feature consumes it. A future
matched evaluation is justified only when all of these conditions hold:

1. This core's commit, source tree, schema version, and model-free corpus are
   pinned and passing.
2. Exactly one independently scoped product feature adopts the contract; no
   omnibus verification stack is used.
3. The control and treatment binaries are authenticated and invoked externally
   by an evaluator branch that contains no product implementation.
4. Control and treatment use identical blueprint digests, fixtures, prompts,
   model/reasoning settings, credentials, schedules, reviewer budgets, and
   evaluator-owned live assertions.
5. Primary outcomes and stop conditions are registered before execution,
   including live-assertion performance, false completion/blocking,
   infrastructure exclusions, token/turn cost, and runtime.
6. The consuming feature's known-goods pass and every targeted mutant fails
   before any model call.
7. Results report this core as a dependency rather than attributing the
   consuming feature's effect to the contract alone.

Until those criteria are met, only the deterministic model-free observables in
this document may be reported.

## Explicit non-goals

This branch does not contain or define:

- concrete verification checks or expected application behavior;
- passive log/event observers, webhook probes, or Stripe resource checks;
- Tree-sitter grammars, query packs, or static-flow analysis;
- retries, backoff, circuit breakers, stale-result handling, completion
  sweeps, reopening, gating, or any other workflow policy;
- tracing or causal-correlation protocols;
- evaluator cases, fixtures, hidden verifiers, reviewer/judge code, model
  runners, or evaluation results;
- blueprint overlays or changes to canonical blueprint JSON;
- credentials, Docker configuration, external services, or live Stripe calls;
- TUI presentation, user-facing claims, or automatic redaction;
- adoption by the existing Co-op runtime.
