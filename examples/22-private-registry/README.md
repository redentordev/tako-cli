# Private Registry Example

**Deploy private images** — declare registry credentials once in a
`registries:` block and every pull and build authenticates with them.

## What This Example Does

✓ Deploys a private GHCR image to your VPS
✓ Credentials come from `.env` — never written into `tako.yaml`
✓ Credentials are request-scoped: sent with each deploy, used for the one
  pull/build inside an ephemeral Docker config, then discarded — nothing
  is persisted on the server

## Quick Start

```bash
cp .env.example .env
# edit .env with your server IP and registry credentials
tako deploy
```

## How It Works

- `registries:` is keyed by registry host (`ghcr.io`,
  `123456789.dkr.ecr.us-east-1.amazonaws.com`, `docker.io`, ...).
- Passwords must be `${ENV_VAR}` references. Config validation rejects
  literal secrets before the file is even parsed for deployment.
- Docker Hub aliases (`docker.io`, `index.docker.io`) normalize to the
  canonical auth key automatically.
- Wrong or expired credentials fail with a distinct
  `registry authentication failed` error (and an `image.pull.auth_failed`
  event in `--events ndjson`), so you know to rotate the token rather
  than debug a missing image.
- Cloud registries can mint short-lived tokens instead of a static
  password: set `provider` and drop `username`/`password`. Tako runs the
  provider CLI on your machine before the deploy changes anything, keeps
  the token in memory only, and masks it in all output:

```yaml
registries:
  123456789012.dkr.ecr.us-east-1.amazonaws.com:
    provider: ecr                # aws ecr get-login-password --region us-east-1
  us-docker.pkg.dev:
    provider: artifact-registry  # gcloud auth print-access-token
  ghcr.io:
    provider: ghcr               # GH_TOKEN, GITHUB_TOKEN, or gh auth token
```

  No wrapper script exporting `ECR_PASSWORD` is needed. See
  [docs/CONFIGURATION.md](../../docs/CONFIGURATION.md#private-registries)
  for the host rules and provider options.
- One-off deploys without a config file work too:

```bash
echo "$REGISTRY_TOKEN" | tako run ghcr.io/acme/app:latest \
  --name app --port 3000 --server YOUR_SERVER_IP \
  --registry-user acme --registry-password-stdin
```
