# Meshed takod E2E Checklist

Use this checklist to prove the single runtime path before treating the mesh
migration as complete. The same commands should work for one node, two nodes,
and CI runners.

## Test Matrix

```text
Case                 Goal
-------------------  ---------------------------------------------------------
One node             A one-node mesh can setup, deploy, reconcile, and recover.
Two nodes            State, env bundles, desired state, and actual state agree.
Offline node         Reachable nodes keep serving and the CLI reports the gap.
New computer         A clean checkout can pull state/env and deploy.
CI runner            Automation uses the same takod path and remote leases.
State repair         Divergent reachable nodes can be repaired from freshest state.
Agent upgrade        Stale takod agents can be patched and verified explicitly.
Proxy protocols      HTTP/1.1, HTTP/2, HTTP/3, WebSocket, and sticky routes work.
Invalid config       Invalid YAML fails at deploy preflight before remote work.
```

## Prerequisites

- A repo with `tako.yaml` and at least one proxied service.
- SSH access to each configured server.
- Host keys accepted locally, or `TAKO_HOST_KEY_MODE=strict` with known hosts
  preinstalled.
- `TAKO_ENV_PASSPHRASE` set when testing env bundle push/pull.

## Runnable Harness

The manual flows below are captured in `scripts/mesh-e2e.sh`. The script builds
the current CLI by default, runs commands from a real app repository, writes
logs outside the app worktree unless `TAKO_E2E_LOG_DIR` is set, and uses
temporary fresh clones for new-computer and CI checks.

This harness is not part of the repository's default CI suite. Normal Tako CLI
CI is self-contained and does not contact servers. Run the harness separately
when a change needs live proof against owned nodes, WireGuard routing, proxy
protocols, leases, or replicated takod state.

Safe preflight:

```bash
scripts/mesh-e2e.sh --app-dir /path/to/app --env production
```

Public protocol proof against a deployed service:

```bash
scripts/mesh-e2e.sh \
  --app-dir /path/to/app \
  --env production \
  --phases protocols \
  --protocol-url https://app.example.com/health \
  --websocket-url wss://app.example.com/socket
```

Standard mutating proof, including an invalid-config preflight check:

```bash
TAKO_ENV_PASSPHRASE=... \
scripts/mesh-e2e.sh --app-dir /path/to/app --env production --phases standard --yes
```

Full proof including two-node repair and an automated offline-node run:

```bash
TAKO_ENV_PASSPHRASE=... \
scripts/mesh-e2e.sh \
  --app-dir /path/to/app \
  --env production \
  --phases full \
  --offline-server node-b \
  --yes
```

You can also run the harness through Make:

```bash
make mesh-e2e APP_DIR=/path/to/app ENV=production PHASES=standard ARGS=--yes
```

Mutating phases refuse to run unless `--yes` or `TAKO_E2E_CONFIRM=run` is set.
Deploy and env phases also fail early unless `.tako/` and `.env` are ignored
and the app worktree is clean, matching `tako deploy`'s clean-check behavior.
The env phase backs up the local `.env`, verifies that `env pull --force`
restores the same content, and restores the original file if the check fails.
The CI phase defaults to `TAKO_E2E_CI_HOST_KEY_MODE=tofu`; set it to `strict`
when validating a runner image with preinstalled known hosts.

The invalid-config phase creates a temporary app directory with malformed
`tako.yaml`, runs `tako deploy --yes`, and verifies the command exits with a
config preflight error before deployment startup, locks, leases, SSH pools, or
runtime reconciliation can begin.

Mutating phases run `tako upgrade servers` so the proof covers stale-agent
patching before deploy. If the harness is testing a local source-tree build
instead of an already released binary, pass a Linux server binary with
`--takod-binary` or `TAKO_E2E_TAKOD_BINARY`; otherwise there is no release asset
for the server to download.

