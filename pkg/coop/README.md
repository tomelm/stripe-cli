# Co-op Mode

Co-op mode enables an AI agent and a human developer to build Stripe integrations together in real time. The agent writes code and reports attempt-scoped progress; Co-op verifies supported Stripe resources and state; the developer watches in a terminal UI and reviews visible UI or Dashboard-owned work.

## Architecture

```text
blueprint JSON ─┐
                ├─> pure plan compiler ─> bounded Stripe reads ─> workflow policy
rule catalog ───┘             ▲                                  │
                              │                                  ▼
request/event streams ─> facts, bindings, triggers       attempt history
                                                               ▲  ▲
                                                               │  │
                                                            TUI  agent CLI
```

The shared JSON session file is the durable boundary between the TUI and agent CLI. Writes are atomic and attempt-scoped. Automatic findings are applied as complete snapshots behind a per-attempt watermark, so an older asynchronous evaluation cannot overwrite or resurrect findings from a newer one. A test-mode reader must authenticate its exact `/v1/account` identity before the first usable account can atomically pin the session; an empty or different account fails closed. A single TUI-owned observer may consume the CLI's existing request/event streams to trigger rereads and attach supporting evidence; polling is the fallback. The observer retries credentials after first-run sandbox provisioning and reconnects closed streams. Stream observations never pass a check by themselves.

## Node State Machine

```
pending ──→ active ──→ review ──→ done     (human UI/Dashboard review)
   │          │          │
   │          │          └──→ active        (request changes: developer entered feedback)
   │          │
   │          └──→ done                     (verified or explicitly unverified non-UI work)
   │          └──→ skipped                  (agent decides node doesn't apply)
   │
   └──→ skipped                             (agent skips from pending)
```

**Terminal states:** `done`, `skipped` — no transitions out.

**Transitions are validated** — `session.TransitionNode()` returns an error for invalid transitions (e.g. pending→done, done→active).

## Session States

```
active ──→ completed    (all nodes done/skipped, or "stripe coop stop")
   │
   └──→ aborted         ("stripe coop stop --abort")
```

## Commands

### User-facing (human runs these)
| Command | Purpose |
|---------|---------|
| `stripe coop start [blueprint]` | Launch tmux split with agent + TUI |
| `stripe coop join [session-id]` | Open the TUI for an existing session |
| `stripe coop status` | Show session summary |
| `stripe coop stop` | End the session |
| `stripe coop recommend` | List available blueprints |

### Agent-facing (AI agent runs these)
| Command | Purpose |
|---------|---------|
| `stripe coop run <blueprint>` | Create a session (outputs JSON with instructions) |
| `stripe coop agent start-work --step <n>` | Mark a node as active |
| `stripe coop agent report-work --step <n> --attempt <a>` | Submit an attempt with implementation evidence, requested resource IDs, and an app URL for UI work |
| `stripe coop agent report-check --step <n> --attempt <a>` | Add an agent-reported check to the current attempt |
| `stripe coop agent skip --step <n> --attempt <a>` | Skip the current attempt |
| `stripe coop agent await-review --step <n> --attempt <a>` | Poll direct checks and block until Co-op or the developer decides |
| `stripe coop agent next-action` | Show post-completion options (blocks until selection) |
| `stripe coop agent start-followup` | Start an internal guided follow-up session selected from next actions |

All agent commands output JSON with an `ok` field and an exact `next` command. `start-work` creates or returns the open append-only attempt. Later mutations must carry that attempt number, so delayed writes cannot modify a correction attempt. The `--step` flag name is retained for the CLI, but its value is the 1-based node number across the session.

## TUI Keybindings

| Key | Action |
|-----|--------|
| `↑`/`k` | Move cursor up |
| `↓`/`j` | Move cursor down |
| `PgUp`/`b` | Page up |
| `PgDn`/`Space` | Page down |
| `Home`/`g` | Jump to top |
| `End`/`G` | Jump to bottom |
| `←` | Collapse selected step |
| `→` | Expand selected step |
| `e` / `?` / `Enter` | Toggle detail panel for selected step or node |
| `Tab` | Move to the next detail tab |
| `Esc` | Close details or cancel a prompt |
| `c` | Confirm the selected review item |
| `r` | Request changes for the selected review item |
| `y` | Copy the selected review command when one is available |
| `f` | Resume following the active/review node after manual navigation |
| `o` | Open the selected app URL, or the sandbox claim URL when no app is selected |
| `q` / `Ctrl+C` | Quit TUI |

When requesting changes, `r` opens a feedback prompt. Press `Enter` to submit a note and move the reviewed node or step back to `active`; press `Esc` to cancel.

In the completion view:
| Key | Action |
|-----|--------|
| `↑`/`↓` | Navigate suggestions |
| `Enter` | Select a suggestion |
| `q` | Quit |

## Example Flow

