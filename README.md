# autok-stack

`stack` is a terminal dashboard for running a multi-service dev stack — long-lived processes, podman-compose services, log tails, and one-key restarts — driven by a single `stack.toml` at the root of whatever repo you're in.

It was originally bolted into the autok monorepo as `stack-terminal/` with hardcoded service definitions. This repo is the config-driven extraction.

## Install

Install with Homebrew on macOS or Linux:

```bash
brew install caliluke/stack/stack
stack --version
```

Homebrew builds the release from source and installs Go as a build dependency.
Install the tools required by your services separately, such as Podman, Bun, or Node.js.

To update:

```bash
brew update
brew upgrade caliluke/stack/stack
```

If you previously installed `~/.local/bin/stack`, check `which -a stack`.
Remove the old binary or put the Homebrew bin directory first on your `PATH`.

To build from a checkout instead:

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
ports = ["8000"]                            # preferred TCP ports; conflicts select a free port
work_dir = "backend"
log_file = "server.log"
command = ["./dev.sh"]
env = { API_PORT = "{{port}}" }              # optional service-specific environment
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
command-name matches alone are never used as proof. If an unrelated process owns a declared command-service port, stack selects a free port.
It leaves the unrelated process running. The legacy `cleanup_patterns` option
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

## Port allocation

Command services use their declared ports when those ports are free. If a port is occupied, stack searches upward for a free TCP port. It skips other declared ports and ports assigned within the same stack session. The dashboard, local health URLs, and supervisor logs show the assigned ports.

For a single-port command service, stack sets `PORT` to the assigned port. Programs that use another variable or a command argument need an explicit binding:

```toml
ports = ["8000"]
command = ["./dev.sh"]
env = { API_PORT = "{{port}}" }
# Alternatively: command = ["server", "--port", "{{port}}"]
```

The `env` table overrides inherited variables for command services. Startup scripts must preserve these values when they load dotenv files. Stack cannot change a port hardcoded inside a program or script.

Port references work in `env` values, command arguments, `ready_url`, and `live_url`:

| Reference | Value |
| --- | --- |
| `{{port}}` | The first assigned port of this service |
| `{{port:4318}}` | This service's assigned replacement for declared port 4318 |
| `{{port:server:8000}}` | The server service's assigned replacement for declared port 8000 |

Services with multiple ports need a binding for each port that can change. For example:

```toml
ports = ["4317", "4318"]
command = ["collector"]
env = { GRPC_ENDPOINT = "127.0.0.1:{{port:4317}}", HTTP_ENDPOINT = "127.0.0.1:{{port:4318}}" }
```

Consumers can use `env = { API_URL = "http://localhost:{{port:server:8000}}" }` to track a provider's endpoint. References do not add startup dependencies. Use `depends_on` when a consumer requires the provider to be ready.

An ordinary restart retains the assigned ports. If another process takes an assigned port, stack selects a replacement. Running command services whose configured references change restart with the new values, even without `auto_restart`.

Compose-published ports remain under the Compose file's control. Stack does not remap them. Port probes close before process launch, so another application can still take a port between the probe and the bind.

## Keys

- Click a service row to select its logs. The mouse wheel moves one service at a time.
- `m` — switch between mouse navigation and terminal text selection
- `j` / `k` — move selection
- `g` / `G` — jump to first / last
- `r` — restart selected service
- `q` / `Ctrl+C` — quit

To copy logs, press `m`, select text, and use your terminal's usual copy command.
The display stays still while services continue to run.
Press `m` again to show the latest output and restore mouse navigation.

## Environment

- `MAX_LOG_BYTES` — cap on per-service log file size (default 1 MiB)
