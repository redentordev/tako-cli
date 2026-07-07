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
- One-off deploys without a config file work too:

```bash
echo "$REGISTRY_TOKEN" | tako run ghcr.io/acme/app:latest \
  --name app --port 3000 --server YOUR_SERVER_IP \
  --registry-user acme --registry-password-stdin
```
