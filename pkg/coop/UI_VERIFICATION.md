# UI Verification in Co-op

Status: prototype on branch `tomer/coop-ui-verifiers`. Design + implementation of
machine-checkable outcomes for `uiComponent` nodes: the human reviewer is the
browser; Stripe-side observable consequences are the machine check.

## 1. Problem

The 2026-07-19 end-to-end baseline eval (real `coop start` TUI + Codex against
real apps, scored on a live-API backend tier and a Dockerized-Playwright browser
tier) scored mean 27.7/100 with **0/100 on the browser tier in all four cases**.
The decisive finding was a verification gap in the coop loop itself: every
relevant blueprint has exactly one `uiComponent` node; the models marked each
one done with confident claims; the reviewer confirmed them, because node
verifications are honor-system (`Verification{Check, Passed}` is whatever the
agent asserts) and the only UI check is the boilerplate review prompt "Open the
app and confirm the user-facing flow works as described."

Failure anatomy per case:

| case | agent claim (self-reported `passed: true`) | ground truth (browser tier) |
|---|---|---|
| one-time-payment | "redirects the browser to the returned checkout_url" | Redirect DID reach checkout.stripe.com, but the app's response carried no recognizable Session identifier → no correlation possible |
| payment-element | "npm run build passed; /checkout statically compiles with PaymentElement" | Element never mounted (only Stripe.js logger iframes); old mock payment buttons still on screen |
| invoice | "checkout UI exposes Pay by invoice and redirects to Stripe's hosted page" | Button present; click never navigated; created invoice was USD $20 w/ random email vs the expected order |
| flat-subscription | "billing boundary compiles and is reviewable" | Upgrade endpoint 404'd; the gating selector exists nowhere in the app |

The missing artifact in every case: **a checkable object identifier recorded at
report time, plus an independent observation of that object reaching its
terminal state.** That is exactly what this mechanism adds.

## 2. Blueprint survey (the taxonomy that drives the design)

Corpus: 6 embedded blueprints (all hosted-redirect) + 27 upstream blueprints
(35 uiComponent nodes). The embedded set is NOT a strict subset upstream:
5 filenames match with divergent content; embedded `metered-subscription`
corresponds to upstream `metered-subscription-with-entitlements`.

Modality distribution across the 41 uiComponent nodes:

| modality | count (upstream set) | examples | Stripe-side observable | binding id | completion event exists? |
|---|---|---|---|---|---|
| Hosted redirect | 26 | Checkout (payment/subscription/setup/managed/destination-charge), hosted invoice page, Connect account links, billing portal | cs_→`status=complete && payment_status∈{paid,no_payment_required}`; in_→`paid`; acct_→capability active | cs_/in_/acct_ | mostly yes (`checkout.session.completed`, `invoice.paid`); account links: v2 events only; **billing portal: none exists at all** |
| Embedded component | 4 | Payment Element, embedded Connect onboarding, embedded checkout scaffold, FC-backed ACH Elements | pi_→`succeeded` (`processing` for bank debits); acct_ capability | pi_/seti_/acct_ | yes for pi_/seti_; often not wired in-blueprint |
| Modal / handoff | 1 | Financial Connections data session | fcsess_→`accounts.data` non-empty (GET; blueprint itself polls) | fcsess_/fca_ | exists but unused — state-poll is the natural check |
| Dashboard-adjacent | 4 | Issuing product activation, sandbox funding | none (account-level settings / dashboard tools) | none | no |

Key survey facts that shaped the design:
- ~22/35 upstream nodes have a directly wired completion event; ~13 have none
  in-file even where the API emits one.
- `/v1/billing_portal/sessions` has **no GET** and portal visits emit **no
  event** — a graceful non-machine tier is mandatory, not optional.
- Billing v2 blueprints (`flat-fee-and-overages`, `pay-as-you-go`,
  `usage-based-billing`, `credit-burndown`) wire only
  `v2.billing.pricing_plan_subscription.servicing_activated`; there is no GET
  for that object in the bundled API specs, and its id does not exist until
  after checkout completes. The Checkout Session is the only bindable object.
- No blueprint schema surface exists for outcome binding; dependencies are
  implicit via `${node.<step>.<key>:<field>}` template references.
