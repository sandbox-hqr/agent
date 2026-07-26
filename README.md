# sandbox-agent

`agent` is the host daemon that runs on every machine contributed to a
sandbox pool. One binary does two jobs:

- **as a daemon** (`agent`, or `agent run`) it enrolls with the control
  plane, sends heartbeats, long-polls for commands, boots/tears down
  microVMs and containers ("Spaces") through a pluggable driver, and
  maintains a reverse tunnel so the control plane can reach ports inside
  those Spaces;
- **as a CLI** (`agent cli <subcommand>`) it installs/manages itself as a
  native OS service (systemd, launchd, or a Windows Service) via
  [`kardianos/service`](https://github.com/kardianos/service).

It is a standalone Go module with no runtime dependency on the rest of the
`sandbox` monorepo — it only ever talks to the control plane (scheduler +
reverse tunnel broker) over the network.

## How it works

```
                 enroll / heartbeat / long-poll / results
agent  <───────────────────────────────────────────────────────>  control plane
  │                                                    (scheduler + RTB)
  ├─ heartbeat loop      reports Driver.List() + resource usage
  ├─ command loop        long-polls for create_space / delete_space, executes
  │                      idempotently, reports results
  ├─ tunnel loop         holds one persistent yamux session to RTB, serves
  │                      dial-back streams into local Space ports
  └─ local API (goose)   127.0.0.1:9090 by default — /healthz /readyz /metrics
```

The three loops fail independently — for example, losing the reverse
tunnel session degrades `tunnel_degraded` in `/readyz` but never stops
heartbeating or command execution. Each loop backs off independently on
failure (exponential backoff with jitter) and resets on success; an empty
long-poll response is treated as success, not a failure.

Local state (`agent.json`, node ID + bearer token + an idempotency log of
executed command IDs) is the *only* state the agent keeps — the control
plane's own store is the source of truth, and this is just enough to
survive the agent's own restart. A redelivered command is detected via the
idempotency log and re-reports success without re-executing.

## Install

### Download a release (recommended)

Prebuilt binaries for Linux (amd64/arm64), macOS (amd64/arm64), and
Windows (amd64) are published on the
[Releases page](https://github.com/sandbox-hqr/agent/releases) by this
repo's [release workflow](.github/workflows/release.yml). Download the
archive for your platform, extract it, and place `agent` (or `agent.exe`)
somewhere on your `PATH`.

### Build from source

Requires Go 1.25+.

```sh
git clone https://github.com/sandbox-hqr/agent.git
cd agent
go build -o agent .
```

## CLI reference

The binary's entire public interface is: no arguments (or `run`) starts
the daemon in the foreground; `cli` routes to one of the subcommands
below, which manage the daemon as a background OS service.

```
agent                          run the daemon in the foreground
agent run                      same as above (explicit form)

agent cli install --cloud <control-plane-url> --token <join-token> [--driver <driver>]
agent cli uninstall
agent cli start
agent cli stop
agent cli status
```

| Command | Description |
|---|---|
| `agent` / `agent run` | Runs the daemon directly in the current process (foreground, logs to stdout/stderr). This is what the installed OS service invokes under the hood — use it directly for local testing or containerized deployments where you manage the process lifecycle yourself. |
| `agent cli install` | One-shot operator install flow: persists the control-plane URL, join token, and driver choice to the agent's env file, then installs **and starts** the daemon as a native OS service (systemd unit on Linux, launchd plist on macOS, Windows Service on Windows — picked automatically by `kardianos/service`). Requires `--cloud` and `--token`; `--driver` defaults to `mock` (see [Drivers](#drivers)). Creates the state directory (`0700`) and env file (`0600`) if they don't exist. |
| `agent cli uninstall` | Stops (if running) and uninstalls the OS service. Does not delete the state directory or persisted credentials. |
| `agent cli start` | Starts the already-installed OS service. |
| `agent cli stop` | Stops the running OS service. |
| `agent cli status` | Queries the daemon's own local health API (`GET /readyz`) rather than just asking the OS service manager whether the process is alive — so it also reports enrollment and tunnel state, not just "process is running." Exits non-zero if the daemon is unreachable or not ready. |

`agent cli install`'s flags:

| Flag | Required | Description |
|---|---|---|
| `--cloud` | yes | Control-plane base URL (e.g. `https://cloud.example.com`). Also used as the reverse-tunnel broker URL unless `SANDBOX_AGENT_RTB_URL` is set separately. |
| `--token` | yes | One-time join token issued by the control plane when the Node was created. |
| `--driver` | no (default `mock`) | Which [driver](#drivers) the daemon uses to actually create/delete Spaces: `mock`, `firecracker`, `cloud-hypervisor`, or `container`. |

## Local HTTP API

The daemon exposes a small HTTP API, bound to `127.0.0.1:9090` by default
(`SANDBOX_AGENT_LOCAL_API_ADDR`) — this is the only inbound network surface
the agent has; everything else is outbound to the control plane.

| Method | Path | Description |
|---|---|---|
| `GET` | `/healthz` | Process-alive check. Always `200 {"status":"ok"}` if the process is up and serving. |
| `GET` | `/readyz` | `200` with `{"enrolled": bool, "tunnel_degraded": bool}` once enrolled; `503` with the same body otherwise. A degraded tunnel does **not** fail readiness — losing the reverse tunnel shouldn't be treated as "the whole agent is unready." |
| `GET` | `/metrics` | Prometheus text-format metrics: `agent_tunnel_session_state` (0=down, 1=connecting, 2=up) and `agent_tunnel_streams_active` (open dial-back streams). |

## Configuration

The daemon reads configuration from environment variables, falling back to
values persisted in the env file `agent cli install` writes
(`$SANDBOX_AGENT_STATE_DIR/agent.env`; a real env var always wins over the
file). Most of these only matter to the daemon (`agent`/`agent run`); the
CLI subcommands only care about `SANDBOX_AGENT_STATE_DIR` (where
`agent.json`/`agent.env` live) and, for `agent cli status`, `SANDBOX_AGENT_LOCAL_API_ADDR`
(which local API endpoint to query).

| Variable | Default | Description |
|---|---|---|
| `SANDBOX_AGENT_CONTROL_PLANE_URL` | *(none — required)* | Base URL of the control plane (scheduler). The daemon refuses to start without it. |
| `SANDBOX_AGENT_JOIN_TOKEN` | *(none)* | One-time enrollment token. Only needed until the agent has enrolled once; ignored after (`agent.json` already has credentials). |
| `SANDBOX_AGENT_RTB_URL` | value of `SANDBOX_AGENT_CONTROL_PLANE_URL` | Base URL of the reverse tunnel broker, if it's served from a different origin than the control plane. |
| `SANDBOX_AGENT_DRIVER` | `mock` | Which [driver](#drivers) backs Space create/delete: `mock`, `firecracker`, `cloud-hypervisor`, or `container`. |
| `SANDBOX_AGENT_HEARTBEAT_INTERVAL` | `15s` | Interval between heartbeats. Accepts a bare integer (seconds) or a Go duration string (`30s`, `1m`). |
| `SANDBOX_AGENT_LONGPOLL_TIMEOUT` | `30s` | How long the command long-poll blocks server-side before returning empty. |
| `SANDBOX_AGENT_RETRY_BACKOFF_MIN` | `1s` | Floor of the exponential backoff applied independently to each loop (enroll, heartbeat, long-poll, tunnel reconnect) after a failure. |
| `SANDBOX_AGENT_RETRY_BACKOFF_MAX` | `30s` | Ceiling of that backoff. |
| `SANDBOX_AGENT_LOCAL_API_ADDR` | `127.0.0.1:9090` | Bind address for the [local HTTP API](#local-http-api). |
| `SANDBOX_AGENT_LOCAL_API_PPROF` | `false` | Reserved for enabling pprof profiling on the local API; not yet wired to an actual route. |
| `SANDBOX_AGENT_STATE_DIR` | `/var/lib/sandbox-agent` (Linux/macOS) or `%ProgramData%\SandboxAgent` (Windows) | Where `agent.json` (enrollment + idempotency log) and `agent.env` (install-time config) are stored. |

### Driver-specific variables

**`firecracker`**

| Variable | Default | Description |
|---|---|---|
| `SANDBOX_AGENT_FIRECRACKER_BIN` | `firecracker` | Path to the `firecracker` binary. |
| `SANDBOX_AGENT_FIRECRACKER_KERNEL` | *(none — required)* | Path to the kernel image booted into every Space. |
| `SANDBOX_AGENT_FIRECRACKER_ROOTFS` | *(none — required)* | Path to the template rootfs image, flat-copied per instance. |
| `SANDBOX_AGENT_FIRECRACKER_STATE_DIR` | `/var/lib/sandbox-agent/firecracker` | Per-instance working directories (API socket, copied rootfs, logs, pid file). |

Requires a Linux host with `/dev/kvm` available. GPU requests are always
rejected (no PCI passthrough), and `type: container` Spaces are rejected
(Firecracker only boots microVMs).

**`cloud-hypervisor`**

The GPU-workload driver (VFIO passthrough). Not implemented yet — every
method returns an error; needs a Linux host with an IOMMU-capable GPU to
build against. `Supports` still reports `true` for VM-type Spaces so the
scheduler can route to a Node advertising this driver, but `Create` will
fail.

**`container`**

| Variable | Default | Description |
|---|---|---|
| `SANDBOX_AGENT_RUNC_BIN` | `runc` | Path to the `runc` binary. |
| `SANDBOX_AGENT_CONTAINER_BUSYBOX` | `/bin/busybox` | Busybox binary staged into every container's rootfs; also serves a static page via `busybox httpd` so exported ports have something to answer with. |
| `SANDBOX_AGENT_CONTAINER_STATE_DIR` | `/var/lib/sandbox-agent/container` | Per-instance OCI bundle directories. |
| `SANDBOX_AGENT_CONTAINER_HOST_NETWORK` | `false` | When `true`, drops the container's network-namespace isolation so its listening port is reachable at `127.0.0.1` on the host. Off by default because it's a real isolation trade-off, not free — there's no tap/veth/bridge networking implemented for containers otherwise, so without this a container's ports are unreachable from the host at all. |
| `SANDBOX_AGENT_CONTAINER_APP_PORT` | `8091` | Port the staged busybox httpd listens on inside the container. |

Only accepts Spaces with `type: "container"`; GPU requests are rejected
(no GPU-aware container runtime wired up).

**`mock`**

No configuration. Fakes a Space with a loopback TCP listener that accepts
and immediately closes connections — enough to prove reachability without
KVM or `runc`, and what the agent's own tests run against. Accepts any
Space.

## Development

```sh
go build ./...     # compile everything
go vet ./...        # static checks
go build -o agent . # build the binary locally
```

## Releasing

Pushing a tag matching `v*.*.*` (e.g. `v0.1.0`) triggers
[`.github/workflows/release.yml`](.github/workflows/release.yml), which
cross-compiles the CLI for linux/amd64, linux/arm64, darwin/amd64,
darwin/arm64, and windows/amd64, packages each as a `.tar.gz` (`.zip` on
Windows), and publishes them as assets on a GitHub Release for that tag
(along with a `checksums.txt`). It can also be run manually from the
Actions tab (`workflow_dispatch`) against an existing tag.

```sh
git tag v0.1.0
git push origin v0.1.0
```
