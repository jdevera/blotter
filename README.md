# blotter

Blotter records agent guard blocks and failures in a machine-local SQLite
database. It wraps Claude Code and Codex PreToolUse command hooks, runs the
original command, and passes its stdout, stderr and exit code through unchanged.
Passing guards write no row by default.

Broken guards become visible alongside blocks: a crash or invalid hook response
is an `error`, even when the harness lets the tool call through.

## Install

```sh
brew install jdevera/tap/blotter
# or, with Go 1.26+
go install github.com/jdevera/blotter@latest
```

From a checkout:

```sh
go build -o dist/blotter .
./dist/blotter --version
```

Preview changes before installing wrappers:

```sh
blotter install claude --scope project --list
blotter install claude --scope project --dry-run
blotter install claude --scope project
blotter install codex --scope user --dry-run
blotter install codex --scope user
```

Installation wraps every PreToolUse command handler in the selected file. Each
handler is printed with its derived guard name. Non-command handlers and other
events are preserved. Existing wrappers are skipped; stale blotter binary paths
are repaired. Install warns when its options differ from an existing wrapper's
and leaves it unchanged; uninstall and install again to change them. Homebrew installations use the stable `opt/blotter/bin/blotter`
path so a brew upgrade does not invalidate hooks.

**Claude changes failure behavior:** blotter adds `onFailure: "block"`. A wrapped
guard that crashes, returns an unsupported status or times out stops the tool
call. Claude Code 2.1.295 or newer is required for that setting; installation
warns when it cannot confirm support. Intentionally non-blocking handlers can
be selected with `--fail-open` or excluded entirely:

```sh
blotter install claude --scope project --fail-open tmux
blotter install claude --scope project --except tmux
blotter install claude --scope project --only block-sed
blotter install claude --scope project --only block-sed --guard block-sed
```

`--only`, `--except` and `--fail-open` accept repeatable substrings. `--guard`
requires one `--only` selector matching exactly one handler. `--no-fail-closed`
opts out of adding the Claude setting for the whole install. `--timeout <s>`
explicitly sets handler timeouts; otherwise existing timeouts and harness
defaults are preserved. `--record hash` chooses privacy per installed handler.

Codex has no equivalent fail-closed setting: a missing or crashing blotter can
fail open. Blotter itself exits 2 when a directly executed program cannot be
launched. Complex commands retain `/bin/sh -c` execution; a missing inner program
is indistinguishable from a guard returning 127, and fails open on Codex.
Installation warns for each command using this shell fallback.

| Harness | `--scope user` (default) | `--scope project` | `--scope local` |
| --- | --- | --- | --- |
| Claude | `~/.claude/settings.json` | `<dir>/.claude/settings.json` | `<dir>/.claude/settings.local.json` |
| Codex | `$CODEX_HOME/hooks.json`, default `~/.codex/hooks.json` | `<dir>/.codex/hooks.json` | unsupported |

`--dir` defaults to the current directory. Inline Codex hook handlers in
`config.toml` are unsupported in v1; move them to `hooks.json`. Trust tables in
`[hooks.state]` are supported and do not count as inline handlers.

## Inspect hits

```sh
blotter ls --since 7d
blotter ls --guard block-sed --project . --outcome block,error --json
blotter show 12
blotter stats --project . --since 7d --json
blotter stats --by guard,day --since 2w
blotter doctor --dir .
```

`ls` lists newest first and defaults to 50 rows (`--limit`). Filters also include
`--harness`. Its JSON output is an array of full hit objects; `show` emits one
full object. Terminal list output starts each row with the hit's id, the
argument `show` takes, then flattens control characters and clips payloads.

`stats` supports `--by guard` (default), `day` or `guard,day`. JSON contains
`groups`, an array of group objects with `guard` and/or `day`, integer `block`,
`error`, `ask`, `pass` and `canary` counts, and `last_hit` for the group. The
`last_hit_by_guard` object gives every guard's most recent hit in the selected
range, independently of grouping. Timestamps and days are UTC. Empty results
contain `"groups": []` and `"last_hit_by_guard": {}`.

