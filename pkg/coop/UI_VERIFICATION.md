# UI Verification in Co-op

Status: prototype on branch `tomer/coop-ui-verifiers`. Design + implementation of
machine-checkable outcomes for `uiComponent` nodes: the developer walks the
journey **starting in their own app**, and the CLI verifies that the walk
produced the right Stripe-side consequence — instead of trusting the agent's
claim that a working UI exists.

**What this proves, precisely.** Starting from a page the app actually serves,
a real traversal produced a settled Stripe object. It does not prove which
pixels rendered; only a browser driver could, and that stays the eval
harness's job. The teeth come from the object *not existing until the app
creates it*: a cart page whose button never navigates, or whose endpoint 404s,
cannot produce one at all.

> Design history worth keeping. The first version had the agent pre-create the
> Stripe object and hand the developer its hosted Stripe URL. That verified a
> payment settled but let the app be skipped entirely — the developer could pay
> a Checkout Session the app never served, and the gate went green. Section 3
> describes the fix (app entry point + object discovery); section 4 records
> what remains unproven.

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

Underneath the honor-system mechanics is an authoring problem, not just a
protocol gap. The blueprint survey (§2, §9) found most upstream `uiComponent`
nodes are written as demo/walkthrough steps for a hosted product tour — "click
the link below", "optionally fill out the form with 4242…" — text aimed at a
human reading the blueprint and clicking through a Stripe-hosted page.
Verification was retrofitted onto that prose afterward, but the prose still
tells the agent a human is the actor. Read as written, an agent that stages a
link and builds nothing has done exactly what the node asked; the loop had no
way to tell a staged demo from a wired integration, because that distinction
was never in the node's contract to begin with.

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

1. **Point at the app** — at `report-work` for a machine-verifiable journey the
   agent must pass `--journey-url`: the page **in the developer's own app**
   where the flow starts (the cart page, the checkout page). It is rejected if
   it is a Stripe-hosted host (`checkout.stripe.com`, `billing.stripe.com`, …)
   or if nothing answers it — a bounded reachability GET runs before the store
   update, so an agent cannot name a page for an app it never ran. Failing
   either way fails closed with a corrective `Hint`.
   `--outcome <role>=<id>` is now required only for journeys that act on an
   object created *earlier* (Financial Connections sessions, Connect accounts);
   for app-minted journeys the object cannot be named yet, because it does not
   exist until the developer walks the app.
2. **Watch, and discover** — while the node sits in review, an observer inside
   the TUI process polls with the profile's test-mode key (3s cadence for 30
   minutes after report, then 10s, idling at 30s) and persists status
   transitions via compare-and-set updates. For app-minted journeys it *lists*
   objects of the expected type created since the review opened and takes the
   first that satisfies the win condition — that object is the evidence, since
   only the app's own flow could have created it. Pre-bound journeys keep the
   direct `GET <object>` path. Pending → observed renders live in the card,
   which names the discovered object so confirming it is an informed act.
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

Binding integrity — what the app-entry model fixed, and what is left:
- **FIXED — the app could be skipped entirely.** Previously the agent bound an
  object it had created and the developer paid that object's hosted URL, so a
  green gate said nothing about the app. Now the journey URL must be an
  app-served, non-Stripe page (reachability-probed), and for app-minted
  journeys the verified object is *discovered* from what the walk produced.
  Validated live: the discovered session (`cs_test_a1vEkTu…`) was a different
  object from the one the agent had created by API (`cs_test_a1TLXgA…`) — only
  the app's own Buy flow could mint it. This also removed the pinned-id false
  negative, since a fresh object per journey is now the expected case.
- **REMAINS — fabrication inside the review window.** A determined agent could
  create and complete its own object of the right type while the node is in
  review, and discovery would find it. This is much narrower than before (the
  object must post-date the review opening, and the card names it so an
  unexpected object is visible before you confirm), but it is not closed. The
  honest framing stays: necessary, not sufficient.
