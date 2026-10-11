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
4. Optionally run `tako config explain` to print inferred runtime, state,
   mesh, server, and service defaults without contacting servers.
5. Optionally run `tako doctor --skip-remote` to prove local build inputs
   without SSH.
6. Optionally patch stale server-side takod agents with `tako upgrade servers`.
7. Pull the newest reachable encrypted environment bundle from takod.
8. Pull remote deployment state into the local `.tako/` cache.
9. Run `tako deploy --yes`.

Remote leases in takod prevent a CI job and a laptop from reconciling the same
target nodes at the same time.

For operations that exceed the default lease TTL, the engine renews the same
holder ID in the background until the operation releases it. Renewal preserves
the original holder metadata and creation time; a different ID still fails fast
as locked. When an older takod does not recognize explicit renewal, the engine
keeps the original confirmed lease only long enough for normal runtime setup to
upgrade the agent; if renewal is still unavailable near expiry, the operation
is canceled before another holder can acquire it.

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
runner is not tied to whichever node answers first. It fails when no bundle has
been pushed, so CI never deploys with an empty environment by accident; pass
`--allow-missing` when the bundle is optional.

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

If the runner should build images instead of the VPS, set
`deployment.build.strategy: auto` or pass `tako deploy --build-strategy local`.
That path requires the runner to have Docker and buildx. Tako exports the exact
local image ID and imports it through the authenticated structured node
transport, so it does not require a Docker SSH plugin or separate remote-Docker
permissions. `auto` falls back to the remote takod builder when local Docker or
buildx is unavailable.

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
      TAKO_SSH_CONNECT_ATTEMPTS: "6"
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
          tako config explain -e production
          tako doctor -e production --skip-remote
          tako upgrade servers --dry-run
          tako upgrade servers  # no-op when takod already runs this version
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

In CI and non-interactive shells, Tako defaults to six SSH TCP attempts because
fresh firewall/security-group changes can take a short time to become usable.
Set `TAKO_SSH_CONNECT_ATTEMPTS` explicitly when you want a different budget.
The first retry follows after one second; later retries wait about 31-35
seconds so an SSH rate limiter such as `ufw limit 22/tcp` (six new connections
per 30 seconds, re-armed by every blocked attempt) can clear. `tako setup`
allows SSH with `ufw allow` on 22 and the server's configured `port` and relies
on fail2ban instead; re-run setup, or run `ufw allow <port>/tcp`, on hosts
provisioned with the older limit rule. Setup's own post-firewall SSH probe
uses a bounded budget (two attempts, 10 seconds each) and ignores
`TAKO_SSH_CONNECT_ATTEMPTS`, so a blocked port fails the step quickly.

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

If a node still exists and you are intentionally shrinking the environment,
clean up its services with `tako remove --server <node>` before removing its
SSH config from `tako.yaml`. After a node is no longer configured, Tako can
prune replicated state from reachable nodes, but it cannot stop containers on
that removed host because it no longer has connection details for it.

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

## Preview Environments per Pull Request

With a `preview:` template in `tako.yaml` (see
[PREVIEW-ENVIRONMENTS.md](./PREVIEW-ENVIRONMENTS.md)), one workflow deploys a
preview when a pull request is opened or updated and destroys it when the pull
request closes. A second, scheduled workflow prunes expired previews.

```yaml
name: Preview

on:
  pull_request:
    types: [opened, reopened, synchronize, closed]

concurrency:
  group: preview-${{ github.event.number }}
  cancel-in-progress: false

jobs:
  preview:
    runs-on: ubuntu-latest
    env:
      CI: "true"
      TAKO_NONINTERACTIVE: "1"
      TAKO_HOST_KEY_MODE: strict
      TAKO_ENV_PASSPHRASE: ${{ secrets.TAKO_ENV_PASSPHRASE }}
      TAKO_SSH_KEY: ~/.ssh/tako_deploy
      TAKO_SERVER_HOST: ${{ secrets.TAKO_SERVER_HOST }}

    steps:
      - uses: actions/checkout@v6

      - name: Install Tako
        run: |
          curl -fL https://github.com/redentordev/tako-cli/releases/latest/download/tako-linux-amd64 -o /usr/local/bin/tako
          chmod +x /usr/local/bin/tako

      - name: Restore SSH key and environment
        run: |
          mkdir -p ~/.ssh
          printf '%s\n' "${{ secrets.TAKO_SSH_PRIVATE_KEY }}" > "$TAKO_SSH_KEY"
          chmod 600 "$TAKO_SSH_KEY"
          tako env pull --force

      - name: Deploy preview
        if: github.event.action != 'closed'
        run: tako deploy --preview pr-${{ github.event.number }} --yes

      - name: Destroy preview
        if: github.event.action == 'closed'
        run: tako preview destroy pr-${{ github.event.number }} --yes
```

