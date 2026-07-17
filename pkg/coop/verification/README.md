# Co-op verification contract

This package provides policy-neutral building blocks for future Co-op
verification features. It is intentionally not imported by the current Co-op
workflow, so adding it does not change session behavior or completion rules.

The contract includes:

- opaque, validated `CheckID` and `ResultID` values;
- `passed`, `failed`, `not_observed`, `unavailable`, and `skipped` statuses;
- integration, application, collector, coverage, and safety failure domains;
- safe, identifier, fingerprint, and sensitive evidence classifications;
- a narrow fail-open predicate for transient, CLI-owned collector outages;
- versioned result envelopes with deterministic JSON serialization.

`not_observed` means the relevant evidence was absent or ambiguous.
`unavailable` means the evidence source could not be evaluated. Both are
indeterminate and remain distinct from a pass. `skipped` records intentional
non-execution and is not treated as indeterminate.

`Result.FailsOpen` is a classification primitive, not a workflow policy. It is
true only for an `unavailable` result produced by the CLI, attributed to the
collector, and marked transient. Consumers decide how to present or act on the
result and must never rewrite it as `passed`.

`ResultSet.MarshalDeterministic` validates the envelope and orders results by
result ID and evidence by key without mutating caller-owned slices. IDs are
durable API values: producers should persist and reuse them rather than build
them from display text, timestamps, or random data.

Evidence classes describe handling requirements only. They do not redact the
`Value` field, prove a claim, or authorize displaying a value. Producers must
keep credentials and other secret material out of durable result data.
