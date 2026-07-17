# Stripe Resource Checks Hypothesis

## Status and scope

This branch descends exactly from verification core
`1f9347a43c64a8b24e4a52e53f695baf269c3ba2`. Its treatment is the isolated
`pkg/coop/resourcecheck` package: read-only, test-mode creation-window
existence, typed scalar-field, account-context, and provenance-bound linkage
checks that emit verification-core results.

The package is not wired into the existing Co-op workflow. It accepts no API
key, performs no concrete HTTP request, mutates no Stripe object, and applies no
completion or workflow retry policy. No model evaluation or live Stripe run
occurred on this branch.

## Hypothesis

If resource verification accepts only trusted type/ID descriptors, binds
existence to a bounded action window, discovers a unique fallback resource
through typed structural predicates rather than trusting a guessed ID, carries
scope-bound passed CLI observations into downstream checks, compares canonical
typed JSON scalars, and bounds every injected read, then Co-op can corroborate
Stripe state without turning missing pointers, ambiguous coverage, or
caller-controlled deadlines into integration failures or fail-open collector
outages.

## Deterministic observable

Model-free tests must demonstrate that:

1. Check/result IDs, verification session, blueprint digest, node IDs,
   test-mode account context, supported resource types, compatible ID prefixes,
   non-placeholder resource/account IDs, creation windows, structural
   predicates, scalar values, list limits, and read timeouts validate before
   reader I/O.
2. Exact existence passes only when a normalized test-mode resource belongs to
   the expected account and its immutable creation time falls in the declared
   half-open action window.
3. The ID-free fallback requires one to eight typed structural equality
   predicates and issues one type/window/predicate-filtered list call capped at
   100 resources. It independently revalidates every returned predicate and
   passes with provenance only for exactly one match on a complete page. Zero,
   multiple, or `has_more` results are `not_observed / coverage`; a reader that
   returns a nonmatching candidate is malformed and non-transient.
4. Existence/window APIs mint an `ObservedResource` only alongside a passed CLI
   result. Its unexported process-local provenance binds it to the creating
   checker, verification session, blueprint digest, blueprint node, result ID,
   resource identity, account, and observed creation time; JSON persistence or
   reconstruction is rejected.
5. Field and account checks require one such observation. Linkage requires
   distinct, prior passed observations for both source and target, rejects bare
   or foreign capabilities before I/O, compares the source link, and re-fetches
   the exact target. A previously observed source or target that disappears is
   an integration contradiction.
6. Field checks distinguish JSON string, number, boolean, null, empty string,
   and absent. Equivalent finite number spellings compare canonically, while
   results retain only scalar kinds and `match`, `mismatch`, or `absent`.
7. Every fetch and list call receives a package-owned deadline no longer than
   30 seconds. A pre-expired caller context performs no I/O; caller and internal
   deadlines never manufacture a transient result.
8. Only an explicit reader `ErrTransientUnavailable` maps to the exact
   CLI-owned `unavailable / collector / transient=true` tuple recognized by
   `Result.FailsOpen`. Authorization, malformed metadata, unknown errors,
   cancellation, and deadline errors never match it.
9. Durable results contain fixed details and bounded fingerprints, never raw
   scalar values, upstream errors, account IDs, resource IDs, request bodies,
   or credentials.
10. Concurrent use passes the race detector when the injected reader is safe.

The focused observables are:

```sh
go test ./pkg/coop/resourcecheck -count=1
go test -race ./pkg/coop/resourcecheck -count=1
go test ./pkg/coop/verification ./pkg/coop/resourcecheck -count=1
go vet ./pkg/coop/resourcecheck
go build ./pkg/coop/resourcecheck
```

## Result semantics

- An exact in-window resource, unique complete-window discovery, typed field,
  account context, or provenance-bound linkage is `passed`.
- A missing unproven exact resource, exact resource outside its action window,
  zero or multiple window candidates, or incomplete page is
  `not_observed / coverage` and non-transient.
- A field absence/mismatch, wrong account, source-link absence/mismatch, or
  disappeared previously observed source/target is `failed / integration`.
- A live-mode resource is `failed / safety`.
- Missing read authority is `unavailable / safety` and non-transient.
- Malformed/oversized normalized metadata, a reader contract violation, caller
  cancellation/deadline, and internal timeout are `unavailable / collector`
  and non-transient.
- Only explicit `ErrTransientUnavailable` is
  `unavailable / collector / transient=true`.

These are producer classifications only. This branch contains no policy that
retries reads, suppresses results, gates progress, or converts a result.

## Expected benefit

- Creation windows tie observed resources to an action interval without
  claiming full causality.
- A bounded, structurally filtered ID-free unique search avoids treating
  model-supplied placeholder or guessed IDs as authoritative pointers while
  preventing a same-type singleton that does not satisfy the declared
  structure from establishing provenance.
