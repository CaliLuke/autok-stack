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

## Config — `stack.toml`

All non-absolute paths are resolved relative to the directory containing `stack.toml`. `log_file` is always relative to `log_dir`.

```toml
[stack]
title = "MY STACK"                          # shown in the TUI hero, default "STACK"
log_dir = ".tmp/dev-stack"                  # default ".tmp/dev-stack"
required_tools = ["go", "bun", "podman"]    # checked on startup; missing tool = fatal
compose_command = ["podman", "compose"]     # optional, defaults to podman compose
cleanup_patterns = [                        # `pkill -f` patterns run before services start
  "scripts/log-server.ts",
]

# A plain long-running process:
[[service]]
key = "server"
name = "Server"
ports = ["8000"]                            # ports it should bind; checked for readiness + freed before start
work_dir = "backend"
log_file = "server.log"
command = ["./dev.sh"]
readiness_timeout = "5s"                    # optional, default 5s
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