```bash
# Explicit blueprint: developer starts a pre-created session (launches tmux with agent + TUI)
$ stripe coop start one-time-payment --language=node

# What happens behind the scenes:
# 1. CLI creates the session and gives the agent the exact session protocol
# 2. Agent (Claude/Codex) is launched in right pane
# 3. TUI appears in left pane showing step progress
# 4. Agent starts from the provided next command:
#      stripe coop agent start-work --session=coop_abc123 --step=1 --note="Beginning: Understand the project"
# 5. Agent works through steps, calling:
#      stripe coop agent start-work --session=coop_abc123 --step=1 --note="Scanning project"
#      # start-work returns attempt=1 and its exact report command
#      stripe coop agent report-work --session=coop_abc123 --step=1 --attempt=1 --note="Found Next.js app"
#      stripe coop agent start-work --session=coop_abc123 --step=2 --note="Creating product"
#      stripe coop agent report-work --session=coop_abc123 --step=2 --attempt=1 --file=server.js --lines=5-20 --note="Created product" --stripe-resource=product=prod_123
#      stripe coop agent await-review --session=coop_abc123 --step=2 --attempt=1
# 6. Supported non-UI work is confirmed automatically. For a uiComponent the
#    agent supplies --app-url, the developer opens it with 'o', then confirms
#    the visible UI or requests changes while ordinary checks keep running.
# 7. Agent continues to next step
# 8. After all steps: agent runs "stripe coop agent next-action --session=coop_abc123"
# 9. Developer picks what to do next from TUI suggestions
```

Discovery mode is different:

```bash
$ stripe coop start

# The agent explores the codebase, asks what the developer wants to build,
# runs `stripe coop recommend`, and only then runs:
#   stripe coop run <blueprint-id> --language=<lang>
```

Post-completion choices are written into the session file for the agent. Deploy follow-ups are internal guided sessions, not blueprints: the agent runs `stripe coop agent start-followup --session=<parent> --action=deploy` or `--action=deploy-update`. The child session returns to the completed parent by running `stripe coop agent next-action --session=<parent> --completed=<action>` when it finishes.

## Verification Decisions

- Required direct resource/state checks all pass: non-UI work completes automatically.
- A deterministic required mismatch: the attempt ends, its evidence is retained, and a correction attempt is created with expected/observed facts and repair guidance.
- A state is still progressing: `await-review` polls it; during UI review the TUI stays quietly pending.
- A direct check is unavailable: it is never presented as passed. Non-UI work can continue with the explicit unavailable result; UI confirmation requires a visibly recorded human override.
- Completed-but-unverified work remains distinct in attempt history and in the TUI outline/completion summary; it is not rendered as a green automatic success.
- Request/event observations are supporting evidence only. Missing or ambiguous observations never become failures. Even a uniquely path-matched 4xx/5xx remains advisory because Stripe request logs are account-wide and carry no Co-op attempt token; Co-op will not blame or wake the agent without a stronger correlation key.
- An event may propose a new resource binding only for a reported, still-unbound state attempt after the human opens an app in the same step, when exactly one attempt matches. The window begins at the later of report/open and expires after five minutes. A direct reader must prove the object was created inside it. An observed event can never replace an existing binding.
- A state rule proves the authoritative Stripe object's state, not that application webhook code received or processed the event. Handler-specific side effects remain agent/human evidence until a direct reusable rule can verify them.
- Real `uiComponent` work requires a syntactically safe application URL. Co-op performs no reachability or authentication probe; the human opens and judges it. `dashboard` work remains human-owned without an app URL.

## Heartbeat

When the agent runs `stripe coop agent await-review`, it writes a `.heartbeat` file every 500ms. The TUI checks this file:
- **Fresh heartbeat (< 5s old):** Agent is actively waiting for confirmation
- **No heartbeat + no session update in 2min:** Show idle warning

The heartbeat file is cleaned up when `await` exits.

## Resuming

`stripe coop join` is the recovery path. With no session ID, it opens the most
recent active session, falling back to the latest session if none are active.
Use `stripe coop join --resume` to pick from recent sessions.

| Issue | What to do |
|-------|------------|
| Node is active | Rejoin the session and check the agent pane/TUI state |
| Node or step is in review | Rejoin the session and confirm or request changes |
| Agent appears idle | Rejoin the session; the TUI shows heartbeat/idle state |
| Need a specific older session | Run `stripe coop join --resume` |

## Blueprint Format

Blueprints are embedded JSON in `pkg/coop/blueprints/`. Each has:
- `id` — unique identifier (also the filename without .json)
- `title`, `description` — human-readable
- `steps` — ordered groups of nodes

Each node has:
- `type` — `apiRequest`, `asyncHandler`, `uiComponent`, `cliCommand`, `dashboard`, `setUpWebhooks`, `testHelper`
- `description` — what the agent should do (source of truth)
- `review_prompt` — what the human should check before confirming
- `review_command` — optional command the TUI can show/copy for developer verification
- `request` — API request details (for `apiRequest` nodes with SDK snippet support)
- `request.hidden_params` — request fields that should not be shown directly in the TUI
- `requests` — API-backed test helper requests for `testHelper` nodes
- `events` — webhook events (for `asyncHandler` nodes)

