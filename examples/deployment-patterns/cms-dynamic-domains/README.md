# CMS Dynamic Domains

Two-project pattern for a CMS with app services on ordinary Tako nodes and a
dedicated Caddy edge node for dynamic customer domains.

```text
app/
  admin, renderer, mongo
  shares admin and renderer default ports

edge/
  imports shared app endpoints for edge config
  generates Caddyfile from imports as a managed config
  binds public 80 and 443 directly
```

The edge node must be dedicated to this edge project. If shared `tako-proxy` is
running on the node, Tako rejects direct host binds on `80/443` with an
actionable error instead of replacing the shared proxy.

The app project sets `deployment.source: git` so CI and fresh laptops build the
committed repository state, not runner-local artifacts or untracked files.

Prepare the edge node with `tako setup --dedicated-edge`. That setup path
stops shared `tako-proxy` only when no active proxy route files exist.

## Deploy

Deploy the app project first:

```sh
cd app
tako setup -e production
tako deploy -e production --yes
```

Deploy the edge project:

```sh
cd ../edge
tako setup -e production --dedicated-edge
tako deploy -e production --yes
```

The Caddyfile is rendered from explicit edge `imports`, uploaded, and mounted
read-only through Tako `configs`; nothing is edited on the remote host and no
upstream addresses are hard-coded in the checkout.

## Notes

- Replace the demo images with the real CMS admin and renderer images or builds.
- Keep Mongo pinned while using node-local storage.
- Keep `JARDIN_ADMIN_HOST` and `JARDIN_SITE_HOST` unique per environment.
- Customer domains are handled by the catch-all HTTPS site with Caddy
  on-demand TLS and the admin ask endpoint.