- ~24/35 nodes use spectator/demo phrasing ("click the link", literal test-card
  digits) rather than integration-work phrasing.

## 3. Mechanism

### The contract

1. **Bind** — at `report-work` for a machine-verifiable journey, the agent must
   pass `--outcome <role>=<object id>` (e.g. `checkout_session=cs_...`) and
   should pass `--journey-url` (the app's URL the developer opens). Missing
   binding fails closed with a self-healing corrective `Hint`; role, id prefix,
   and charset are validated. Re-reports while in review rebind idempotently.
2. **Watch** — while the node sits in review, an observer inside the TUI
   process polls `GET <object>` with the profile's test-mode key (3s cadence
   for 30 minutes after report, then 10s, idling at 30s) and persists status
   transitions into the session file via compare-and-set updates. Pending →
   observed renders live in the review card.
3. **Gate** — `ConfirmReview` refuses to move a `uiComponent` node to done
   while its outcome is `pending` or `failed` (typed error, rendered as a
   status line, never a fatal view). A bounded one-shot re-check runs at
   confirm time to close the poll-interval race. Both auto-confirm bypasses
   are closed; `RequestChanges` clears the binding; Skip still works but a
   skipped gated node is recorded as never verified.

### Outcome lifecycle

| status | meaning | gate behavior |
|---|---|---|
| `pending` | binding recorded; outcome not yet observed | **blocks** confirm |
| `observed` | derived predicate held on the bound object (evidence recorded) | unlocks |
| `failed` | deterministic contradiction: 404 (wrong id / wrong account), cs `expired`, pi `canceled`, invoice `void`, invoice "paid" with `amount_paid=0` | **blocks**; `r` redo is the path out |
| `unavailable` | check could not run: no test key, live key, 401/403, sustained transport failure, unreadable v2 object | fails **open**: confirm/`a` converts to `attested` with the reason preserved |
| `attested` | human vouched without machine evidence (tier-3 journeys, unavailable checks, legacy nodes) | recorded distinctly — visible honor system instead of silent |

### Derivation (no per-blueprint tables, no schema changes)

`uicheck.DeriveExpectation` reads the **embedded blueprint** (falling back to
the session's node copies only when the blueprint id doesn't resolve — guided
actions — marked `Degraded`): an agent editing its session file cannot loosen
its own gate. The rule, validated against all 33 blueprints:

> Bind the object created by the **nearest preceding** apiRequest (same step
> first, then earlier steps) whose POST path is a journey-outcome creation
> path. asyncHandler events after the node only refine predicates or declare
> gaps; they never choose the binding (subscription and v2 ids do not exist
> yet at report time).

Creation paths are deliberately a subset of resource-verification's: only
journey-settled objects (checkout sessions, invoices, payment/setup intents,
FC sessions, accounts, billing-portal sessions as the tier-3 marker) — not
supporting objects — so the backward scan lands correctly (e.g. invoice-
payments binds `create-invoice`, skipping `add-invoice-item` and the `/send`
action).

Per-modality predicates (canonical Stripe semantics):

| role | GET | observed when | failed when | notes |
|---|---|---|---|---|
| checkout_session | /v1/checkout/sessions/{id} | `complete` && payment_status ∈ {paid, no_payment_required} | `expired` | no_payment_required covers setup mode and $0 trial/metered settles; evidence records pi_/sub_/seti_ ids |
| invoice | /v1/invoices/{id} | `paid` && `amount_paid>0` && payment_intent≠null | void/uncollectible, or "paid" with no real payment | the amount/PI clause kills `paid_out_of_band`/`forgive` fakes |
| payment_intent | /v1/payment_intents/{id} | `succeeded` (`processing` counts for bank-debit method families) | `canceled` | |
| setup_intent | /v1/setup_intents/{id} | `succeeded` | `canceled` | |
| fc_session | /v1/financial_connections/sessions/{id} | `accounts.data` non-empty | — (state-poll tier; no failure state) | strongest modality: no test helper can fake a linked account |
| connected_account | /v1/accounts/{id} | `charges_enabled` or any capability `active` | — | application-collected accounts are prefillable via API — a weaker proof, noted honestly |
| billing portal / dashboard / static-snippet / v2 core account | — | — | — | tier 3: explicit human attestation |