- Package-owned descriptors reject unknown type/ID combinations before they
  can reach a credential-owning adapter.
- Scope/node/result-bound in-memory observation capabilities stop downstream
  checks from accepting bare references and stop linkage from passing solely
  because a caller copied the source link into an expected target.
- Canonical JSON scalars prevent number/string, boolean/string, null/empty, and
  null/absent conflation.
- Package-enforced deadlines bound cooperative readers while keeping caller
  cancellation outside the transient collector predicate.
- Stable check IDs and redacted evidence can be retained by later evaluator
  branches.

These benefits are unmeasured and make no integration-correctness, causality,
model-quality, token-efficiency, or runtime-efficiency claim.

## Blind spots

- A passing creation-window check proves observed Stripe state in an interval,
  not that the candidate application caused it or used it correctly.
- Window-search completeness depends on the reader applying the requested type,
  half-open time, and structural filters correctly; malformed returned
  candidates are detected, but omitted resources cannot be inferred.
- Readers own credentials, authorization, normalization, and transport and
  must honor context deadlines. A deliberately non-cooperative reader can
  retain its own blocked call after the deadline.
- The trusted descriptor vocabulary is intentionally finite and requires an
  explicit code/test change for another Stripe resource family or ID prefix.
- Provenance capabilities are process-local and intentionally cannot be
  reconstructed from an agent-authored result or bare resource reference.
- Scalar comparison covers only JSON string, finite decimal number, boolean,
  and null. Objects, arrays, timestamps, sets, and nested semantic comparison
  remain out of scope.
- Direct reads can race with external mutation; the package has no causal
  trace, transaction snapshot, or observation epoch.
- Fingerprints conceal identifiers in retained output but are not encryption
  or access control.
- No real Stripe account, network fault, pagination sequence, or model behavior
  is qualified here.

## Expected cost

Each direct check performs at most one fetch, except linkage, which performs at
most two after earlier successful source and target observations. An ID-free
fallback requests exactly one page, capped at 100 normalized resources, and
never paginates or retries. Every call receives a five-second default internal
deadline; qualified callers may choose a positive bound no greater than 30
seconds.

Validation and comparison are linear in bounded normalized fields/links and
list size. Decimal canonicalization is bounded to 4096 encoded bytes. Each
passed observation retains one resource identity, one result ID, one creation
timestamp, verification scope, node ID, and an in-memory checker provenance
pointer. There is no model-token cost on this branch.

## Fail-open and fail-closed behavior

Invalid configuration, unsupported types, incompatible or placeholder IDs,
invalid fields/scalars/predicates, account or verification scope, windows, list
limits, or timeout bounds fail before reader I/O. Live-mode metadata fails
closed as a safety contradiction. Unauthorized reads and malformed responses
are indeterminate but never fail open.

A pre-expired caller context returns non-transient
`unavailable / collector` without calling the reader. Mid-read caller
cancellation, caller deadline, package deadline, and a raw
`context.DeadlineExceeded` are also unavailable and always non-transient. Only
a reader's explicit `ErrTransientUnavailable` produces the inherited fail-open
tuple, and that result remains `unavailable`; it never becomes a pass or
workflow decision.

Raw reader errors are never retained. Unknown reader failures use a fixed,
non-transient collector-unavailable result. The package does not catch panics,
retry reads, or weaken the injected reader contract.

## Model-free qualification corpus

Known-good cases cover exact in-window existence, exactly-one complete window
discovery without an ID, provenance minting, typed string/number/bool/null and
empty-value equality, test-mode account ownership, provenance-bound
two-resource linkage, deterministic redacted serialization, package deadlines,
and concurrent use.

Targeted mutants cover resource creation outside the action window, missing or
duplicate structural predicates, a same-type candidate with the wrong
predicate, zero and multiple matches, incomplete and oversized pages,
out-of-window list results, unsupported types, wrong prefixes, short and long
placeholder resource/account IDs, malformed identities/metadata, live mode,
wrong account, string/number and string/boolean confusion, null/absent
confusion, link absence/mismatch, bare or foreign source/target capabilities,
disappearance or changed creation metadata after a proven observation,
pre-expired and mid-read caller deadlines, package timeout, unauthorized,
explicit transient, canceled, deadline, and unknown reader failures.

Qualification stops if any result violates verification-core validation, any
secret/raw value appears in deterministic output, an ambiguous window mints
provenance, an unproven source or target reaches reader I/O, any deadline fails
open, any non-explicit transient case fails open, or the race suite fails.

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
pagination loops, transport retries, stale-result policy, circuit breakers,
completion sweeps, reopening, workflow gates, evaluator code, model runs, UI
integration, automatic session adoption, or concrete credential/network
adapters. It does not modify blueprint JSON.

## Claim boundary

This branch makes no integration-correctness, causality, model-quality, or
efficiency claim. Its only claim is that the model-free package behavior above
passes deterministic tests against injected cooperative readers.
