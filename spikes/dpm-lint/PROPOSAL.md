# Proposal: `stripe doctor` — a migration doctor for the Stripe CLI

**Status:** working prototype, adversarially reviewed · **Branches:** `tomer/dpm-test`
(core), `tomer/dpm-packs` (multi-pack experiment, superset) · **Updated:** 2026-07-31

## Summary

Generic migration verbs with migrations as data:

```
stripe doctor [topic] [dir]   diagnose: code findings judged against live account facts
       --live                   + behavioral proof (webhook round-trip via listen/trigger)
       --offline                scan-only, explicitly
stripe fix    [topic] [dir]   remediate: dry-run default; --apply writes only
                              reparse-verified files; risky findings are gated
stripe demo   [topic]         guided human walkthrough
stripe guide                  agent playbook (--json + exit-code contract)
```

Topics are a registry of rule packs. Five exist today: `dpm` (Dynamic Payment
Methods, fixable) plus four advise-only packs (`tax-percent`, `collection-method`,
`prorate`, `source-types`) authored from changelog research to prove the registry
claim. The engine scans 7 languages (Ruby, Python, PHP, JS/TS + JSX/TSX/ESM, Go,
Java, C#) with pure-Go tree-sitter under `CGO_ENABLED=0`.

Every command speaks both audiences: styled human output, and `--json` (pure JSON
on stdout, logs on stderr) with exit codes `0` clean/verified, `1`
findings/not-verified, `2` error. A fresh agent following `stripe guide` literally
completes the loop: doctor (exit 1) → fix --apply → doctor (exit 0 = migrated).

## How the shape changed since the first draft

The original plan was a scan-only linter with account checks deferred to a distant
"doctor" phase. Three findings inverted that:

1. **Matching is not advising** (still the central finding): on the canonical
   sample every technically-correct finding was *wrong advice* — correctness
   depends on account facts (API version, Dashboard config) and frontend surface.
   So the account layer moved from phase 4 into the core: `doctor` *is* the
   product, and scan-only is just its credential-less degradation (`verdict_class:
   UNKNOWN`, `.degraded` says why).
2. **The account layer was cheap** (three read-only GETs with the CLI's stored
   credentials, zero CLI changes) while being the only part no standalone linter
   can replicate.
3. **Adversarial review** (six independent lenses) showed verdicts that gate
   nothing are prose: now the `fix` gate mechanically skips what the taxonomy says
   needs humans, and every failure fails properly (see *Trust properties*).

Autofix also moved up — but gated, dry-run by default, and only ever writing files
whose edited form reparses clean.

## Technical design

**Engine** (unchanged core, hardened): literal prefilter → tree-sitter parse →
per-language query → nesting-path check → resolution → finding. Resolution ties a
param to an operation four ways, each labeled in the finding (`via:` `direct`,
`var:` bag-bound-to-variable, `recv:` typed builder receiver, `type:` C#
target-typed `new()`). Post-review hardening: variable resolution is scoped to the
enclosing function (`funcKinds` per language); resolution tokens are computed per
`(param, operations)` entry, so nested-rule params can't resolve against the wrong
resource; `node_modules`/`vendor`/`dist`/`build`/`.git` are never scanned.

**Rules are data, now with two more fields.** A rule is
`(id, action, introduced_in, message, docs, [(param, [operations])], companion?)`.
`Action` is `remove` (fixable — span deletion + reparse proof) or `advise`
(detect-only: renames and value rewrites, primitives the fixer doesn't have yet —
`fix` refuses them with a docs pointer, by design). `IntroducedIn` is the API
version the change shipped in — the key to scaling (below). Operations lists are
derived from and verified against the vendored OpenAPI spec.

**Companion: remove forks into replace, by version.** A rule may declare a
companion parameter (dpm: `automatic_payment_methods[enabled]=true` on
PaymentIntents/SetupIntents). Before editing, `fix` consults the same
events-census the doctor uses: traffic all at/after the cutoff → plain removal
(the companion is default-on there); below it, mixed, or unknowable (no
credentials, no events, `--offline`) → each eligible removal span is *spliced*
with the language-correct companion instead — required below the cutoff
(verified live: a bare pre-cutoff PaymentIntent resolves to card-only),
harmless above it. Eligibility demands create evidence per SDK shape (the
parameter is create-only; update/confirm sites are bare-removed), is scoped
per call site (half-migrated files still get their remaining inserts), and
dedupes per builder instance in Java. The fix report's `.companion` carries
the decision, the account evidence, and the insert count.

**Doctor** fetches three account facts read-only (identity;
payment-method-configurations census including per-method names; API versions from
recent events — the *effective* truth, not the setting) and judges in a fixed
order: no-events → honest `REVIEW` (never a fabricated claim about traffic);
version cutoff → `BLOCKED`/`CAUTION`; hardcoded-methods-vs-Dashboard diff →
`CAUTION` naming the missing methods (the migration doc's loudest silent-breakage
warning); then intent (`CANDIDATE`/`SKIP`/`REVIEW`). Account-level verdicts print
once as a headline, not N times. Two credential-free code signals ship in every
report: `.webhook_handlers` (does *your* code mention the three
delayed-notification events) and `.frontend_warnings` (legacy Card Element
signals — Dashboard-managed methods can't render there). `--live` merges a real
webhook round-trip (listen + trigger, HMAC-verified) into the report as
`.live_drill` — explicitly a *toolchain* proof; the handler signal covers the
user's code.

**Fix** re-locates each finding on a fresh parse, computes the removal span (pair
+ separator / Java chain-link / whole statement), applies in memory, reparses, and
writes only if clean. The gate skips `dynamic` and `deliberate` findings into
`.skipped` with reasons (`--all` overrides). Per-file write errors are recorded
and don't abort the rest — a half-applied tree still yields a complete report.

**Trust properties** (all empirically verified, most born from the adversarial
review): nonexistent paths and typo'd topics exit 2 with did-you-mean hints —
never a green "no findings"; the JSON field names match the guide (`Finding` is
tagged); `STRIPE_API_KEY` must be a test-mode key; confirmations fail fast on
non-TTY stdin instead of hanging CI; demo's ephemeral config never touches the
account's Default configuration and auto-deactivates.

## Scaling: packs and the breaking-changes changelog

The registry claim is now demonstrated, not asserted: four packs were authored by
parallel agents from changelog + spec + SDK-history research (29 fixtures, per-file
counts asserted in `TestPacks`), with zero verb changes. Notably the research
*corrected* two prompt assumptions (prorate shipped 2020-08-27; `subscription_items`
never took `tax_percent`) — grounding works.

A survey of 82 breaking changes from the changelog maps the ceiling:

- **~20% request-side mechanical** (remove/rename/add/value): expressible as rule
  data; rename/value/add need three bounded span primitives; rules are
  *generatable* by diffing consecutive spec versions (stripe/openapi git history).
- **~38% response-side**: attribute-read detection — per-rule polarity inversion
  of the current precision doctrine; lower confidence, typed languages first.
- **~22% behavior changes**: no code signature; best served as a versionless
  advisory tier ("you are crossing 2023-08-16: these defaults flip") — near-zero
  engineering, high value.
- **The version explosion is linear, not quadratic**: the changelog is an ordered
  delta sequence; a user at version V needs exactly the rules with
  `introduced_in ∈ (V, target]`. Zero A→B→C rename chains in the survey. The real
  new cost is *start-point resolution per callsite* (version pins in code are
  trivially scannable; SDK-release→API-version tables from lockfiles; account
  events as fallback).

Out of architectural reach as autofix (and documented as such): flow redesigns
(Charges→PaymentIntents, Sources→PaymentMethods, legacy Checkout) — detectable
via a cheap deprecated-operation primitive, fixable only by humans/agents; and
client-side Stripe.js rewrites (no OpenAPI anchor).

## Honest coverage

Against everything the DPM migration docs actually require, the tool verifies the
param removal to a high standard and now *checks* three more things it previously
only mentioned (Dashboard method diff, user handler presence, Card Element
signals). Still absent: test-card matrices, per-method activation/terms steps,
currency/amount/capture-method interactions, Connect connected-account settings,
live-mode (all diagnosis is sandbox), and true verification of the user's webhook
*behavior* (signals are substring-level). Verdict language is now calibrated to
what each check actually measures.

## Known issues

- **Substring token collision** (found by a pack agent): `SubscriptionSchedule`
  contains `Subscription`, so schedule calls can spuriously match
  subscription-scoped rules. Fix: word-boundary token matching in `resolve.go`.
- Java cannot match dotted (nested) params (`pairKinds` nil — builder chains are
  flat); a silent false-negative class for nested rules in Java.
- Resolution is intra-file and function-scoped; response objects that travel
  across files are out of reach by design.
- Typed-language tokens are Create-biased (`XCreateParams`/`CreateOptions`);
  update-op param bags in TS/Java/C# under-anchor until tokens are generated
  per-operation from the spec.

## Evidence

| Claim | Evidence |
|---|---|
| Precision/recall | 44 findings across 9 stripe-samples repos, 0 false positives, 0 known misses; 2 negative-control repos correctly zero |
| Fix safety | all 44 removal spans reparse clean incl. multi-edit files; gate + reparse + per-file errors verified |
| Agent loop | fresh agent following `stripe guide` literally: doctor 1 → fix --apply → doctor 0, pure-JSON stdout throughout |
| Live behavior | `doctor --live` on a real test account: version-BLOCKED findings *and* `.live_drill.verified: true` in one report |
| Account layer flips advice | real account on 2022-11-15 traffic: every removal correctly BLOCKED — naive advice would have silently dropped methods |
| Multi-pack | 4 researched packs, 29 fixtures, `TestPacks` green; `fix` refuses advise packs |
| Speed / size | ~255ms per 2,020 files; +25.3MB stripped (runtime-dominated; malivvan wasm path remains the fetch-on-demand alternative) |

## Open questions for productization (unchanged in spirit, renumbered)

1. **Real third-party repo eval** — samples are canonical-style; wrapper layers
   are the resolver's known weak spot. (~1 day, highest information per dollar.)
2. **Distribution**: in-tree vs plugin against a binary-size budget; entangled
   with the embedded-vs-wasm runtime choice.
3. **Rule-content ownership**: who owns pack accuracy on docs timelines; the
   verification bar per pack; backfill volume argues for a shallow version window
   first (acacia-forward).
4. **Maintenance staffing**: 7 grammars, 7 SDK conventions, an immature runtime.
5. **New since the packs experiment**: build the three span primitives
   (rename/value/add), the deprecated-operation rule shape, per-callsite version
   resolution, and spec-diff rule generation.

## Try it

```bash
cd spikes/dpm-lint && CGO_ENABLED=0 go build -o ../../bin/stripe-demo .
../../bin/stripe-demo demo dpm --dir testdata     # guided walkthrough (humans)
../../bin/stripe-demo guide                       # agent playbook
../../bin/stripe-demo doctor dpm testdata          # diagnose against your test account
../../bin/stripe-demo doctor dpm testdata --live   # + webhook round-trip proof
../../bin/stripe-demo doctor collection-method testdata-packs/collection-method --offline
../../bin/stripe-demo fix dpm testdata             # gated dry-run; --apply to write
CGO_ENABLED=0 go test ./...                        # full suite incl. TestPacks
```

A 70-second asciinema recording of the full flow exists (`dpm-demo.cast` /
`dpm-demo.gif`, in ~/Downloads and the session scratchpad).
