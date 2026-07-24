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

In the request-changes editor, Enter inserts a newline and Ctrl+S sends the
feedback. Opening the submitted app with `o` is optional; confirmation does not
depend on opening it.

## Automated tmux smoke test

Run:

```bash
scripts/test-coop-debug-agent-tmux.sh
```

This builds a temporary CLI binary and tests both `173x50` and `260x50` tmux
sessions. The smoke test checks:

- the expected narrow and wide split layouts;
- the review card, confirmation control, and visibly optional app-opening copy;
- a real tmux bracketed paste containing multiline feedback longer than 500
  characters;
- Ctrl+S submission and the debug agent's resulting rerun; and
- one-press confirmation and completion without sending `o`.

Use this script for regression checks. Use `bin/stripe coop start ... --debug-agent` when manually inspecting layout, copy, spacing, or interactions.

## Notes

- `--debug-agent` is hidden and intended only for local development.
- The internal `stripe coop debug-agent --session <id>` command is launched automatically by `coop start --debug-agent`; you normally should not run it directly.
- The flow uses normal Co-op session files under your configured Stripe CLI config directory, so use a local build when testing unmerged TUI changes.