Billing-v2 journeys bind the sibling cs_ and record the v2 event as an explicit
unverified gap in `Detail`.

### Why this catches the four baseline failures

| baseline failure | what happens under the gate |
|---|---|
| response carried no Session identifier | the agent must report `--outcome checkout_session=cs_...` or report-work fails closed — the identifier IS the protocol now |
| Element never mounted | nobody can complete the PaymentIntent in a browser → `pi_` stays `requires_payment_method` → pending forever → confirm locked |
| invoice button never navigated | invoice never paid → pending; and an agent "fixing" it via `POST /v1/invoices/{id}/pay paid_out_of_band=true` produces `amount_paid=0` → **failed**, not observed |
| endpoint 404'd, selector nowhere | no journey can complete any bound object → pending → locked; rejecting clears the binding for the redo |

## 4. Honesty: what the gate cannot prove (fakeability by modality)

The threat model is the overconfident/lazy agent (the baseline failure mode),
which this fully catches: such agents never complete the journey at all. A
deliberately adversarial agent holding the account's secret key can still fake
some outcomes; the table below is the honest map, verified empirically in the
M0 spike (2026-07-20, test sandbox `acct_1Q5WiUDIKN6pQZY9`):

| modality | server-side fake | discriminator |
|---|---|---|
| checkout session | YES — `GET /v1/payment_pages/{cs}` → `POST /v1/payment_methods` (tok_visa) → `POST /v1/payment_pages/{cs}/confirm` completes a real cs_ with only the secret key (the recipe ships in this repo's own `checkout.session.completed` fixture). **Spike-verified: the completing event's `request.id` is null for BOTH the API recipe and a genuine browser payment** — event origin does NOT discriminate. | none at object/event level; raises effort (undocumented endpoint), documented as a limit. Future hardening: watch the request-log stream for `payment_pages/{bound-id}/confirm` during review (passive-verification worktree has the connector prior art) |
| invoice | YES — `/pay` with an attached PM produces a real charge | **works**: API `/pay` events carry `request.id=req_...` (spike-verified); `paid_out_of_band` additionally fails the `amount_paid>0` clause. Origin recording is future work; the object-level clause ships now |
| payment intent | YES — `POST /v1/payment_intents/{id}/confirm` with pm_card_visa | none — browser confirms also carry request ids. The TUI copy says "payment observed" and the human review prompt remains the check that it happened *inside the app* |
| FC session | NO — no test helper populates a session's accounts | the modal is genuinely required |
| account (Stripe-collected requirements) | NO — platform writes to requirement fields are refused | strong |
| account (application-collected) | YES — prefill test data via API | weak; noted in Summary |

Binding integrity limits (adversarial review findings, recorded):
- The agent could bind an object its app never serves; the human paying the
  bound object's own URL would bypass the app. Mitigations shipped: the agent
  is instructed to pass the **app's** URL as `--journey-url` (hosted URLs are
  only a display fallback captured from the object while pending); amount/mode
  cross-checks against the creating node's params are future work (port from
  resource-verification's `paramChecks`).
- Apps that mint a fresh cs_ per journey false-negative against a pinned id.
  Designed mitigation (deferred, below): hybrid observation — poll
  `GET /v1/events` for same-type completions since review start and surface
  "cs_Y completed just now — accept as this journey's outcome?" for
  one-keystroke human acceptance.
- The session file is owner-writable; derivation from the embedded blueprint
  and the confirm-time live re-check raise the bar, but an agent with the
  account key can always manufacture state. Full tamper-proofing is
  impossible; the goal is a strong evidence trail.

## 5. Schema and protocol changes

- `SessionNode.UIOutcome *UIOutcome{Role, Type, ObjectID, AccountID,
  JourneyURL, Expect, Status, Detail, Evidence(≤8 k/v), AttestedBy,
  ReportedAt, LastCheckedAt, ResolvedAt}` — additive, `omitempty`; session
  schema version stays 2 (resource-verification claims a different v3;
  reconcile at merge).
