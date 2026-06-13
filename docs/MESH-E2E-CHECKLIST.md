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
```

## Prerequisites

- A repo with `tako.yaml` and at least one proxied service.
- SSH access to each configured server.
- Host keys accepted locally, or `TAKO_HOST_KEY_MODE=strict` with known hosts
  preinstalled.
- `TAKO_ENV_PASSPHRASE` set when testing env bundle push/pull.

## One-Node Flow

```bash
tako setup -e production
tako deploy -e production --yes
tako state status -e production
tako history -e production
tako ps -e production
tako drift -e production
```

Expected result:

- `setup` installs/refreshes Docker, WireGuard, takod, proxy, and firewall rules.
- `deploy` reconciles through takod, not a local Docker path.
- `state status` shows one reachable node with deployment history, desired
  state, aggregate actual state, node-local actual state, agent status, mesh
  status, and no state disagreement.
- `history`, `ps`, and `drift` match the deployed revision.

## Two-Node Flow

Add a second server to the same environment, then run:

```bash
tako setup -e production
tako state repair -e production
tako state status -e production
tako deploy -e production --yes
tako state status -e production
```

Expected result:

- Both nodes have takod running.
- WireGuard mesh status lists both peers.
- Deployment history and desired state are available from both reachable nodes.
- Actual state is reported per node and as an aggregate.
- Proxy upstreams include healthy local and mesh-reachable service instances.

## Env Bundle Flow

```bash
tako env push -e production
rm -f .env
tako env pull -e production --force
```

Expected result:

- `env push` writes the encrypted bundle to every reachable environment node.
- `env pull` can restore from any node that has the bundle.
- Unsupported bundle paths are skipped instead of written outside the project.

## New-Computer Flow

From a clean checkout with no `.tako/` directory:

```bash
git clone <repo>
cd <repo>
tako env pull -e production --force
tako state pull -e production
tako state status -e production
tako deploy -e production --yes
```

Expected result:

- Local `.tako/` state is rebuilt from the freshest reachable mesh state.
- Deploy does not depend on deployment history from the previous laptop.
- Remote leases prevent concurrent laptop and CI mutations.

## Offline-Node Flow

Temporarily stop SSH or takod on one non-critical node, then run:

```bash
tako state status -e production
tako drift -e production
tako deploy -e production --yes
```

Expected result:

- `state status` clearly reports the unreachable node and still shows best
  known state from reachable nodes.
- `drift` reports actual state from reachable nodes.
- Deploy behavior follows `state.onUnreachableNode`; the default robust posture
  is to block when a target node is unreachable.
- Existing containers on the unreachable node keep serving their last accepted
  revision.

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
tako env pull -e production --force
tako state pull -e production
tako state status -e production
tako deploy -e production --yes
```

Expected result:

- The runner has no dependency on a persisted `.tako/` workspace.
- `TAKO_HOST_KEY_MODE=strict` works after known hosts are installed.
- Remote leases reject overlapping deploy, rollback, scale, destroy, and repair
  operations.

## Completion Bar

The migration is proven only when the checklist passes against real servers and
the following local gates pass:

```bash
go test ./...
go test -race ./cmd ./internal/state ./pkg/mesh ./pkg/provisioner ./pkg/secrets ./pkg/deployer ./pkg/ssh ./pkg/takodclient ./pkg/takod ./pkg/takodstate ./pkg/config
go build ./...
go vet ./...
git diff --check
```
