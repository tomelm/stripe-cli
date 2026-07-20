# Co-op verification contract

This package defines the policy-neutral result contract for Co-op passive
verification. Results are produced by the session-owned passive observer
(`pkg/coop/observe`), run by the Co-op TUI process through
`pkg/coop/verification/runtime`, and stored on session nodes as
`SessionNode.VerificationResults`. They are advisory only: no result confirms,
gates, reopens, or advances workflow.

The contract includes:

- opaque, validated `CheckID` and `ResultID` values;
- `passed`, `failed`, `inconclusive`, `not_observed`, `unavailable`, and
  `skipped` statuses;
- integration, collector, and coverage failure domains;
- safe and sensitive evidence classifications;
- versioned result envelopes.

`not_observed` means the expected activity was not seen during healthy
coverage. `unavailable` means the evidence source could not be evaluated.
Both are indeterminate and remain distinct from a pass.

The `Sanitizer` and `UpsertResult` keep durable results bounded (at most 24
per node, details ≤240 bytes) and strip credential material before results
reach disk. `AgentSummaries` is the bounded, evidence-free projection shared
with agent-facing command responses.
