# DPM migration tooling — session summary and outcomes

**Branch:** `tomer/dpm-test` · **Date:** 2026-07-30 · **Code:** `spikes/dpm-lint/` (self-contained Go module, excluded from the CLI build)

## Goal

Explore a CLI feature that scans codebases for hardcoded `payment_method_types` and
helps users migrate to [Dynamic Payment
Methods](https://docs.stripe.com/payments/payment-methods/dynamic-payment-methods) —
across Ruby, Python, PHP, JS/TS, Go, Java, and C# — with something *strong and
deterministic* that can grow into a general migration-scanning capability.

## What got built (all working, all tested)

A four-stage pipeline, each stage proven on real Stripe sample code:

1. **Scan** — tree-sitter engine, pure Go (`odvcencio/gotreesitter`), `CGO_ENABLED=0`
   so the Linux release constraint is a non-issue. Rules are language-agnostic data
   `(param, operations, message)` derived from the vendored OpenAPI spec; all language
   knowledge lives in per-language specs written once. A literal prefilter keeps it
   fast: ~255ms across 2,020 files; clean repos never parse a single file.
2. **Resolve** — real integrations rarely put params inside the call. Four resolution
   mechanisms tie a finding to an actual Stripe operation, and every finding reports
   which one (`[direct]`, `[var:params]`, `[recv:paramsBuilder]`, `[type:options]`),
   so inferences are auditable.
3. **Doctor** (`stripe doctor dpm`) — the scanner alone cannot say whether removal is *safe*;
   that depends on account facts. Three read-only GETs using the CLI's stored
   credentials (zero CLI changes needed): account identity, Dashboard payment-method
   configuration, and the API versions recent events actually ran at. Verdicts gate
   in order: version cutoff (`2023-08-16`) → Dashboard config → intent
   (`CANDIDATE` / `SKIP` / `REVIEW` / `BLOCKED`).
4. **Fix** (`stripe fix dpm`) — computes the exact byte span that removes each finding
   (pair + separator / Java chain-link / whole statement), applies edits in memory,
   **reparses, and verifies**. Dry-run writes nothing; `--apply` writes only files
   whose edited form reparses clean.

## Outcomes (verified, reproducible)

Evaluated against **9 stripe-samples repos** (accept-a-payment plus an 8-repo sweep
including two negative controls):

- **44 findings, 0 false positives, 0 remaining misses.** Negative controls
  (issuing, identity) and three no-param payment repos all correctly zero.
- **All 44 removal spans reparse clean** across every language, including multi-edit
  files (a Java file with one chain-link and two statement removals).
- Precision behaviors held under pressure: comments describing the param, test
  assertions reading it off responses, client-side camelCase Elements options, and a
  Java server already on `automatic_payment_methods` were all correctly not flagged.
- Live `stripe doctor dpm` run against a real test account (recent traffic at `2022-11-15`,
  pre-cutoff) correctly **BLOCKED all removals** — the naive "remove it" advice would
  have silently dropped payment methods on the tool author's own account.

## The two findings that shaped the design

**Matching is not advising.** On the canonical sample, every one of 35
technically-correct findings would have been *wrong advice* — they sit in a sample
whose purpose is per-method integration, while the DPM-style samples produce zero
findings. Whether removal is correct depends on facts outside the code (Dashboard
config, API version, Payment Element vs Card Element). Hence: inventory + ranked
verdicts, never bare directives — and the account-aware doctor layer is where the
CLI's unique value lives, since no standalone linter can be authenticated.

**Call-shapes are empirical.** Each new corpus surfaced exactly one unanticipated
shape: variable indirection (`params = {...}` → `create(**params)`), route-handler
anchors, C# target-typed `new()`, and legacy PHP static calls
(`\Stripe\SetupIntent::create`) — the last found by the 8-repo sweep as a real miss
and fixed same-day. The engine converges fast, but per-language specs harden through
corpus exposure, not up-front design. This is why the proposal gates
productionization on a wider real-repo evaluation.

## Key design decisions (rationale in PROPOSAL.md)

- **Tree-sitter** over ANTLR (rules must be data, not compiled visitors) and over
  ast-grep/Semgrep (unusable as Go libraries). Two runtimes proven; embedded
  gotreesitter used for the spike (+25.3MB stripped), wasm-on-wazero (malivvan) kept
  as the fetch-on-demand alternative — production choice deferred to a measurement.
- **12 POST operations** take the param per the vendored spec (docs mention 3
  resources); `report_runs` matches the string only as a report column name and is
  structurally excluded because rules are operation-scoped.
- Resolution is **intra-file name matching, not dataflow** — probed failure modes
  (scope, kill, flow order) are documented with bounded fixes scheduled.

## State of the branch

- `spikes/dpm-lint/` — engine, doctor, fix-dry, 20+ fixtures covering every
  resolution mechanism and adversarial negatives, 4 tests + benchmark, PROPOSAL.md
  (fact-checked by a 4-agent verification pass, revised against a 15-finding
  3-lens adversarial review), this summary.
- Isolated: nested Go module; the parent CLI build (`go list ./...`) does not see it.
  Binary artifacts stay in gitignored `bin/`.

```bash
cd spikes/dpm-lint
CGO_ENABLED=0 go test -v ./...             # full suite: findings, negatives, mechanisms asserted
CGO_ENABLED=0 go run . demo dpm --dir <dir>   # guided walkthrough (humans)
CGO_ENABLED=0 go run . guide                  # agent playbook: steps, schemas, exit codes
CGO_ENABLED=0 go run . doctor dpm <dir>       # diagnose (degrades to scan-only w/o creds)
CGO_ENABLED=0 go run . doctor dpm --live      # + webhook round-trip proof
CGO_ENABLED=0 go run . fix dpm <dir>          # dry-run removals; --apply writes
```

## Recommended next steps (proposal phase 0)

1. Wider eval on real third-party OSS repos — wrapper layers are the resolver's known
   weak spot and sample repos can't stress it.
2. In-tree vs plugin decision against a binary-size budget (entangled with the
   runtime choice).
3. Rule-content ownership: who updates messages when migration guidance changes.
4. Pull a thin doctor slice into the MVP — inventory alone is the weakest version of
   this product; inventory + account context is the differentiated one.
