# Co-op verification contract

This package defines the durable result contract used by Co-op's automatic
Stripe resource verification. `pkg/coop/workflow` imports it in the
`report-work` path: results are persisted on the session node before the node
can move to review, and bounded summaries are projected back to the agent.

A result is three fields — a stable ID, a status, and a human-readable
detail — and the whole policy lives in the status:

- `passed` — the check ran and the resource matched the blueprint.
- `failed` — a deterministic contradiction (wrong value, missing object,
  missing required ID). Report-work keeps the node active so the agent can
  repair and re-report.
- `unavailable` — the check could not run (missing credentials, unsupported
  API access, rate limits, incomplete bounded lookups). This fails open to
  normal human review and must never be presented as a pass.

The `Sanitizer` removes process-local credential material and recognizable
Stripe credential patterns from details before persistence, `UpsertResult`
keeps per-node storage bounded and deterministic, and `AgentSummaries`
projects a bounded, redacted view for the agent-facing JSON response.
Producers must keep credentials and raw resource IDs out of result IDs and
details; the sanitizer is defense in depth, not the primary control.
