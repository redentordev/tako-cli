# CI/CD Deployments

Tako uses the same deployment path from laptops and CI runners:

1. Checkout the app repository.
2. Restore SSH credentials.
3. Pull encrypted environment files from takod.
4. Pull remote deployment state into the local `.tako/` cache.
5. Run `tako deploy --yes`.

Remote leases in takod prevent a CI job and a laptop from reconciling the same
target nodes at the same time.

Each installed takod refreshes its own actual container snapshot in the
background. CI still runs `tako state pull` for deployment history and local UX,
but it does not depend on the runner's old `.tako/` directory to know what is
currently running.

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
      TAKO_ENV_PASSPHRASE: ${{ secrets.TAKO_ENV_PASSPHRASE }}
      TAKO_SSH_KEY: ~/.ssh/tako_deploy
      TAKO_SERVER_HOST: ${{ secrets.TAKO_SERVER_HOST }}

    steps:
      - uses: actions/checkout@v4

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
          tako env pull --force
          tako state pull
          tako state status

      - name: Deploy
        run: tako deploy --yes
```

Use `TAKO_HOST_KEY_MODE=strict` when the runner image already has trusted host
keys. For first-time automation, run one manual bootstrap with `tofu` or install
known hosts before switching CI to `strict`.
