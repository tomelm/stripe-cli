# Co-op Eval Fixture: Scrumboy

This workspace is a pinned copy of `markrai/scrumboy` with a small eval overlay.

Use the existing project-management app shape:

- Go backend in `internal`.
- HTTP routing in `internal/httpapi`.
- Persistence in `internal/store`.
- Docker Compose starts the app at `http://localhost:8080`.

For subscription work, map Stripe customers/subscriptions to the existing user or
project ownership model. Add plan gates to existing project/workspace behavior
instead of building a standalone billing demo.

Eval verification should prove both sides: Stripe objects/webhooks are correct
and the app enforces the intended subscription state.
