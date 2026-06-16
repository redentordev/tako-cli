# Next Admin Renderer Mongo

This fixture mirrors a Jardin-style CMS deployment without private code.

```text
app/
  mongo
  admin       # Dockerfile
  renderer    # Dockerfile.renderer
  edge-local  # checked-in Caddyfile behind shared tako-proxy

edge/
  edge        # generated Caddyfile, direct 80/443 on a dedicated edge node
```

Use `app/tako.yaml` when the CMS and edge Caddy can share one app/stage
network. Use `edge/tako.yaml` when public dynamic domains need a separate edge
node that imports the shared `admin` and `renderer` default ports over the mesh.

The renderer gets `ADMIN_URL` from a local service link to `admin`, so the app
config does not hard-code internal URLs. The app services use `share: true` for
the dedicated edge project. Mongo stays private and uses a node-local volume.
The runtime token is passed through environment state and is never printed by
the fixture endpoints.

The app fixture sets `deployment.source: git` to prove the committed-source
build path for the two Dockerfiles from one repository root.

## Env And Fresh Checkout Flow

The app config reads service runtime values from `JARDIN_ENV_FILE`. For local
template validation this points at `.env.example`; for real deploys use an
uncommitted stage file:

```sh
cd app
cp .env.example .env.production
export JARDIN_ENV_FILE=.env.production
TAKO_ENV_PASSPHRASE=... tako env push production --from-file .env.production
```

From another laptop or a CI runner:

```sh
cd app
export JARDIN_ENV_FILE=.env.production
TAKO_ENV_PASSPHRASE=... tako env pull production --force
tako state pull -e production
tako deploy -e production --yes
```

The deploy still uses committed Git source, so CI should run from a clean checkout
and should not generate or edit tracked files before `tako deploy`.

For the dedicated edge project, run from `edge/` after the app project has
healthy shared `admin` and `renderer` endpoints:

```sh
TAKO_NONINTERACTIVE=1 tako setup -e production --dedicated-edge
TAKO_NONINTERACTIVE=1 tako deploy -e production --yes
```