The offline phase stops `takod` on the selected node through SSH, verifies that
status degrades and drift/deploy fail closed while the node agent is
unavailable, restarts `takod`, runs repair, and deploys again. If the phase
fails after stopping the agent, the harness tries to restart it from the exit
trap. Override `TAKO_E2E_OFFLINE_STOP_CMD`,
`TAKO_E2E_OFFLINE_START_CMD`, or `TAKO_E2E_OFFLINE_STATUS_CMD` if your service
manager is not systemd. The harness infers offline-node host, user, port, and
SSH key from `tako.yaml`; use `--offline-host`, `--offline-user`,
`--offline-port`, or `--offline-ssh-key` only when you need to override config.

## One-Node Flow

```bash
tako setup -e production
tako upgrade servers -e production --dry-run
tako upgrade servers -e production
tako deploy -e production --yes
tako doctor -e production
tako state status -e production
tako history -e production
tako ps -e production
tako drift -e production
```

Expected result:

- `setup` installs/refreshes Docker, WireGuard, takod, firewall rules, and host
  hardening.
- `upgrade servers` reports and patches stale takod agents, then verifies
  `/v1/status` reports the CLI version.
- `deploy` reconciles services and the shared `tako-proxy` through takod, not a
  local Docker path.
- `doctor` reports matching server-side takod agent versions, rootful remote
  Docker, `Proxy Runtime` readiness for public services, and `Replicated State`
  health, including deployment history, desired runtime, aggregate actual
  state, node-local actual snapshots, and a free remote operation lease.
- `state status` shows one reachable node with deployment history, desired
  state, aggregate actual state, node-local actual state, agent status, mesh
  status, and no state disagreement.
- `history`, `ps`, and `drift` match the deployed revision.

## Two-Node Flow

Add a second server to the same environment, then run:

```bash
tako setup -e production
tako upgrade servers -e production --dry-run
tako upgrade servers -e production
tako state repair -e production
tako state status -e production
tako deploy -e production --yes
tako state status -e production
```

Expected result:

- Both nodes have takod running.
- `upgrade servers` verifies every reachable node is running the CLI-matched
  `takod` agent before state repair and deploy.
- WireGuard mesh status lists both peers.
- Deployment history and desired state are available from both reachable nodes.
- Actual state is reported per node and as an aggregate.
- Proxy upstreams include healthy local and mesh-reachable service instances.
- Host firewall rules allow the WireGuard listen UDP port and routed traffic to
  peer mesh /32 addresses on the Tako interface.
- IPv4 forwarding is enabled live and persisted so mesh-to-Docker routing
  survives host reboot.

## Proxy Protocol Flow

First verify the node-local proxy shape:

```bash
tako doctor -e production
```

The `Proxy Runtime` section should pass for every proxy target before external
protocol checks. It verifies the live container settings and UDP 443 publish,
even when the local test client cannot negotiate HTTP/3.

Run this against a public proxied service:

```bash
scripts/mesh-e2e.sh \
  --app-dir /path/to/app \
  --env production \
  --phases protocols \
  --protocol-url https://<domain>/health
```

The phase writes response headers and bodies to the harness log directory. It
always verifies HTTP/1.1, HTTP/2, and HTTP/3 `Alt-Svc` advertisement. If the
local `curl` supports `--http3`, it also makes an HTTP/3 request. Set
`TAKO_E2E_HTTP3_REQUIRED=1` or pass `--http3-required` when a test runner must
fail instead of skipping an HTTP/3 wire request.

For WebSocket services, include the WebSocket URL:

```bash
scripts/mesh-e2e.sh \
  --app-dir /path/to/app \
  --env production \
  --phases protocols \
  --protocol-url https://<domain>/health \
  --websocket-url wss://<domain>/socket
```

For sticky replica behavior, inspect the saved protocol response bodies or run
several requests with and without the sticky cookie.

Expected result:

- HTTP/1.1 and HTTP/2 requests succeed through `tako-proxy`.
- UDP 443 is allowed for HTTP/3 and HTTP/3 succeeds when client/server support
  is available.