- `CommandResponse.UIOutcome *UIOutcomeSummary{Role, ObjectID, Expect,
  Status, JourneyURL}` — start-work advertises the role + expectation and a
  Next template with the required flags; report-work explains the pending
  outcome and warns: do NOT wait for the webhook, do NOT poll, do NOT
  `stripe trigger` (fixture events mint a NEW object — spoof-resistant by
  construction).
- Agent flags: `report-work --outcome <role>=<id> --journey-url <url>`.
  `--stripe-resource` was deliberately NOT reused (the resource-verification
  branch gates object creation at report time; this gates journey outcomes at
  confirm time; a merge adapter is trivial, a shared flag with divergent
  semantics is not).
- Workflow: `ErrUIOutcomeNotObserved` (typed), `ConfirmReviewContext` (one-shot
  re-check via `WithUIVerifier`), `AttestOutcome`, auto-confirm bypasses closed
  in ReportWork and AwaitReview, RequestChanges clears the outcome, await
  timeout messaging carries journey context.
- Observation: `uicheck` package — pure derivation; bounded `StripeReader`
  (1MiB cap, pinned Stripe-Version + drift test, preview header for /v2/,
  live-key refusal, key redaction, memoized /v1/account identity); `Checker`
  (one-shot + Watch pass); `ApplyObservation` compare-and-set persistence
  (transition-only writes — no session Version churn; never regresses a
  resolved outcome; tolerates the binding vanishing mid-poll).
- TUI: observer scheduled on its own message chain (the 500ms session tick
  pauses while the terminal is unfocused — which is precisely when the human
  is in the browser); model-held workflow options; blocked confirm via the
  status line (the old path put gate errors into `m.err`, which replaces the
  whole view and is never cleared — a one-line bricking bug found in
  adversarial review).

## 6. TUI design (as shipped)

Review card rows (between the confirmation checks and the agent metadata, so
height truncation keeps the gate explanation):

```
│ Confirmation steps                                             │
│ Open the app and confirm the user-facing flow works…           │
│ Your turn: complete the journey in your browser                │  ← Attention
│ Open: http://localhost:3000/checkout  o open                   │  ← Muted + key hint
│ ⠋ Watching cs_test_a1B2… for status=complete and payment_st…   │  ← Muted + spinner
│                                                                │
│ Includes: Create a Checkout Session, Complete the checkout     │
│ Agent changed: server/checkout.js                              │
│ Agent verified: 2 check(s) passed                              │
```

Per status: observed → `✓ Observed: checkout.session cs_… status=complete ·
payment_status=paid · 12:04`; failed → `✗ Outcome: … — redo the step` + `r`
hint; unavailable → dimmed reason + `Attest: press a — "I completed this
journey myself"`; tier-3 → `No API-observable outcome for this step.` +
attest hint; attested → `✓ Attested by you · 12:04`.

Interactions: `c` while gated → status line "Confirm is locked: complete the
journey in your browser (watching cs_… for …) — press o to open it" and help
relabels `c confirm (locked)`; footer becomes "Waiting for you: complete the
journey to unlock review". `a` = attest (refused for observable-tier nodes
with a healthy check). `o` contextually opens the pending journey URL, else
the sandbox claim URL. Reject flows unchanged. **No override key**: `failed`
should hurt; `unavailable` already fails open; RequestChanges/Skip are the
escape hatches (alternative — a heavy, recorded override — documented for a
future decision).

## 7. Headless validation (CI: no network, no browser)

The debug agent grew hidden flags:
- `--simulate-outcome=bind` (default) — synthetic bindings (`cs_debug_000004`)
  on gated nodes via the same report-work path real agents use.
- `--simulate-observe=after=<dur>|fail|never|unavailable` (default
  `after=2s`, so plain `--debug-agent` runs exercise the gate and still
  complete) — the debug agent plays the observer, persisting scripted
  observations through the same compare-and-set path, so the TUI renders
  production behavior end-to-end.
- `--live` — really executes apiRequest nodes with the test-mode key
  (blueprint `${node…}` refs resolve against live responses), binds real ids,
  and for payment intents serves a minimal localhost page that mounts a real
  Payment Element (profile publishable key).

Tests (all green, `-race` clean):
- `pkg/coop/uicheck`: derivation golden table over every embedded blueprint
  (fails loud on upstream sync drift) + 11 synthetic modality cases; reader
  classification/redaction/limits; observation compare-and-set.
