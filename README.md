# autok-stack

`stack` is a terminal dashboard for running a multi-service dev stack — long-lived processes, podman-compose services, log tails, and one-key restarts — driven by a single `stack.toml` at the root of whatever repo you're in.

It was originally bolted into the autok monorepo as `stack-terminal/` with hardcoded service definitions. This repo is the config-driven extraction.

## Install

```bash
./scripts/install
```

Builds and drops a `stack` binary at `~/.local/bin/stack`. Make sure that directory is on your `PATH`.

## Use

Run `stack` from anywhere inside a repo that has a `stack.toml` — it walks up to find the config:

```bash
cd ~/code/myproject/some/subdir
stack
```

To make `stack` launch one default repo even when your shell is elsewhere, set:

```bash
export STACK_DEFAULT_DIR="$HOME/code/myproject"
```

The nearest `stack.toml` still wins when you are inside another stack-managed repo.

## Config — `stack.toml`

All non-absolute paths are resolved relative to the directory containing `stack.toml`. `log_file` is always relative to `log_dir`.

```toml
[stack]
title = "MY STACK"                          # shown in the TUI hero, default "STACK"
log_dir = ".tmp/dev-stack"                  # default ".tmp/dev-stack"
required_tools = ["go", "bun", "podman"]    # checked on startup; missing tool = fatal
compose_command = ["podman", "compose"]     # optional, defaults to podman compose

# A plain long-running process:
[[service]]
key = "server"
name = "Server"
ports = ["8000"]                            # checked for readiness; unrelated listeners are never killed
work_dir = "backend"
log_file = "server.log"
command = ["./dev.sh"]
depends_on = ["postgres"]                    # optional; dependency cycles are rejected
readiness_timeout = "5s"                    # optional, default 5s
ready_url = "http://localhost:8000/readyz"   # optional authoritative startup check
live_url = "http://localhost:8000/livez"     # optional steady-state health check
auto_restart = false                        # optional, exp-backoff restart on crash

# A podman/docker compose service group:
[[service]]
key = "postgres"
name = "Postgres"
ports = ["5432"]
work_dir = "backend"
log_file = "postgres.log"
compose_file = "backend/docker-compose.yml"
compose_services = ["postgres", "pgbouncer"]
```

A service is either a `command` (plain process) or a `compose_services` list (managed via `compose_command`). Compose services aren't torn down when you quit the dashboard.

Every command service starts behind a stack-owned process-group leader with a
random ownership token recorded in `.tmp/dev-stack/processes.json`. After an
unclean supervisor exit, the next stack instance reclaims a surviving group only
when that leader still presents the exact token in its process identity. PID or
command-name matches alone are never used as proof. If an unrelated process owns a declared port, startup fails with its
PID and command instead of terminating it. The legacy `cleanup_patterns` option
is rejected because its broad `pkill -f` behavior cannot establish ownership.

Services whose dependencies are ready start concurrently. A failed service only
blocks entries that name it in `depends_on`; unrelated services continue booting
and failures remain visible in the dashboard for manual restart. A `ready_url`
is authoritative during startup when configured. Steady-state monitoring uses
`live_url` when present, then falls back to `ready_url`, then all declared ports.
The dashboard renders immediately with `starting` rows while this work proceeds;
compose services in the same project are started as one group. Shutdown follows
the dependency graph in reverse and stops independent consumers concurrently.
Steady-state health checks run concurrently so a slow service cannot serialize
the rest of the supervisor's checks.

Quitting transitions the dashboard into a visible shutdown mode instead of
closing the terminal UI immediately. Command services move through `stopping`
and `stopped` states as their process groups receive SIGTERM (and, after the
bounded grace period, SIGKILL). Compose services are labeled `kept`. The TUI
exits only after shutdown finishes and the completed state has rendered; a
second quit request hides progress and falls back to the same idempotent cleanup.

Current service logs remain capped by `MAX_LOG_BYTES`. On the next stack start,
each non-empty log is rotated to `<service>.log.previous`, preserving one prior
session for crash diagnosis. Supervisor start, exit, and stale-recovery events
are written into the corresponding service log.

`compose_command` defaults to `["podman", "compose"]`. For Docker-backed runtimes such as Colima, set:

```toml
[stack]
required_tools = ["go", "bun", "docker"]
compose_command = ["docker", "compose"]
```

## Keys

- `j` / `k` — move selection
- `g` / `G` — jump to first / last
- `r` — restart selected service
- `q` / `Ctrl+C` — quit

## Environment

- `MAX_LOG_BYTES` — cap on per-service log file size (default 1 MiB)
