# CI/CD Deployments

## Tako Project CI vs App Deployment CI

The CI suite for the Tako CLI repository is self-contained. Pull requests and
pushes run Go tests, builds, vet, formatting checks, shell syntax checks, and
race tests on GitHub-hosted runners. Those checks do not SSH to playground
servers, do not need deployment secrets, and do not mutate any real hosts.

Server-backed deployment proof is intentionally separate. Use the mesh E2E
harness from a real app repository when you want to prove setup, deploy, remote
state, leases, proxy protocols, or node-recovery behavior against actual
servers. That harness is opt-in and requires the app's normal SSH/env secrets.

Tako uses the same deployment path from laptops and CI runners:

1. Checkout the app repository.
2. Restore SSH credentials.
3. Validate `tako.yaml` with `tako validate`.
4. Optionally run `tako doctor --skip-remote` to prove local build inputs
   without SSH.
5. Optionally patch stale server-side takod agents with `tako upgrade servers`.
6. Pull the newest reachable encrypted environment bundle from takod.
7. Pull remote deployment state into the local `.tako/` cache.
8. Run `tako deploy --yes`.

Remote leases in takod prevent a CI job and a laptop from reconciling the same
target nodes at the same time.

If a runner is cancelled while holding a lease, inspect it with
`tako state lease`. Release only the exact stale ID shown by the mesh:

```bash
tako state lease release --id <lease-id> --force
```

The release command matches the current remote ID before deleting anything, so
it will not clear a newer lease that replaced the stale one.

`tako deploy` reads Git metadata for deployment history and rollback context,
but it does not create commits. CI should deploy a clean checkout; if generated
files or dependency installers modify the worktree before deploy, commit or
discard those changes earlier in the pipeline.

Build-backed services are tagged with the clean checkout commit hash. A CI
deploy does not require a `project.version` bump; the Git commit is the deploy
artifact identity. Use `tako deploy --force --yes` only when you intentionally
need to recreate unchanged app services. Broad force skips services marked
`persistent: true`; targeted `tako deploy --service <name> --force --yes`
is the explicit path for stateful containers that keep data in declared
volumes.

Each installed takod refreshes its own actual container snapshot in the
background. CI still runs `tako state pull` for deployment history and local UX,
but it does not depend on the runner's old `.tako/` directory to know what is
currently running.

`tako env pull` selects the newest bundle from reachable mesh nodes, so a fresh
runner is not tied to whichever node answers first.

`tako state repair` is the recovery path for stale or divergent node state. It
is not required on every CI deploy; run it before deploy when a node was
replaced, a runner is bootstrapping after state loss, or `tako state status`
shows that reachable nodes disagree on deployment history, desired runtime
state, or actual runtime state.

`tako doctor` is the quick diagnostic when a runner can reach the nodes but the
state picture is unclear. Its server-agent check reports stale or mismatched
takod versions, and its `Replicated State` section reports deployment history,
desired runtime, aggregate actual runtime, node-local actual snapshots, and the
current remote operation lease from each reachable takod node.

## Required Secrets

Store these in your CI provider:

- `TAKO_SSH_PRIVATE_KEY`: private key that can SSH to the configured nodes.
- `TAKO_ENV_PASSPHRASE`: passphrase used by `tako env push`.

Your `tako.yaml` can reference CI-provided paths through environment expansion:

```yaml
servers:
  prod:
    host: ${TAKO_SERVER_HOST}
    user: deploy
    sshKey: ${TAKO_SSH_KEY}
```

## GitHub Actions

The direct binary install below is the simplest runner path. Tako releases also
publish `ghcr.io/redentordev/tako-cli:<version>` and `:latest` as multi-arch
Linux images for AMD64 and ARM64 when you prefer a containerized CLI. Mount the
checkout and SSH material into that container before running `tako validate`,
`tako doctor --skip-remote`, `tako upgrade servers`, or `tako deploy`. A Docker
socket is only needed for custom local Docker steps outside the normal takod
deploy path.