- `pkg/coop/workflow`: report-work gate matrix (fail-closed, validation,
  auto-confirm closed, idempotent rebind, tier-3 ungated) + confirm gate
  matrix (pending/failed block with typed error; observed unlocks;
  unavailable→attested; tier-3 stamps attestation; AwaitReview bypass closed;
  RequestChanges clears; AttestOutcome refusals).
- `pkg/coop/tui`: rendering per status, blocked-confirm status path (no
  m.err), attest key, contextual open, observer scheduling with a fake
  observer and cadence table.
- `pkg/cmd/coop`: `TestCoopDebugAgentGatedOutcomeFlow` — the full loop
  (bind → human confirm BLOCKED while pending → simulated observation →
  confirm unlocks → session completes) and
  `TestCoopDebugAgentGatedOutcomeNeverObserved` — the negative path (never
  observed → repeatedly blocked over real time → reject clears the binding →
  redo re-binds fresh).
- `scripts/test-coop-debug-agent-tmux.sh`: gated positive + negative
  scenarios against the real TUI in tmux (plus the pre-existing ungated
  smokes, unchanged).

## 8. Manual validation evidence (exit criterion)

All three modalities were validated live on 2026-07-20 against the test
sandbox (`acct_1Q5WiUDIKN6pQZY9`): real `stripe coop start <bp> --debug-agent
--live` sessions in tmux, with a real browser completing (and, for the
negative paths, failing) each journey. Verbatim TUI transcript excerpts below.

### Hosted redirect — one-time-payment (session `coop_27486d38`)

Pending, with the real Checkout Session the live agent created and bound:

```
│ Your turn: complete the journey in your browser                           │
│ Open: https://checkout.stripe.com/c/pay/cs_test_a1dlIAsDZuwD8hMWPvS7JHRm… │
│ ⠼ Watching cs_test_a1dlIAsDZuwD8… for status=complete and payment_status  │
│ paid or no_payment_required (payment mode)                                │
  c confirm (locked) · r changes · …
```

Negative interaction — `c` pressed before paying:

```
  Confirm is locked: complete the journey in your browser (watching cs_test_a1d…
  Waiting for you: complete the journey to unlock review
```

The session was then paid in a real browser (4242, checkout.stripe.com →
success redirect). Within one observer poll:

```
│ ✓ Observed: checkout.session · cs_test_a1dlIAsDZuwD8… · status=complete · │
│ payment_status=paid · payment_intent=pi_3TvNOYDIKN6pQZY90TII6iIP · 13:02  │
  c confirm all · r changes · …
```

`c` then confirmed through to the completion view. Session JSON records the
full trail: bound `12:58:34`, observed `13:02:38` (the genuine four-minute
human journey), status `observed`, evidence `{status: complete,
payment_status: paid, payment_intent: pi_3TvNOY…}`, node `done`.

### Embedded — accept-payment-with-payment-element (prototype blueprint)

The live agent created a real PaymentIntent, bound it, and served the
localhost Payment Element page:

```
│ Your turn: complete the journey in your browser                           │
│ Open: http://127.0.0.1:64985/  o open                                     │
│ ⠇ Watching pi_3TvNSbDIKN6pQZY90n… for status=succeeded                    │
  c confirm (locked) · …
```

A genuine Payment Element mounted in the browser (Stripe iframe card fields).
**Negative path**: paying with the declining card `4000…0002` produced "Your
card has been declined." in the browser — and the TUI stayed locked:

```
│ ⠸ Watching pi_3TvNSbDIKN6pQZY90n… for status=succeeded                    │
  c confirm (locked) · …
```

**Positive path**: re-paying with `4242…` ("Payment submitted"):

```
│ ✓ Observed: payment_intent · pi_3TvNSbDIKN6pQZY90n… · status=succeeded ·  │
│ latest_charge=ch_3TvNSbDIKN6pQZY90aX2THzl · 13:12                         │
  c confirm all · …
```

Session JSON: node `done`, `observed`, evidence `{status: succeeded,
latest_charge: ch_3TvNSb…}`, `journey_url: http://127.0.0.1:64985/`.

### No-observable — billing-portal (prototype blueprint)