- WebSocket upgrade traffic passes through the same proxy.
- Sticky services keep session-affine traffic on the same replica while the
  sticky cookie is present.

## Env Bundle Flow

```bash
tako env push -e production
rm -f .env
tako env pull -e production --force
```

Expected result:

- `env push` writes the encrypted bundle to every reachable environment node.
- `env pull` restores the newest bundle from reachable nodes.
- Unsupported bundle paths are skipped instead of written outside the project.

## New-Computer Flow

From a clean checkout with no `.tako/` directory:

```bash
git clone <repo>
cd <repo>
tako validate -e production
tako doctor -e production --skip-remote
tako env pull -e production --force
tako state pull -e production
tako state status -e production
tako state lease -e production
tako deploy -e production --yes
```

Expected result:

- Environment files and local `.tako/` state are rebuilt from the freshest
  reachable mesh state.
- Local build contexts, Dockerfiles, or Nixpacks inputs are checked before SSH
  and deploy work.
- Deploy does not depend on deployment history from the previous laptop.
- Remote leases prevent concurrent laptop and CI mutations.

## Offline-Node Flow

Run the harness against one non-critical node:

```bash
TAKO_ENV_PASSPHRASE=... \
scripts/mesh-e2e.sh \
  --app-dir /path/to/app \
  --env production \
  --phases offline \
  --offline-server node-b \
  --yes
```

Expected result:

- `state status` clearly reports the unavailable node agent and still shows best
  known state from reachable nodes.
- `drift` and `deploy` fail closed while the target node agent is unavailable.
- Deploy behavior follows `state.onUnreachableNode`; the default posture is to
  block when a target node is unreachable.
- `TAKO_SSH_CONNECT_TIMEOUT` and `TAKO_SSH_CONNECT_ATTEMPTS` can be lowered for
  incident checks so destroyed nodes fail quickly without changing the default
  fail-closed deploy posture.
- Existing containers on the unreachable node keep serving their last accepted
  revision.
- After a destroyed node is removed from the active environment config,
  `tako state forget-node <node> --yes` prunes its replicated node snapshot from
  reachable nodes before the next deploy.
- After `takod` restarts, `state repair` and `deploy` succeed again.

For a destroyed node rebuilt with the same logical node name, keep the node in
`tako.yaml` and repair it through the normal mesh path:

```bash
tako setup --server node-b -e production
tako upgrade servers --server node-b -e production
tako state repair -e production
tako deploy -e production --yes
```

For a permanently retired node, remove it from `tako.yaml`, run
`tako state forget-node node-b --yes -e production`, then deploy so proxy routes
and replicated runtime state no longer reference that node.

## State Repair Flow

Run this after replacing a node, restoring from backup, or finding divergent
mesh state:

```bash
tako state status -e production
tako state repair -e production
tako state status -e production
```

Expected result:

- Repair selects the freshest reachable deployment history.
- Desired state, aggregate actual state, and node-local actual snapshots are
  written back to reachable nodes.
- Local `.tako/` deployment history is refreshed when remote history exists.

## CI Flow

CI should run the same steps as a fresh laptop:

```bash
tako upgrade servers --dry-run
tako upgrade servers
tako env pull -e production --force
tako state pull -e production
tako state status -e production
tako state lease -e production
tako deploy -e production --yes
```

Expected result:

- The runner has no dependency on a persisted `.tako/` workspace.
- Stale takod agents are patched before app deployment.
- `TAKO_HOST_KEY_MODE=strict` works after known hosts are installed.
- Remote leases reject overlapping deploy, rollback, scale, maintenance, live,
  remove, cleanup, destroy, and repair operations.
- `tako state lease` shows held lease IDs, and
  `tako state lease release --id <id> --force` releases only the exact stale ID
  when a runner or laptop is interrupted.

## Completion Bar

The migration is proven only when the checklist passes against real servers and
the following local gates pass:

```bash
make ci-check
```
