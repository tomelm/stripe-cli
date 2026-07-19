# Co-op Resource Verification Treatment

## Product shape

Co-op resource verification is part of the existing agent lifecycle. There is
no public `stripe coop verify` command and no verification-specific user
action. A supported stage advertises digest-bound `stripe_resource_roles` in
the normal `agent start-work` JSON response. The agent supplies only IDs with
repeatable `stripe coop agent report-work --stripe-resource <role>=<id>`
flags.

Role/type pairs and checks come from package-owned, stage-specific overlays for
the six frozen blueprints. New references are deduplicated and persisted in the
session; relevant later stages automatically re-fetch retained references.
The session records the SHA-256 digest of the exact embedded canonical
blueprint bytes, and overlays refuse name-only or wrong-digest binding.

`report-work` performs one bounded, read-only Stripe pass using the existing
test/sandbox authentication path. Credentials stay process-local. Durable
results contain fixed details, allowlisted metadata, and fingerprints—not raw
API bodies, object IDs in result text, or keys. `sk_test_`, `rk_test_`, and
`rkcs_test_` credentials are supported.

## Semantics

- A resource declared newly created is confirmed only when its Stripe creation
  timestamp falls within the node's recorded `StartedAt`/`CompletedAt` window
  (expanded only to Stripe's second timestamp precision).
- A retained or reused resource is described only from its current observed
  state; verification does not claim the current node caused it.
- A missing role ID is `not_observed`.
- Missing credentials, account context, reader capability, or API support is
  `unavailable`.
- An observed wrong account, wrong mode, field mismatch, linkage mismatch, or
  creation-time contradiction is `failed`.
- Results are advisory. They do not gate workflow state, retry, fail fast,
  provision resources, or prompt the user.

Stable overlay expectations deliberately omit sample amounts, currencies,
descriptions, exact fees, and premature terminal statuses. Checks focus on
resource/account existence, invariant stage configuration, Stripe linkage,
active entitlement access where supported, meter event configuration, and
Connect destination/commission presence.

## Bounded scope

The concrete reader uses injected Stripe request infrastructure and supports
only the finite object types and narrow entitlement/meter reads declared by
this treatment. Each read has a package-owned timeout; each report has an
overall deadline; multiple references are deterministically bounded. There
are no passive observers, webhook probes, application drivers, background
daemons, workflow policies, evaluator changes, or canonical blueprint edits.

## Model-free validation

```sh
go test ./pkg/coop/... ./pkg/cmd/coop/...
go test -race ./pkg/coop/... ./pkg/cmd/coop/...
go vet ./pkg/coop/... ./pkg/cmd/coop/...
go build -o bin/stripe ./cmd/stripe
```

No live Stripe qualification, Docker run, or model evaluation is part of this
treatment.