```yaml
name: Deploy

on:
  push:
    branches: [main]

jobs:
  deploy:
    runs-on: ubuntu-latest
    env:
      CI: "true"
      TAKO_NONINTERACTIVE: "1"
      TAKO_HOST_KEY_MODE: strict
      TAKO_SSH_CONNECT_TIMEOUT: 10s
      TAKO_SSH_CONNECT_ATTEMPTS: "3"
      TAKO_ENV_PASSPHRASE: ${{ secrets.TAKO_ENV_PASSPHRASE }}
      TAKO_SSH_KEY: ~/.ssh/tako_deploy
      TAKO_SERVER_HOST: ${{ secrets.TAKO_SERVER_HOST }}

    steps:
      - uses: actions/checkout@v6

      - name: Install Tako
        run: |
          curl -fL https://github.com/redentordev/tako-cli/releases/latest/download/tako-linux-amd64 -o /usr/local/bin/tako
          chmod +x /usr/local/bin/tako

      - name: Restore SSH key
        run: |
          mkdir -p ~/.ssh
          printf '%s\n' "${{ secrets.TAKO_SSH_PRIVATE_KEY }}" > "$TAKO_SSH_KEY"
          chmod 600 "$TAKO_SSH_KEY"

      - name: Restore environment and state
        run: |
          tako validate -e production
          tako doctor -e production --skip-remote
          tako upgrade servers --dry-run
          tako upgrade servers
          tako env pull --force
          tako state pull
          tako state status
          tako state lease

      - name: Deploy
        run: tako deploy --yes
```

Use `TAKO_HOST_KEY_MODE=strict` when the runner image already has trusted host
keys. For first-time automation, run one manual bootstrap with `tofu` or install
known hosts before switching CI to `strict`.

When a node is destroyed or firewalled, state and deploy commands still fail
closed, but SSH should not hang for minutes. Use `TAKO_SSH_CONNECT_TIMEOUT` and
`TAKO_SSH_CONNECT_ATTEMPTS` to tune incident behavior; for example,
`TAKO_SSH_CONNECT_TIMEOUT=8s TAKO_SSH_CONNECT_ATTEMPTS=1 tako state status`
quickly proves which configured node is unreachable before running
`tako state repair` against the surviving mesh. After the destroyed node is
removed from `tako.yaml`, run `tako state forget-node <node> --yes` to prune
its replicated actual snapshot from reachable nodes before the next deploy. The
next deploy or state repair also rewrites the current target-node runtime state
and prunes stale per-node actual snapshots for removed nodes.

When a destroyed node is rebuilt with the same logical node name, keep it in
`tako.yaml`, repair the node, and deploy through the normal mesh path:

```bash
tako setup --server <node>
tako upgrade servers --server <node>
tako state repair
tako deploy --yes
```

That sequence recreates server setup, verifies the matching `takod` agent,
rewrites replicated state from reachable nodes, and reconciles proxy routes,
WireGuard state, remote leases, and live containers.

If `tako doctor` or a CI deploy reports that the server agent is stale or
missing features from the current CLI, run `tako upgrade servers --dry-run` to
see each node's agent version, then `tako upgrade servers` to install the
matching release binary, restart `takod`, refresh `/etc/tako/version.json`, and
verify `/v1/status` before deploying again. Development CLI builds must pass
`--takod-binary` with a Linux binary because there is no release asset for
`dev`.

## Proving the Workflow

Before treating CI deploys as reliable, run the same flow from a temporary fresh
clone with the mesh E2E harness:

```bash
TAKO_ENV_PASSPHRASE=... \
scripts/mesh-e2e.sh --app-dir /path/to/app --env production --phases ci --yes
```

The harness CI phase defaults to `TAKO_E2E_CI_HOST_KEY_MODE=tofu` so first-run
automation can prove the flow. Set `TAKO_E2E_CI_HOST_KEY_MODE=strict` when you
want the proof to match a runner with preinstalled known hosts.

For the fuller laptop and CI proof, use:

```bash
TAKO_ENV_PASSPHRASE=... \
scripts/mesh-e2e.sh --app-dir /path/to/app --env production --phases standard --yes
```