- **REMAINS — the session file is owner-writable.** Deriving expectations from
  the embedded blueprint and re-checking live at confirm time raise the bar,
  but an agent holding the account key can manufacture state. Full
  tamper-proofing is impossible; the goal is a strong evidence trail.
- Amount/mode cross-checks against the creating node's params are still future
  work (port from resource-verification's `paramChecks`).

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

### App-first hosted checkout — one-time-payment (2026-07-20, the current model)

Run with `--debug-agent --live`, which now serves a real local storefront (a
cart page whose Buy button creates the Checkout Session server-side and
redirects). The review card points at the **app**, not Stripe:

```
│ Your turn: walk this flow in your app                                     │
│ Start here: http://127.0.0.1:53766/  o open                               │
│ ⠋ Watching for a checkout.session from your app: status=complete and      │
│ payment_status paid or no_payment_required (payment mode)                 │
  c confirm (locked) · r changes · …
```

Pressing `c` before walking it: `Confirm is locked: walk this flow in your app
— waiting for your app to produce a checkout.session`.

Then the genuine traversal: cart page → **Buy now** → the app created its own
Session and redirected to Stripe Checkout → paid with 4242 → back to the
**app's** success page. Discovery found it:

```
│ ✓ Observed: checkout.session · cs_test_a1vEkTu6oyGiO… · status=complete · │
│ payment_status=paid · payment_intent=pi_3TvVIyDIKN6pQZY90cQXvsBv · 21:29  │
│ Your app created this during review — confirm only if it was your         │
│ journey.                                                                  │
```

The decisive evidence is the id comparison in the session file:

| | id |
|---|---|
| created by the agent's own API call (blueprint node) | `cs_test_a1TLXgAh20GQ5Bzv…` |
| **discovered and verified** (minted by the app's Buy flow) | `cs_test_a1vEkTu6oyGiOGFg…` |

`ui_outcome` records `discovered: true`, `journey_url:
http://127.0.0.1:53766/`, `detail: "checkout completed (created by your app
during this review)"`. Under the previous model the gate would have watched
the agent's session and passed without the app doing anything.

### (Superseded) Pre-bound hosted redirect — one-time-payment (session `coop_27486d38`)

Kept as the record of the earlier model, and of why it was replaced: this run
went green while the developer paid a Stripe URL directly, with no app in the
loop.

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

**What a uiComponent node means.** A uiComponent node means: wire this flow
end-to-end into the developer's app. The app needs a real surface — a page, a
button, a mounted component — that a person can start from, and walking that
surface has to be what drives the Stripe object to its terminal state. A node
is not satisfied by an agent that stages a link for a human to click; it is
satisfied by an agent that builds the thing the human clicks through.

For a uiComponent node to be machine-verifiable, a blueprint needs only what
good blueprints already have — no schema changes:
1. A **preceding apiRequest node that creates the journey object** (checkout
   session, invoice, payment intent…). The uiComponent's binding derives from
   it. Don't hide the creation inside prose or expect the app to mint it
   unseen; the agent must be able to report the id it wired into the app.
2. **Events refine, never bind**: wire the asyncHandler with the canonical
   completion event when one exists; derivation uses it to phrase the
   expectation and records v2-only events as explicit gaps.
3. **Write uiComponent descriptions in integration voice, not spectator
   voice** — see below; this is the single most common defect in the corpus
   (~24/35 nodes today).
4. **Journeys with no observable outcome** (portal visits, dashboard steps)
   are fine — they classify as attestation tier automatically. Prefer pairing
   them with a follow-up apiRequest that reads the side effect when you want
   more than attestation.
5. Optional future field: an explicit `outcome` hint per node was prototyped
   behind derivation (Go-side defaults remain the source of truth). Adopt in
   the upstream schema only if derivation proves insufficient.

### Voice: spectator vs. integration

Spectator voice describes what a human does by hand: click here, open this
link, optionally type 4242 4242 4242 4242. Integration voice describes what
the app must do: create the object, mount the component, wire the redirect.
The difference isn't tone — it's who the text casts as the actor. Spectator
voice casts a human as the actor and the app as scenery it walks past;
integration voice casts the app as the actor, with the human exercising it
afterward. Three real examples, verbatim:

| blueprint | before (verbatim) | after (integration voice) |
|---|---|---|
| `billing-portal.json` | "Click the link below to open the billing portal in a new tab." | "Add a button in your app that requests a Billing Portal session from your backend and redirects the customer to it." |
| `invoice-payments.json` | "Preview the hosted invoice page where customers can view and pay their invoice online. Optionally fill out the form with the card number as 4242 4242 4242 4242. Use a valid future date, such as 12/34, and any three-digit CVC." | "Add a 'Pay invoice' action to your billing UI that sends the customer to the invoice's hosted_invoice_url — wired from a real invoice your app created, not a preview link." |
| `learn-accounts-v1-marketplace.json` | "The connected account's customer can then complete a payment on the checkout surface. Click on the link below and fill in the test card information with the card number as 4242 4242 4242 4242 to run a successful payment." | "Add a checkout surface in your marketplace app where a shopper pays the connected account directly — the page itself creates the Checkout Session and redirects." |

For contrast, `accept-payment-with-payment-element.json` is already close to
integration voice: "On your client, initialize Stripe.js with the
client_secret from the PaymentIntent, mount the PaymentElement, and call
stripe.confirmPayment() to complete the payment." Every verb there belongs to
the app, not the human.

### Per-modality acceptance criteria

"The app must actually have X" means something different per modality. This
grounds it in the taxonomy from §2/§3 — no new modalities invented:

| modality | what the app must have | what the developer does, starting from the app URL |
|---|---|---|
| Hosted redirect (Checkout, hosted invoice, account links, billing portal) | A real page/control that server-side creates the Stripe object and redirects to it — not a bare Stripe URL pasted into the node text | Start on the app page, click the app's own control, land on Stripe's hosted flow, complete it, return to an app page |
| Embedded component (Payment Element, embedded Connect onboarding, embedded checkout, FC-backed ACH) | A page that mounts the real Stripe.js component against a live client secret from a real PaymentIntent/SetupIntent/Account Session — a passing build is not evidence | Start on the app page, watch the component render inline, submit it without leaving the app |
| Modal / handoff (Financial Connections) | A real trigger wired to a live session client secret, plus handling for the return | Start on the app page, launch the modal, connect a (test) institution, watch the app resume with the linked data |
| Dashboard-or-no-observable (Issuing activation, sandbox funding, billing portal's actual outcome) | A real entry point for the described action, even though no Stripe API can confirm the result | Start on the app page, perform the action, then attest it happened — no observable outcome doesn't waive the app-entry requirement |

### The protocol already enforces the app-entry half of this

Independent of what a node's prose says, `report-work` on a gated uiComponent
node requires `--journey-url` pointing at a page the developer's own app
serves (§3): Stripe-hosted hosts are rejected, reachability is probed, and for
app-minted journeys the verified object is discovered from what the walk
creates, not from what the agent names. A blueprint whose text implies "the
human just clicks a Stripe link" gets no easier ride from this — the agent
still has to produce an app page and pass the reachability probe, just while
working against prose that told it a human already did the work. That
mismatch is friction, not a bypass, and it's the intended pressure: it's also
the reason to fix the prose upstream. Doing so doesn't change what the CLI
enforces; it stops making the agent fight the blueprint to comply with it.

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

- ~~Hybrid candidate discovery~~ — **shipped** as the app-entry + discovery
  model (§3): the fresh-object-per-journey case is now the primary path, not
  an edge case.
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

## 12. Spike: can we tell a browser journey from an API-driven fake? (2026-07-20)

Motivation: §4 records that object state alone cannot prove the developer's
browser did the work — an agent holding the account key can complete a
Checkout Session server-side with the recipe that ships in this repo
(`pkg/fixtures/triggers/checkout.session.completed.json`). This spike tested
whether the request-log stream closes that hole. Method: one session
completed via the API recipe and one paid in a real browser, both observed on
a live `stripe logs tail` stream, then each request enriched via
`GET /v1/request_logs/{request_id}`.

**Refuted — path presence proves nothing.** Both completions appear in the
merchant's request log as `POST /v1/payment_pages/:id/confirm`. The hosted
page's own confirm is a normal logged API request. (This is the second
plausible discriminator to fail empirically; §4 already records that
`event.request.id` is null for both.) The streamed path is also
**id-redacted to `:id`**, so the stream alone cannot say *which* session a
confirm belongs to.

**Confirmed — the credential discriminates, and the enrichment carries ids.**
`GET /v1/request_logs/{request_id}` returns both the calling credential and
the concrete objects touched:

| | API recipe (fake) | Browser (real journey) |
|---|---|---|
| `request.headers.User-Agent` | `curl/8.7.1` | `Mozilla/5.0 … Chrome …` |
| `request.key.redacted_secret` | `sk_test_…` | `pk_test_…` |
| `objects[]` | `[cs_test_a1fxz…]` | `[pi_3TvW4I…, cs_test_a1eOU…]` |

The hosted Checkout page confirms with the **publishable** key — a browser
has nothing else. The recipe must use the **secret** key. So key type is a
semantic, not heuristic, signal, and it yields *positive* evidence ("this
settled from a browser") rather than only fake-detection. `objects[]` restores
the exact correlation the redacted path destroys.

**Operational constraint discovered.** The request-log websocket rejects
AI-agent user agents outright:
`websocket: bad handshake — Invalid user agent: Stripe/v1 stripe-cli/master AIAgent/claude_code`.
The CLI derives that suffix from environment detection (`CLAUDECODE`,
`CODEX_*`, `CURSOR_AGENT`, … in `pkg/useragent`), so a TUI launched from the
developer's own shell is unaffected, but a co-op session started from inside
an agent session would be refused. `stripe listen` accepts the same UA;
only `request-log-payloads` refuses it. Any design leaning on this stream
must degrade to today's object-state verification rather than fail the node.
Note also that request-log tail sessions are capped per account, so the
observer competes with a developer's own `stripe logs tail`.

**Built, and validated live.** The shape above now ships: the TUI starts a
request-log stream, buffers entries, and on a settled journey enriches the
matching `SettlePaths` request, correlates through `objects[]`, and
classifies by `request.key` prefix. A live app-first run (cart page → Buy →
Stripe Checkout → paid with 4242) recorded:

```
✓ Observed: checkout.session · cs_test_a1UpUpDhDx0dx… · status=complete ·
  payment_status=paid · payment_intent=pi_3TvWMUDIKN6pQZY90Bfm0QVJ
  Your app created this during review — confirm only if it was your journey.
✓ Settled from a browser at http://127.0.0.1:56328/
```

with `settled_by: browser` and `settled_from: http://127.0.0.1:56328/`
persisted as node evidence. Note the recorded origin was the app's own URL
rather than `https://checkout.stripe.com/` as in the isolated spike above;
both are Stripe-reported values from `request.origin`, and the difference
between the two runs is not yet explained — do not build logic that assumes
which one appears. Only the credential (`pk_` vs `sk_`) is relied on to
classify; origin is displayed as corroboration.

The `api` branch is covered by unit tests rather than a live run: the
classifier is symmetric (the same code path differing only in the key
prefix), and staging a live server-side completion inside a review window
adds setup without exercising new code.

**Reporting it back to the agent.** The developer sees this on the review
card, but only the agent can close the gap and it never sees the card — so
`await-review`'s confirmation response carries the warning in prose and
`CommandResponse.UIOutcome.SettledBy` carries it structurally. A browser-
settled journey adds nothing; an unobserved origin adds nothing at all,
since the stream is optional and silence must never read as suspicion.
