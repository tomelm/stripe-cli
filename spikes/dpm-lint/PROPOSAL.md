# Proposal: `stripe lint` — rule-based scanning for Dynamic Payment Methods migration

**Status:** validated spike (this directory) · **Branch:** `tomer/dpm-lint` · **Date:** 2026-07-30

## Summary

Add a CLI command that scans a codebase in 7 languages (Ruby, Python, PHP, JS/TS, Go,
Java, C#) for Stripe API requests carrying parameters a migration wants changed —
starting with `payment_method_types`, per the [Dynamic Payment Methods
migration](https://docs.stripe.com/payments/payment-methods/dynamic-payment-methods).
Rules are language-agnostic data; all language knowledge lives in a fixed engine
written once. The spike here scores **100% precision and recall** on
`stripe-samples/accept-a-payment` (35/35 sites, zero false positives) at ~255ms per
2,000 files, `CGO_ENABLED=0`. v1 is inventory-only, never directive — see
*Matching is not advising*, the finding that shapes the whole plan.

## Evidence (all verified, reproducible from this directory)

| Claim | Evidence |
|---|---|
| Pure-Go tree-sitter under `CGO_ENABLED=0` | `gotreesitter` runtime, 8 grammars embedded; entire spike builds/tests with CGO off |
| One language-agnostic rule covers 7 languages | `dpmRule` in `main.go` is data; language knowledge is 8 `langSpec` entries in `langs.go` |
| Real-world code resolves | 4 mechanisms in `resolve.go`, all exercised on the sample: `direct` (22), `var:` bag bound to a variable (9), `recv:` typed builder receiver (2), `type:` C# target-typed `new()` (2) |
| Nesting paths enforced | `payment_settings.payment_method_types` cannot cross-match top-level usage in either direction (`pathSatisfied`); exactness is path-shape-level — binding resolution tokens per `(param, operation)` pair is a known, bounded follow-up |
| Precision under adversarial negatives | attribute reads, comments, strings, non-Stripe resources, client-side Elements options all rejected (`main_test.go`) |
| Fast | literal prefilter parses only files containing the param's SDK spelling: 255ms / 2,020 files, M1 Max (`BenchmarkScanSyntheticRepo`) |

Ground truth was an independent grep sweep: the linter found every hit that was a real
request-parameter usage and none that weren't. The Java builder spelling
(`addPaymentMethodType`) is confirmed against the real Java SDK in the sample.

## Decisions

**Tree-sitter, embedded runtime for the spike; production runtime is an open
decision.** ANTLR was the only real alternative (pure Go, mature) but its rules are
compiled visitor code — not shippable as data — and error tolerance on non-compiling
files is weaker. ast-grep/Semgrep are the right shape but unusable as a Go library.
Two runtimes were both proven working: `odvcencio/gotreesitter` (zero toolchain, all
grammars embedded, **+25.3MB stripped**, ~all of it runtime) and `malivvan/tree-sitter`
(real C tree-sitter as wasm on wazero; grammars are fetchable data blobs, C+C++ build
is 4MB, but adding grammars needs `zig cc` in CI). Decision criteria: binary-size
budget and the distribution decision below; measure a 7-grammar `ts.wasm` first.

**Rules are data; the spec is the source of truth.** A rule is
`(param, [operations], action, message)`. The operations list is derived from and
verified against the vendored `api/openapi-spec/spec3.cli.json` (generation becomes
`go generate` in phase 1): 12 POST operations take the param where the docs mention 3
resources — including `payment_links`, and nested under `payment_settings` on
subscriptions/invoices — while `report_runs`, which matches the string only as a
report column name, is structurally unreachable because the rule never lists it. SDK
spellings are SDK-wide conventions (snake_case / PascalCase / camelCase / Java
`add<Singular>`), not per-rule knowledge. LLMs may draft rule files at build time in
CI; drafts are verified against per-language fixtures before shipping; no LLM runs on
user machines.

**Resolution is intra-file name matching, not dataflow.** `params = {...}` passed
later to `create(**params)` resolves by whole-word name lookup against a per-file
index of Stripe call sites and typed declarations. Probed failure modes: no scope
awareness, no kill on reassignment, no flow order. All three have bounded fixes
(function-scoped index, position ordering, nearest-preceding binding) needing no CFG —
scheduled for phase 1. Cross-file resolution is out of scope permanently unless field
data demands it.

## Matching is not advising

On `accept-a-payment`, all 35 findings sit in `custom-payment-flow` — a sample whose
*purpose* is per-method integration (`oxxo.php` sets `['oxxo']`; the Python/Node
servers set the param from a request field). The DPM-style samples produce zero
findings: `payment-element` already uses `automatic_payment_methods`, and
`prebuilt-checkout-page` simply omits the param (Checkout is Dashboard-managed by
default). "Remove `payment_method_types`" would have broken all 35.

Whether removal is correct depends on facts outside the code: Dashboard configuration,
whether the effective API version is ≥ `2023-08-16` (below it, removal without
`automatic_payment_methods[enabled]=true` silently *drops* payment methods), and
Payment Element vs Card Element on the frontend. A code-only scanner inventories and
ranks; it must not direct. Hence v1:

1. **Inventory framing, severity `info`** — "N places hardcode payment methods," with
   the migration's preconditions in the message.
2. **Intent heuristics rank, never decide** — dynamic values and single non-card
   methods (`['oxxo']`) demote; static `['card']` / `['card','link']` (the pre-DPM
   default) promote. This alone would have demoted essentially all 35 sample findings.
3. **The API-version warning is mandatory** — the one case where naive advice is
   worse than no tool.

The CLI's authentication is the long-term differentiator: only it can check the
account's actual API version and Dashboard config. That's the phase-4 `doctor` layer,
and only there does directive language ("safe to remove here") become defensible.

## v1 scope

`stripe lint` (name TBD): zero-arg scan of cwd, language auto-detect, human output
plus `--format json|sarif` (SARIF = CI annotations for free). Scan and report only —
no autofix, no network, no API key. Findings carry file:line:col, resolution mechanism
(`direct`/`var`/`recv`/`type` — already implemented, and what makes inferences
auditable), intent rank, and a docs link. Ships with the DPM pack; the rule format is
general so future packs (deprecations, version migrations) are one rule file each.

## Open questions a funder should force (phase 0)

1. **Real-repo evaluation before productionizing.** The 100% score is one
   Stripe-authored, canonical-style repo — the code the heuristics were built against.
   Run the spike over 10–20 real OSS Stripe integrations; report recall, precision,
   and post-ranking finding density. If wrapper layers collapse recall, that changes
   the design, not the polish. (~1 day; highest information per dollar available.)
2. **Distribution: in-tree vs plugin.** +25.3MB on every install for a feature most
   users never invoke is a real cost, and it's entangled with the runtime choice
   (fetch-on-demand grammars only make sense in-tree) and with rule-pack update
   cadence. Decide against a stated binary-size budget and an honest assessment of
   what the existing go-plugin boundary can carry (SARIF, exit codes, file access).
3. **Rule-content ownership.** Rule messages embed migration guidance that changes on
   docs timelines, and v1 has no network path to update them. Name the owning team,
   define the verification bar per pack (fixture matrix × 7 languages + adversarial
   negatives, human review of LLM drafts), and the recall path when a shipped rule is
   wrong. Given the central finding — wrong advice is worse than no tool — an unowned
   content pipeline is the plan's largest liability.
4. **Maintenance staffing.** 7 grammars tracking language syntax, 7 SDKs whose
   conventions can shift on major versions (engine edits, not rule edits), and an
   immature runtime (its changelog documents recent JS-ASI and C#-nondeterminism
   correctness campaigns). Estimate the annual load and name who carries it; state
   the plan if `gotreesitter` stalls, including the real cost of exercising the
   malivvan swap.

## Phased plan

0. **Eval + decide** (open questions above; ~1 week including the OSS-repo run).
1. **Productionize** the engine where phase 0 says it lives: port the spike, generate
   operation lists in `go generate`, apply the three resolver scoping fixes, add
   intent heuristics. Success gate: real-repo precision holds ≥95% post-ranking.
2. **Runtime decision** per the criteria above (build + measure 7-grammar `ts.wasm`).
3. **Ship** behind the inventory framing, with CLI telemetry on invocation and
   finding counts so "is anyone using this" is answerable. Kill criterion: if
   invocations or acted-on findings stay flat for a quarter, stop at inventory.
4. **`doctor` layer**: authenticated API-version + Dashboard checks; directive
   language becomes allowed only here.
5. **Autofix** (byte-range edits + reparse-verify) only after rules have field mileage.

## Try it

```bash
cd spikes/dpm-lint
CGO_ENABLED=0 go test -v ./...          # 11 findings, 5 negatives, 4 mechanisms asserted
CGO_ENABLED=0 go run . testdata          # human output with [via] tags
CGO_ENABLED=0 go run . -dump testdata    # raw grammar shapes per language
```
