# Co-op verification contract

This package defines the durable result contract used by Co-op's automatic
Stripe resource verification. `pkg/coop/workflow` imports it in the
`report-work` path: results are persisted on the session node before the node
can move to review, and evidence-free summaries are projected back to the
agent.

The contract includes:

- opaque, validated `CheckID` and `ResultID` values;
- `passed`, `failed`, `not_observed`, `unavailable`, and `skipped` statuses;
- integration, application, collector, coverage, and safety failure domains;
- safe, identifier, fingerprint, and sensitive evidence classifications;
- a `Sanitizer` that removes credential material before results are persisted;
- bounded per-node storage (`UpsertResult`) and bounded agent-facing
  projections (`AgentSummaries`).

`not_observed` means the relevant evidence was absent or ambiguous.
`unavailable` means the evidence source could not be evaluated (missing
credentials, unsupported API access, rate limits). Both remain distinct from a
pass: workflow policy treats deterministic contradictions as blocking and
`unavailable` as fail-open to normal human review, and consumers must never
rewrite either as `passed`.

IDs are durable API values: producers should persist and reuse them rather
than build them from display text, timestamps, or random data.

Evidence classes describe handling requirements only. They do not redact the
`Value` field, prove a claim, or authorize displaying a value. Producers must
keep credentials and other secret material out of durable result data; the
`Sanitizer` additionally redacts recognizable Stripe credential patterns and
process-local key material as defense in depth.
