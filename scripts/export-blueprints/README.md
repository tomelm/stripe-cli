# Blueprint Exporter

Converts exported Workbench Blueprint JSON into CLI-friendly JSON for co-op mode.

## Pipeline

```
pay-server/blueprintDefinitions/*.tsx
    ↓  (pay-server: pay js:run export)
pay-server/dist/blueprints/*.json
    ↓  (this script)
pkg/coop/blueprints/*.json  (checked into stripe-cli and embedded via //go:embed)
```

## Usage

### From pay-server source

Point `BLUEPRINT_SOURCE` at pay-server's `src/blueprintDefinitions` directory.
The Make target runs pay-server's exporter first, then transforms the exported
JSON into the CLI schema:

```bash
BLUEPRINT_SOURCE=/path/to/pay-server/frontend/workbench/shared/blueprints/src/blueprintDefinitions make sync-blueprints
```

By default, the Make target syncs Workbench learning blueprints that represent
merchant integration guides. Testing blueprints, examples, health alert helpers,
and partner-certification/dashboard-only flows are left out of the coop catalog.
To try exporting every pay-server blueprint, run with `BLUEPRINT_IDS=all`.

### From exported JSON

If pay-server has already produced `dist/blueprints/*.json`, point
`BLUEPRINT_SOURCE` there:

```bash
BLUEPRINT_SOURCE=/path/to/pay-server/frontend/workbench/shared/blueprints/dist/blueprints make sync-blueprints
```

## What gets stripped

- `MessageDescriptor` objects → resolved to `defaultMessage` string
- React JSX components (`display`, `messageDescriptorFormatters`) → removed
- Dashboard-only nodes (`settingsUpdate`, `contactStripe`, etc.) → removed
- Environment conditions (`hiddenIfEnvMatchesOne`, etc.) → removed

## What gets kept

- Blueprint structure: id, title, description, type, steps
- Node types: apiRequest, asyncHandler, uiComponent, testHelper, dashboard
- API request details: path, method, params (from first configuredDetails)
- Interpolation strings: `${node.chapter.node:field}` preserved as-is
- Event types for asyncHandler nodes and explicit UI verification contracts
- Product-authored review prompts and commands, when present upstream
- Product-authored lifecycle facts and required application outcomes
- Product metadata

The exporter does not manufacture review prompts or `stripe trigger` commands.
Generic guidance is a CLI presentation fallback, and a trigger command is only
safe when the upstream blueprint explicitly declares one. Not every Stripe
event has a corresponding CLI trigger fixture. It also does not infer UI event
contracts from neighboring steps; those must be declared on the UI node.

### Application outcome contract

Workbench may declare provider lifecycle facts at the blueprint root:

```json
{
  "lifecycleFacts": [
    {
      "id": "redirect_not_proof",
      "statement": "A success redirect is not proof of subscription state."
    }
  ]
}
```

A node can reference those facts while stating a required application outcome:

```json
{
  "requiredOutcomes": [
    {
      "id": "verified_checkout_return",
      "factRefs": ["redirect_not_proof"],
      "statement": "Retrieve and correlate the returned Checkout Session server-side."
    }
  ]
}
```

The exporter accepts either camelCase upstream names or their snake_case CLI
equivalents and always emits `lifecycle_facts`, `required_outcomes`, and
`fact_refs`. It only preserves explicitly authored fields; it never derives an
application contract from descriptions, node topology, or a Stripe rule.

These fields are public implementation guidance, not a hidden verifier.
Co-op's Stripe readers cannot prove application-owned persistence,
authorization, idempotency, or reconciliation, so each required outcome is
reported as an explicit automatic-check-unavailable result. Agent-reported
checks remain claims, and UI confirmation remains human judgment. Account-wide
UI observations can trigger authoritative Stripe rereads, but cannot create an
attempt-attributed failure without a unique trusted resource binding.

`subscription-with-trial` is the first vertical slice. It describes durable
signup and access behavior, but the tutorial is still only a partial
production lifecycle: cancellation, payment failure, recovery, and related
lifecycle operations must be added to the upstream Workbench blueprint before
the tutorial can claim production completeness.
