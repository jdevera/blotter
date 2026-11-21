# Blotter

Single Go CLI. `main.go` is the module-root executable; behavior lives in
`internal/`. Read this guide before working and follow the existing patterns.

## Workflow

- Treat git status and diffs as read-only; never revert in-progress work.
- Plan against existing code before external references. Surface real trade-offs.
- When asked to plan, plan only until the user confirms implementation.
- Do not push, open/close PRs, comment on GitHub or send messages without asking.
- Local commits are allowed, but `.local/` working notes must remain uncommitted.
- Match surrounding style, fix root causes, reuse helpers and delete unused code.
- No compatibility shims or breadcrumb comments unless requested.

## Verification

`mise run build`, `mise run test`, `mise run lint`.
Use isolated HOME/XDG/CODEX_HOME directories for tests; never edit live hooks.
Wrap must preserve guard output and status even when storage fails. All config
edits must be reversible, preserve unrelated bytes, and detect concurrent edits.
Codex trust is pinned to rust-v0.171.0; update fixtures before changing the port.

When interacting with Cloudflare use cf unless a Wrangler configuration exists.
