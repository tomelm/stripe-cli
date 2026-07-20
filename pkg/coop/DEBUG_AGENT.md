# Co-op Debug Agent

The debug agent is a deterministic fake agent for local TUI debugging. It runs the normal Co-op TUI flow, but the right-hand pane is driven by scripted session updates instead of a real Claude/Codex agent.

## Manual TUI debugging

Build the local CLI:

```bash
go build -o bin/stripe cmd/stripe/main.go
```

Start a debug session:

```bash
bin/stripe coop start one-time-payment --language=node --debug-agent
```

This opens the same tmux split as `coop start`:

- left pane: the Co-op TUI
- right pane: deterministic debug-agent logs

The fake agent advances steps quickly, pauses at review cards, handles `c` confirm, handles `r` request changes, and eventually drives the completion / next-steps view.

## Journey-outcome gating (--simulate-outcome, --simulate-observe, --live)

Some blueprint steps end in a `uiComponent` node whose journey settles a specific Stripe object — a Checkout Session reaching `complete`, an Invoice being paid, and so on. For those nodes the real agent protocol binds an object id at report-work time, and the TUI then watches Stripe for that object and blocks `c` confirm until the outcome is observed: "Confirm is locked" until the human actually completes the journey, so nobody rubber-stamps a screen that was never exercised. The debug agent has three hidden flags for exercising this gate without a real agent or a real browser:

- `--simulate-outcome=bind` (the default) makes the fake agent play the agent side of the contract, attaching a synthetic `<prefix>debug_<step>` id to the gated node exactly as a real agent's `--outcome` binding would. Pass `off` to omit the binding — note that report-work then fails closed on gated nodes, which is the protocol working as designed.
- `--simulate-observe=<mode>` scripts the observer side headlessly, with zero network: `after=<duration>` flips the outcome to observed after the given delay (confirm unlocks on its own); `fail` marks it failed (confirm stays locked; only `r` request changes works); `unavailable` marks the check unavailable (falls open to a human attestation); `never` leaves it pending forever, for proving the gate holds indefinitely. The default is `after=2s`, so a plain `--debug-agent` run exercises the gate briefly and still completes on its own.
- `--live` replaces simulation entirely: it executes the blueprint's `apiRequest` nodes for real against Stripe with your configured test-mode key, binds the real object id the response returns, and lets the TUI's outcome observer watch that genuine object — so a human can complete an actual Checkout/Invoice/etc. journey in a browser against a live TUI while every other step still runs through the deterministic debug agent.

All three are debug-agent-only passthroughs, available on both `stripe coop debug-agent --session <id>` and `stripe coop start ... --debug-agent` (which forwards them to the spawned debug-agent pane), e.g.:

```bash
bin/stripe coop start one-time-payment --language=node --debug-agent \
  --simulate-outcome=bind --simulate-observe=after=5s
```

## Automated tmux smoke test

Run:

```bash
scripts/test-coop-debug-agent-tmux.sh
```

This builds a temporary CLI binary, launches a `173x50` tmux session, checks the approximate `69x50` TUI pane, requests changes once, confirms remaining reviews, and asserts the completion view appears. It also runs two gated-outcome scenarios against one-time-payment's checkout step: a positive path (`--simulate-outcome=bind --simulate-observe=after=5s`) that confirms the review card shows "Watching cs_debug...", confirm is locked until the simulated observation lands, and the session then reaches completion; and a negative path (`--simulate-observe=never`) that confirms the gate stays locked across repeated confirm attempts and that `r` request changes still recovers the step.

Use this script for regression checks. Use `bin/stripe coop start ... --debug-agent` when manually inspecting layout, copy, spacing, or interactions.

## Notes

- `--debug-agent`, `--simulate-outcome`, `--simulate-observe`, and `--live` are all hidden and intended only for local development.
- The internal `stripe coop debug-agent --session <id>` command is launched automatically by `coop start --debug-agent`; you normally should not run it directly.
- The flow uses normal Co-op session files under your configured Stripe CLI config directory, so use a local build when testing unmerged TUI changes.