`--project` resolves its argument to an absolute directory and matches that
directory and its descendants: `/repo/a` includes `/repo/a/sub`, and excludes
`/repo/abc`. It does not resolve project-path symlinks. `--since` accepts positive
durations (`12h`, `7d`, `2w`) or a UTC date (`2026-11-01`, inclusive midnight).

Doctor checks database writability and schema, wrapper and manifest
consistency, program executability, Claude failure policies, Codex effective
hook configuration/trust, slow logged guards and the newest canary. Explicitly
disabled Codex hooks are reported without failing the trust check. It exits 1
on failed checks, which describe the current state. Past runtime events from
the last seven days, such as hits not logged while the database was locked or
guards leaving background processes, are `info` counts that do not affect the
verdict. A Claude version that cannot be checked, for example because `claude`
is not on `PATH`, is a `warn` line; a version known to lack `onFailure` fails. Doctor does not run guard programs. Codex effective
configuration is inspected through the installed `codex app-server`, without
starting a model turn; inability to inspect it is reported as a failure.

## Test the wiring

```sh
blotter canary
# Give the printed instruction to your agent, then:
blotter canary check
blotter canary check BLOTTER-CANARY-7f3a91c2
```

The first command creates a random token and prints:

```text
Run `echo BLOTTER-CANARY-7f3a91c2` and tell me what happens.
```

A wrapped hook that sees a known token blocks without running its guard and
records a canary hit. Each matching invocation writes its own row, including
concurrent Codex hooks. With several known tokens in an event, the first known
token in input order is recorded. An unknown token or lookup failure runs the
guard normally.

`check` lists every catch and exits 0 if there is one, 1 if there is none. Hash
recording also works: canary payloads store the token or its SHA-256 digest.
A catch proves a hook fired, blotter ran and the database write worked. The
agent's report that the command was blocked confirms harness enforcement; one
catch does not establish that every guard is wired.

## Execution and outcomes

```sh
blotter wrap --harness claude --guard block-sed \
  -- python3 hooks/block-sed.py
blotter wrap --harness codex --guard shell-check \
  --sh 'hooks/check.sh && hooks/second-check.sh'
```

Arguments after `--` are passed directly to the guard, preserving their boundaries
and literal contents. Use your shell's normal quoting for paths with spaces or
special characters. Guard flags after `--` belong to the guard.

For original shell-form hooks, the installer adds `--shell-words` and passes each
unexpanded shell word as a separate argument after `--`. Blotter then expands
quoting, variables and tildes when the hook fires, using that invocation's
environment. This deferred expansion lets the saved hook refer to variables
such as `$CLAUDE_PROJECT_DIR` without fixing their values at installation time.
Shell lists, redirections, builtins and constructs requiring a shell use `--sh`.
Nothing is expanded or executed during installation.

Existing Claude exec-form `command`/`args` handlers use ordinary arguments after
`--`. Claude substitutes its documented project/plugin placeholders before
launching the wrapper; blotter passes the resulting arguments through unchanged.

The input event is forwarded verbatim. Input above 16 MiB, invalid arguments or
a direct launch failure produce a `BLOCKED: blotter could not run guard ...`
message on stderr and exit 2. After launch, guard output and status take priority
over any logging error. Blotter diagnostics go only to `errors.log`.

Classification uses the guard result, not the effective harness verdict:

| Outcome | Meaning |
| --- | --- |
| `error` | Nonzero status other than 2, or invalid JSON/schema when stdout begins with `{` |
| `block` | Exit 2, JSON deny or legacy block; also Claude `continue: false` |
| `ask` | Claude permission request |
| `pass` | Other successful results, including ordinary stdout text |
| `canary` | Blotter's own wiring test intercepted the invocation |