```yaml
name: Prune previews

on:
  schedule:
    - cron: "0 3 * * *"
  workflow_dispatch:

jobs:
  prune:
    runs-on: ubuntu-latest
    env:
      CI: "true"
      TAKO_NONINTERACTIVE: "1"
      TAKO_HOST_KEY_MODE: strict
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
      - name: Prune expired previews
        run: tako preview prune --yes
```

- The preview name `pr-<number>` is stable for the life of the pull request,
  so each push redeploys the same preview, keeps its host and certificate, and
  extends its expiry. The deploy's job summary shows the preview line, its
  URLs, and an undo command that uses `--preview`.
- The `concurrency` group serializes jobs for one pull request. Without it,
  a job that starts while another still holds the preview's lease exits 3.
- `tako preview destroy` exits 0 when the preview is already gone, so the
  closed job is safe to rerun.
- `tako preview prune` exits 6 when an expired preview could not be removed,
  which fails the scheduled job so someone looks at it. Previews that another
  job is deploying are skipped, not failed.
- GitHub does not pass repository secrets to `pull_request` workflows from
  forks. Only run previews for branches of the repository itself, and never
  switch to `pull_request_target` to work around that: it would run untrusted
  code with your deploy key.

## Lifecycle Hooks in CI

[Hooks](./HOOKS.md) in `.tako-hooks/` run on the CI runner, exactly as they
do on a laptop, so a `pre-deploy` test gate or a `post-deploy` notification
applies to every deploy path.

- **Executable bit from Windows.** Linux runners only run hook files with the
  executable bit, and Windows checkouts cannot set it. Record it in git when
  you add the hook:

  ```bash
  git add .tako-hooks/pre-deploy
  git update-index --chmod=+x .tako-hooks/pre-deploy
  git commit -m "Add pre-deploy hook"
  ```

  A hook without the bit fails the deploy with exit code 2 before any lock is
  taken. Run `tako validate` early in the job to catch it before deploying.
- **CI tokens through `passEnv`.** Hooks do not inherit the runner's
  environment. List the variables a hook needs, and set them from CI secrets:

  <!-- tako:snippet partial -->
  ```yaml
  hooks:
    passEnv: [GITHUB_TOKEN, SLACK_WEBHOOK_URL, GITHUB_SHA]
  ```

  ```yaml
      - name: Deploy
        env:
          GITHUB_TOKEN: ${{ secrets.GITHUB_TOKEN }}
          SLACK_WEBHOOK_URL: ${{ secrets.SLACK_WEBHOOK_URL }}
        run: tako deploy --yes
  ```

  Values of passed variables whose names look like credentials are redacted
  from hook output. `CI` is always passed, so scripts can detect the runner.
- **Exit codes.** A failing `pre-deploy` exits 1 with nothing deployed; a
  failing `post-deploy` exits 6 while the deploy itself stays successful. End
  best-effort notification commands with `|| true` if they must not fail the
  job.
- **Emergency deploys.** `tako deploy --skip-hooks --yes` (also on
  `rollback`, `promote`, and `setup`) runs no hook, not even `on-failure`, and
  ignores broken hook files. Each skipped hook still appears in the result as
  `skipped`.

## Job Step Summary

When `GITHUB_STEP_SUMMARY` is set, `tako deploy`, `rollback`, `promote` and
`scale` append a Markdown summary of the operation to that file, so it shows
on the workflow run page. It contains a status heading (succeeded, partial or
failed), the duration and deployment ID, a table of services with their
change, strategy, image, replicas and health, the URLs with their DNS/TLS
readiness, the undo command, next steps, and the error. It is written in
every output mode (`--output json` and `--events ndjson` keep stdout
machine-readable) and is capped at 64 KiB per operation.

- Gitea and Forgejo Actions runners set `GITHUB_STEP_SUMMARY` too, so the
  summary works there unchanged.
- Set `TAKO_STEP_SUMMARY=off` (or `0`, `false`, `no`) to disable it.
- A failure to write the file is a warning and never changes the exit code.
- Free text is redacted the same way as every other Tako output.

The same summary is the `summary` object of the result document. A pipeline
can read the undo command without parsing prose:

```bash
tako deploy --yes --output json > deploy.json || status=$?
jq -r '.summary.outcome' deploy.json
# argv form, safe to exec without a shell; null when no safe undo exists
jq -c '.summary.undo.argv // empty' deploy.json
jq -r '.summary.undo.unavailable // empty' deploy.json
```

The undo command never includes `--yes`; add it explicitly when a pipeline
runs it unattended.

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