Verification applicability is derived from this existing blueprint data: requests select resource rules, `${node...}` references select cataloged relationship predicates, events select state rules, and `uiComponent` selects the app handoff. Reusable Stripe object predicates and repair guidance live in the strictly validated embedded catalog in `checks/catalog.json`; blueprints do not duplicate them in sidecars. Unsupported operations, events, relationships, and runtime-resolved inputs compile to explicit advisory coverage gaps rather than silently disappearing or being treated as passes.

`testHelper` request metadata tells the agent which Stripe-backed test helpers can advance test state. Agents should use those helpers while verifying work, but should not encode helper-only request parameters into the user's application.

### Syncing Blueprints

Workbench blueprint definitions are the source of truth. Do not supplement or modify `pkg/coop/blueprints/` by hand to add CLI-only product work. Update the upstream blueprint source, then sync the CLI-friendly JSON:

```bash
BLUEPRINT_SOURCE=/path/to/pay-server/frontend/workbench/shared/blueprints/src/blueprintDefinitions make sync-blueprints
```

If pay-server has already exported `dist/blueprints/*.json`, `BLUEPRINT_SOURCE`
can point at that directory instead.

After syncing, test with `go run ./cmd/stripe coop run <blueprint-id>`. Prefix matching works: short prefixes resolve to full IDs if unambiguous.

## Troubleshooting

| Problem | Cause | Solution |
|---------|-------|----------|
| TUI shows "Agent appears idle" | Agent crashed or stopped | Check the agent pane; restart with `stripe coop start` |
| Agent stuck on "await" | Developer hasn't confirmed | Press `c` in TUI to confirm, or `r` to request changes |
| "Version conflict" error | TUI and agent wrote simultaneously | Agent retries the command (safe to re-run) |
| "timed out waiting for session lock" | A previous writer left a `.lock` file behind | If no `stripe coop` command is running, remove the named lock file and retry |
| TUI shows wrong session | Multiple sessions exist | Use `stripe coop join <session-id>` with the correct ID |
| Steps not updating in TUI | Agent created a duplicate session | Check `stripe coop status` for the correct session ID |
| Agent ignores "next" hint | LLM didn't follow instructions | Copy the `next` value and run it manually, or restart |
| Double footer / layout broken | Terminal resize not detected | Resize the terminal window (triggers recalculation) |
| "Blueprint not found" | Typo in blueprint ID | Run `stripe coop recommend` to see available IDs |

## Locking

Writes are serialized with a per-session `.lock` file. `Store.Write()` also checks the file's current version before writing. If another writer changed the file since you read it, the write fails with a version conflict error. This prevents the TUI and agent from clobbering each other's changes.

## File Structure

```
pkg/coop/
  appsurface/        — Syntax-only app URL safety validation
  checks/            — Embedded Stripe rule catalog and pure step compiler
  checkrun/          — Bounded read-only Stripe evaluator
  observe/           — Bounded request/event normalization and attribution
  types.go          — Session, Node, Step types and constants
  session.go        — State machine, validation, queries
  store.go          — Atomic file I/O, heartbeat, lock files, optimistic locking
  blueprint.go      — Blueprint type, embed loader, prefix matching
  guided_action.go  — In-code guided follow-up session model
  snippet.go        — SDK snippet fetcher (docs.stripe.com)
  blueprints/       — Embedded JSON blueprints
  colors/           — Sail Design System palette helpers
  followups/        — Built-in guided follow-up definitions

pkg/coop/tui/
  app.go            — tea.Program entry points
  model.go          — Bubbletea model and Update loop
  view.go           — Top-level rendering
  commands.go       — Async commands (polling, snippets, session discovery)
  completion.go     — Post-completion suggestion view
  detail.go         — Detail panel rendering
  keymap.go         — Keyboard bindings
  layout.go         — Responsive layout calculations
  markdown.go       — Glamour rendering helpers
  mouse.go          — Mouse interactions
  outline.go        — Step/node outline rendering
  review.go         — Review card rendering
  selection.go      — Navigation and selection helpers
  helpers.go        — Word wrap, formatting, browser open
  messages.go       — Custom message types
  theme.go          — Sail Design System colors

pkg/coop/workflow/
  service.go        — Attempt-scoped lifecycle operations for agent commands and TUI review actions
  verification.go   — Product-agnostic result policy and evaluator boundary

pkg/coop/helpers/
  nextaction.go     — Post-completion suggestions, environment detection, and next-action responses
  prompt.go         — Shared Huh prompt helpers using Sail-styled prompts
  review.go         — Shared step-review navigation rules

pkg/cmd/coop/
  coop.go           — Parent command, subcommand registration, command-package options
  coop_start.go     — User-facing orchestrator (tmux launcher)
  coop_launcher.go  — Agent detection and tmux/process management
  coop_run.go       — Agent-facing session creator
  coop_agent.go     — Typed agent lifecycle commands
  coop_join.go      — TUI launcher
  coop_status.go    — Session status display
  coop_stop.go      — End session
  coop_recommend.go — Blueprint discovery
```

## Local Harness Artifacts

The repository-level `bin/` directory remains ignored. Tmux harness isolation files under `bin/` are treated as local development artifacts unless a specific script or fixture is moved into a tracked source path with tests. Do not commit the ignored `bin/` directory wholesale.