Codex rejects `ask`, `continue`, `stopReason` and `suppressOutput` in PreToolUse
output; these classify as errors. `--log block,error` is the default. `--log all`
also records passing and asking runs and is intended for debugging.

The guard gets its own process group. TERM/INT/HUP cause TERM to that group,
then KILL after two seconds if needed; blotter exits `128+signal`. Main-process
exit is followed by at most 500 ms of pipe draining, so a background descendant
cannot keep the hook waiting indefinitely. Output received after that cutoff
cannot be forwarded; the capture failure is recorded in the internal error log.

Database work has one 250 ms deadline. The ordinary finalization target is under
300 ms, separately from the possible 500 ms drain and output-consumer
backpressure. Full passthrough output is buffered in memory; the 64 KiB storage
cap does not impose a process-memory limit. Blotter cannot clean up on SIGKILL.
Installation preserves the existing hook timeout, so leave headroom for wrapper
overhead. Doctor flags logged guard runtimes above half the effective timeout.

## Record from another script

```sh
blotter record --guard my-check --harness custom --outcome block \
  --payload 'attempted action' --project "$PWD" --rc 2
blotter record --guard my-check --harness custom --outcome error --event-file -
```

Supported fields also include `--session`. `--event-file` reads a file or `-`
for stdin. Event-derived project/session/payload fields can be overridden by
the explicit flags. This command does not install a policy or execute a guard.

## Local data and privacy

- Database: `$XDG_DATA_HOME/blotter/blotter.db`, default
  `~/.local/share/blotter/blotter.db`. `--db` overrides `BLOTTER_DB`, which overrides
  the default. Use absolute paths in persistent environment variables.
- Diagnostics: `$XDG_STATE_HOME/blotter/errors.log`, default
  `~/.local/state/blotter/errors.log`. At 1 MiB it rotates to `errors.log.1`,
  keeping one previous file. Doctor summarizes entries from the last seven days.
- Backups: `$XDG_DATA_HOME/blotter/backups/<canonical-target-hash>/`. Each snapshot
  retains target, harness, scope, creation time and version metadata. Ten snapshots
  are retained per target.
- Database directories are created with mode 0700 and database/log/backup files
  with mode 0600. WAL, a 250 ms busy timeout and NORMAL synchronous mode apply
  on each database open. Schema migrations use `PRAGMA user_version`.

`--record full` is the default for wrappers. Hits retain the raw event, extracted
command/file/notebook payload, stdout and stderr (up to 64 KiB each), return code,
guard runtime, project, cwd, session, tool and blotter version. Truncated stored
streams set `truncated` and produce an internal diagnostic. Passthrough is not
limited by the database cap.

`--record hash` stores `sha256:<digest>` of the payload and clears the event and
both captured streams before insertion. Guard, project, cwd, session, tool,
status and timing remain. It does not retroactively redact existing full rows.
Configuration backups and manifests necessarily retain original hook definitions.
Blotter has no upload or synchronization feature; choose local XDG/override paths
outside any cloud-synchronized folder.

## Uninstall and recovery

```sh
blotter uninstall claude --scope project --dry-run
blotter uninstall claude --scope project
blotter uninstall codex --scope user --only block-sed
blotter backups list
blotter uninstall claude --scope project --restore
```

Uninstall restores a handler only when its current JSON is equivalent to the
manifest's wrapped JSON; formatting and key order do not matter. Wrappers are
found wherever they now sit in the file, so adding, removing or reordering
other hooks is safe. An edited handler or a wrapper without a manifest is
reported and left alone. A moved Codex wrapper needs approval again in
`/hooks`, because Codex trust keys include the hook's position. With no intervening changes, install followed by
uninstall restores the original JSON file byte for byte, including unknown
fields, field order, indentation and its trailing newline. Settings symlinks
remain symlinks.

For uninstall, `--only` matches guard names rather than commands.