The tier-3 card rendered with the explicit attestation affordance:

```
│ No API-observable outcome for this step.                                  │
│ Attest: press a — "I completed this journey myself"                       │
  a attest · c confirm all · …
```

Pressing `a`: `Attestation recorded. Press c to confirm.` →
`✓ Attested by you · 13:04`. After `c`, the session JSON records
`status: attested, attested_by: human-review, detail: "no machine-checkable
Stripe outcome for this journey; attested by the developer"` — the honor
system made visible and scoreable instead of silent.

### Headless negative coverage (CI-repeatable)

The same negative guarantees hold with zero network in
`TestCoopDebugAgentGatedOutcomeNeverObserved` (confirm repeatedly blocked
over real time; reject clears the binding; the redone node re-binds fresh)
and in the tmux script's `gated-negative` scenario (locked across repeated
confirms; `r` recovers; session never completes). Full tmux suite: 33/33.

## 9. Upstream blueprint-authoring guidance

For a uiComponent node to be machine-verifiable, a blueprint needs only what
good blueprints already have — no schema changes:
1. A **preceding apiRequest node that creates the journey object** (checkout
   session, invoice, payment intent…). The uiComponent's binding derives from
   it. Don't hide the creation inside prose or expect the app to mint it
   unseen; the agent must be able to report the id it wired into the app.
2. **Events refine, never bind**: wire the asyncHandler with the canonical
   completion event when one exists; derivation uses it to phrase the
   expectation and records v2-only events as explicit gaps.
3. **Write uiComponent descriptions as integration work**, not spectator
   instructions. "Add a button that redirects the customer to the Checkout
   page" derives a verifiable journey; "Open the URL below and optionally pay
   with 4242…" is demo voice (24 of 35 upstream nodes today) and tells the
   agent the human is the actor — the exact honor-system framing that failed.
4. **Journeys with no observable outcome** (portal visits, dashboard steps)
   are fine — they classify as attestation tier automatically. Prefer pairing
   them with a follow-up apiRequest that reads the side effect when you want
   more than attestation.
5. Optional future field: an explicit `outcome` hint per node was prototyped
   behind derivation (Go-side defaults remain the source of truth). Adopt in
   the upstream schema only if derivation proves insufficient.

## 10. Eval-integration handoff (deliberate later phase, eval workstream owns)

What the eval harness will need:
1. Its deterministic reviewer must play the human for gated nodes: read
   `ui_outcome` from the session file, complete the journey (its browser tier
   already pays hosted checkouts), wait for `observed`, then confirm. Attested
   and skipped-gated nodes are distinct, scoreable outcomes now.
2. A fresh candidate binary from this branch; new known-good controls
   qualified for the gate (the old controls never bound outcomes).
3. An A/B baseline — same model, vanilla vs gated candidate — with the
   browser tier scoring whether UI verification moves real outcomes.
4. If the harness completes checkouts via the `payment_pages` API recipe
   rather than a real browser, note that object-level state cannot tell the
   difference (spike-verified) — no eval-only seam is required for cs_; if
   invoice-origin hardening is later enforced, an eval-only injection seam
   (the `WithUIVerifier`/`WithOutcomeObserver` options) is the right hook.

## 11. Deferred work / open questions

- **Hybrid candidate discovery** (`/v1/events`-based "a different cs_ just
  completed — accept it?") — designed (see §4), not prototyped. The realistic
  fresh-object-per-journey integration needs it before broad rollout.
- **Invoice origin evidence** (record the completing event's `request.id`,
  warn on API origin) — predicate hardening shipped; origin recording is a
  small follow-up now that the spike proved the signal.
- Amount/mode cross-checks binding↔creating-node params (port
  resource-verification's `paramChecks`).
- Cross-account bindings (customer-payment-method-sharing) — the observer's
  404 detail names key mismatch as a likely cause; proper account-id capture
  at report time needs an agent-side /v1/account call (deliberately avoided:
  no network in the report path).
- Request-log watch for `payment_pages/{bound-id}/confirm` (cs_ fake
  detection) — connector prior art exists in the passive-verification branch.
- Pending-timeout auto-degrade (no: Skip/RequestChanges are the escape
  hatches — revisit with usage data). Schema v3 reconciliation at merge.
