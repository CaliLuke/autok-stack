# AGENTS.md — autok-stack

A Go TUI dashboard for running a multi-service dev stack. Implemented with [Bubble Tea](https://github.com/charmbracelet/bubbletea) + [lipgloss](https://github.com/charmbracelet/lipgloss). One binary; the service catalog comes from a `stack.toml` at the consumer repo's root.

This file is for agents working on the autok-stack code. See [`README.md`](./README.md) for user-facing usage and the TOML schema reference.

## Repo layout

| Path | Role |
|---|---|
| `main.go` | Entry point, `model` struct, all TUI rendering (`renderHero`, `renderServiceList`, `renderFocusLine`, `renderLogPanel`, `renderCompactFallback`), styles, process management, podman-compose handling, singleton lock. Single file (~1500 LOC) — kept whole because rendering and process layers share the same `model` state. |
| `process_registry.go` | Token-verified process ownership, stale-group recovery, safe port-conflict inspection, and supervisor log events. |
| `config.go` | TOML schema (`fileConfig`, `stackBlock`, `serviceBlock`), `loadConfig(start)` which walks up from `start` to find `stack.toml`, conversion to `[]serviceConfig`. |
| `snapshot_test.go` | UI snapshot harness for headless polish. Driven by the `tui-snapshot-iteration` skill. |
| `scripts/install` | Build + write `~/.local/bin/stack`. |
| `.agents/skills/` | Shared agent skills (symlinked into `.claude/skills/`). |

There is no `stack.toml` in this repo — it lives in consumer repos (e.g. `autok/stack.toml`).

## Path resolution rules

Everything declared in `stack.toml` is resolved relative to the directory containing that file, with two exceptions:

- `log_file` is always relative to `log_dir` (which is itself relative to the stack.toml dir, default `.tmp/dev-stack`).
- `command[0]`, if it looks like a relative path (`./...`, `../...`, `~/...`), is resolved against the *service's own* `work_dir`, not the stack root. So `command = ["./dev.sh"]` together with `work_dir = "autok-server"` runs `autok-server/dev.sh`.

Bare commands (`bun`, `node`, `python`) stay bare and resolve via `$PATH`.

## Build / install / test

```bash
go build ./...                    # quick compile check
./scripts/install                 # full install to ~/.local/bin/stack
go vet ./...                      # sanity
go test -run TestUISnapshot -v    # regenerate UI snapshots in .tmp/snapshots/
```

Behavior tests cover configuration, startup, readiness, supervision, locking,
restart recovery, and log capping. The snapshot test separately validates the
rendered string of `model.View()` rather than runtime process behavior.

## Making changes

### Adding or amending a service-state visual

Two places carry the glyph/label switch — keep them in sync:

- `statusChip(state, width)` in `main.go` — the dashboard chip
- `renderCompactFallback()` in `main.go` — the <60×12 fallback view

After the code edit, add or amend a scene in `snapshot_test.go` that exercises the new state (set the relevant `state.*` fields on a fake service in `newFakeModel()`), then `go test -run TestUISnapshot -v` and read `.tmp/snapshots/`.

### Adding a config field

1. Add the field to `serviceBlock` in `config.go` with its `toml:"..."` tag.
2. Add to `serviceConfig` in `main.go` (the runtime struct).
3. Wire through `buildServiceConfig` in `config.go` — parse, validate, default.
4. Consume wherever needed in `main.go`.

If the field affects layout or a chip's appearance, also extend the snapshot test.

### Polishing the UI

**Use the `tui-snapshot-iteration` skill** — see [`.agents/skills/tui-snapshot-iteration/SKILL.md`](./.agents/skills/tui-snapshot-iteration/SKILL.md). Key points:

- The TUI cannot be visually evaluated from a non-interactive agent shell. Bubble Tea takes the terminal with alt-screen + raw mode; `go run` from Bash returns escape codes you can't render.
- The snapshot harness renders `model.View()` at a fixed set of widths/heights with `termenv.Ascii` forced, strips ANSI, and writes plain-text scenes to `.tmp/snapshots/`. Read them, edit, re-snapshot.
- Snapshots verify **layout** (alignment, sizing, wrapping, text, structural state changes). They do **not** verify **color, contrast, or animation timing**. After structural polish lands, install and ask the user to verify those in a real terminal.

## Architecture conventions

- **Fast tick is conditional.** A 100ms `spinnerTickCmd` runs only when `anyAnimating()` is true (something is `starting`, `restarting`, or `stopping`). Otherwise it falls back to a 500ms heartbeat so animation resumes promptly once state changes. Don't make this always-on; redrawing a 7-row dashboard 10 times a second is cheap but pointless.
- **Shutdown stays inside the TUI.** The first quit request cancels boot/restart work, streams `kept` / `stopping` / `stopped` progress through Bubble Tea, and exits only after the completion frame renders. Keep the post-`program.Run` shutdown call as an idempotent safety fallback for terminal errors and second-quit escape.
- **Compose services persist across stack quits.** `shutdownServices` skips anything where `cfg.isComposeService()` is true. Intentional — quitting the dashboard should not tear down Postgres/TypeDB. Don't add SIGTERM-on-quit for compose without an explicit user request.
- **Singleton lock**: `.tmp/dev-stack/stack.pid` at the consumer repo's root. `isLiveStackProcess(pid)` checks both that the PID is alive AND that its `comm` ends in `stack`, to avoid PID-collision false positives. A false positive only delays a manual override; a false negative would let two stacks fight over the same ports.
- **Chip vs badge styles are split.** Row chips use `chipUp/Warn/Down/Danger` (foreground only — sits cleanly on the row's background, including the selected-row tint). The hero count badge uses `badgeHealthy/Warn/Danger` (background pill, more visual weight). Don't reuse one for the other or contrast breaks.
- **Hero adapts to health.** `heroHealthy` and `heroDegraded` differ only in border color (accent green vs danger red). `renderHero` picks based on `m.anyExited`. Don't add more hero variants without a corresponding state change in the model.
- **Auto-restart is opt-in.** A service must declare `auto_restart = true` in its TOML block. Default is off — a service that exits stays exited so the user sees the failure rather than a flapping restart loop. Exponential backoff caps at 30s.

## Limits / not in scope yet

- Live filtering / search in the log panel
- Per-pane resize (log panel inherits remaining vertical space; not separately resizable)
- Historical log archival beyond one previous session — current and `.previous` logs are each bounded to one supervisor session
- Pluggable status types beyond `live` / `restart` / `exit` / `down`
- Full terminal-runtime integration coverage; the snapshot harness remains structural only

## Commit / publish conventions

- Per the parent monorepo: do not add `Co-Authored-By`, `Generated with Claude`, or similar attribution to commits. This is a paid tool, not a collaborator.
- Commit messages: explain *why*, not *what* — the diff says what changed. Two-paragraph form is fine; lead with the motivation, follow with the mechanical detail if it's non-obvious.
- The binary at `/autok-stack` (the literal `go build` default output) and the install target at `/stack` are both gitignored.

## Pointers

- [`README.md`](./README.md) — user-facing usage and full TOML schema reference.
- [`.agents/skills/tui-snapshot-iteration/SKILL.md`](./.agents/skills/tui-snapshot-iteration/SKILL.md) — how to polish the TUI from an agent shell without seeing it.
- Upstream repos: `bubbletea` for the runtime model, `lipgloss` for styles, `BurntSushi/toml` for config parsing.