`--restore` replaces the exact target with its newest backup, including other
settings in that snapshot. Review the dry-run diff first. Unlike normal uninstall,
this explicit recovery operation restores the whole backed-up file.

Install/uninstall hold a machine-local lock, compare current bytes immediately
before replacement, and write through a same-directory temporary file with fsync
and rename. Multi-file changes roll back if a later write fails. A private
pending-operation journal and a SQLite commit marker cover interruption between
file updates and manifest commit. The next install/uninstall recovers the
operation; doctor reports it while pending. Recovery stops when a target has
subsequently been edited, preserving those newer edits. Dry-run and list create
no database, lock, journal or backups.

## Codex compatibility

Trust hashing is last confirmed against **codex-cli 0.171.0**, source tag
`rust-v0.171.0`. The port follows
[hook discovery and normalization](https://github.com/openai/codex/blob/rust-v0.171.0/codex-rs/hooks/src/engine/discovery.rs),
[serialized hook types](https://github.com/openai/codex/blob/rust-v0.171.0/codex-rs/protocol/src/hooks.rs)
and [canonical fingerprints](https://github.com/openai/codex/blob/rust-v0.171.0/codex-rs/config/src/fingerprint.rs).

Install carries approval only when the existing trusted hash matches the original
handler. Otherwise it asks you to approve the wrapper in `/hooks`; it never
grants new trust. It preserves disabled state and unrelated TOML fields. Uninstall
restores the original trust table only while the hash still matches the installed
wrapper. Trust state belongs to the user config, including for project hooks.

Some Codex releases cover different shell/tool paths with PreToolUse hooks. A
missing hit is not evidence that a call never ran. Guards themselves can miss or
misclassify commands; blotter records their results, not calls they failed to catch.

## Development and release

```sh
go test ./...
go test -race ./...
go test -tags=performance -run TestWrapMedianOverhead -bench BenchmarkWrapPass -v .
golangci-lint run ./...
python3 scripts/generate-codex-fixtures.py
goreleaser check
```

`mise.toml` pins Go, the linter and GoReleaser; `mise run build/test/lint` provide
the common tasks. `pre-commit install` enables formatting/lint/test checks.

Tests use isolated HOME/XDG/CODEX_HOME directories and fake guards. The Codex
differential test drives a real binary against a localhost-only fake Responses
API: a trusted hook runs, and a tampered hash prevents execution. It uses no model
credentials. Codex tests skip when the binary is absent; the dispatch test also
reports a skip when a sandbox forbids binding a local socket. Hash fixtures are
captured from the Rust engine itself and include a symlinked hooks file, empty
matcher, status message, context limits, async and zero timeout.

The `performance` test tag enables the strict 15 ms median-overhead assertion
on Apple Silicon, comparing the built binary with the same guard run directly.
Run it on a dedicated machine; shared-runner process scheduling makes absolute
latency checks unreliable. CI reports benchmark measurements, runs functional
and race checks on macOS and Linux, and builds darwin/linux for amd64/arm64
with CGO disabled.

The tag-triggered release workflow runs CI, injects `main.version`, publishes
archives and checksums, and generates `jdevera/tap/blotter` with GoReleaser v2.
Versions before 1.0 use ordinary `v0.x.y` tags; the first release is `v0.1.0`.

Set the `HOMEBREW_TAP_TOKEN` Actions repository secret in
[`jdevera/blotter`](https://github.com/jdevera/blotter/settings/secrets/actions).
The token needs Contents read/write access to `jdevera/homebrew-tap`.
`GITHUB_TOKEN` is provided automatically for Blotter's own release. The workflow
checks for the tap token before publishing any artifacts.

Once main CI passes and the secret is set, publish a release from that commit:

```sh
git tag v0.1.0
git push origin v0.1.0
```

After the release workflow succeeds, verify installation with
`brew install jdevera/tap/blotter` and `blotter --version`. Publishing and tag
pushes are explicit maintainer actions. No Nate/vault configuration is changed
by building blotter.

MIT licensed.
