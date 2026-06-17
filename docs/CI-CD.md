# CI/CD Deployments

Tako uses the same deployment path from laptops and CI runners:

1. Checkout the app repository.
2. Restore SSH credentials.
3. Optionally patch stale server-side takod agents with `tako upgrade servers`.
4. Pull the newest reachable encrypted environment bundle from takod.
5. Pull remote deployment state into the local `.tako/` cache.
6. Run `tako deploy --yes`.

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
`tako state repair` against the surviving mesh.

If a CI deploy fails because the server agent is stale or missing features from
the current CLI, run `tako upgrade servers --dry-run` to see each node's agent
version, then `tako upgrade servers` to install the matching release binary,
restart `takod`, refresh `/etc/tako/version.json`, and verify `/v1/status`
before deploying again. Development CLI builds must pass `--takod-binary` with a
Linux binary because there is no release asset for `dev`.

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
