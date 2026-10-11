# Preview Environments

A preview environment is a short-lived, isolated copy of your app, usually
one per pull request:

```bash
tako deploy --preview pr-42          # create or update preview-pr-42
tako ps --preview pr-42              # status, logs, exec, history, rollback
tako preview list                    # every preview of the project
tako preview destroy pr-42 --yes     # remove it and all its data
tako preview prune --yes             # remove every expired preview
```

Previews run on the servers you already declare in `servers:`. Tako creates
no DNS records and no cloud resources; you provide a wildcard DNS record once.
For pull-request automation, see [CI-CD.md](./CI-CD.md#preview-environments-per-pull-request).

## How a preview is built

You opt in by adding a `preview:` block to one environment, the preview
*base*. `tako deploy --preview NAME` then synthesizes a new environment
`preview-<name>` that extends the base, applies the preview policy below, and
deploys it like any other environment. Nothing is written to `tako.yaml`.

- The name is normalized: lowercase, every run of other characters becomes
  `-` (`Feature/Login` becomes `feature-login`). Names longer than 40
  characters are cut and suffixed with a short hash.
- With one preview base, Tako uses it. With several, choose one with `-e`
  (`tako deploy --preview pr-42 -e preview`).
- Each preview has its own deployment history, lease, mesh peer set,
  Docker network, containers and volumes. Its leases, builders and edge
  nodes are scoped to the base's servers. It never blocks deploys of the
  base or of other previews, but a node maintenance lease (`tako upgrade`,
  `tako setup`) blocks preview deploys and teardowns on that node like any
  other environment lease.

## Setting up the template

The recommended base is an abstract `preview` template that extends your
staging (or production) environment, drops what previews do not need, and
runs on a dedicated preview server:

<!-- tako:snippet complete -->
```yaml
project:
  name: shop
  version: 1.0.0

servers:
  app:
    host: ${TAKO_APP_HOST}
    user: root
    sshKey: ${TAKO_SSH_KEY}
  previews:
    host: ${TAKO_PREVIEW_HOST}
    user: root
    sshKey: ${TAKO_SSH_KEY}

environments:
  staging:
    servers: [app]
    services:
      web:
        image: ghcr.io/example/shop:1.4.2
        port: 3000
        replicas: 2
        secrets: [DATABASE_PASSWORD]
        proxy:
          domain: staging.example.com
      worker:
        image: ghcr.io/example/shop:1.4.2
        command: ["bin/worker"]
      db:
        image: postgres:16
        persistent: true
        volumes: ["pgdata:/var/lib/postgresql/data"]

  preview:
    abstract: true
    extends: staging
    servers: [previews]
    services:
      worker: null
      web:
        proxy:
          tls:
            staging: true
    preview:
      domain: "{name}.preview.example.com"
      ttl: 72h
      max: 5
      env:
        PUBLIC_URL: "{url:web}"
```

`tako deploy --preview pr-42` deploys `web` and `db` (no `worker`) to the
`previews` server, with `web` at `https://pr-42.preview.example.com`, one
replica, and `PUBLIC_URL` set to that URL.

The `preview:` block is not inherited: it makes only the environment that
declares it a base. An abstract template with `preview:` is not reported as
unused. A concrete environment (for example `staging`) can declare `preview:`
too, but then previews share its servers and secrets file; prefer the
abstract template.

### `preview:` reference

| Key | Default | Meaning |
|---|---|---|
| `domain` | `{name}.{domain}` | Host template for every proxied service. `{name}` is the preview name, `{service}` the service name, `{domain}` the service's own domain (or internal `host`). |
| `ttl` | `168h` | Lifetime after the last successful deploy: a Go duration (`72h`) or `none`. `deploy --ttl` overrides it for one deploy. |
| `max` | `5` | Previews allowed for this base at once. |
| `maxReplicas` | `1` | Replica cap for every service except `placement.strategy: global`. |
| `keepJobs` | `false` | Keep `kind: job` services (dropped by default). `kind: run` steps always run. |
| `env` | | Added to every service. Placeholders: `{name}`, `{host:<service>}`, `{url:<service>}`. |
| `services.<name>.env` | | Added to one service, with the same placeholders. |

Every preview service also gets `TAKO_PREVIEW=true`, `TAKO_PREVIEW_NAME`,
and, when proxied, `TAKO_PREVIEW_URL`. Lifecycle hooks of a preview run see
`TAKO_PREVIEW_NAME`, `TAKO_PREVIEW_BASE` and `TAKO_PREVIEW_EXPIRES_AT`.

## What the preview policy changes

Applied to the synthesized environment only; the base is unchanged:

1. `kind: job` services are dropped unless `keepJobs: true`.
2. Isolation checks (exit 2): a preview service must not publish host
   `ports`, use `imports`, bind-mount a host path (`/…`, `./…`, `~/…`), or
   mount a volume declared with `name`, `external`, a non-`local` `driver`
   or `driver_opts`. Each preview gets fresh, empty volumes of its own. Fix
   these in the template, for example `ports: null`. `tako validate` warns
   about them ahead of time.
3. `backup` and `monitoring` are removed, and `deploy.promotion` is forced
   to `automatic`.
4. Replicas are capped at `maxReplicas`.
5. Proxy routes get the rendered host; `domains`, `redirectFrom`,
   `dynamicDomains` and `cdn` are cleared. `tls`, `basicAuth`, `allowIps`,
   `trustedProxies` (CIDRs and presets such as `cloudflare`, resolved at
   preview deploy time like any deploy) and the other route settings are
   kept. `requestHeaders` are kept except a header whose value is one of the
   base's hosts (for example `X-Forwarded-Host: app.example.com` behind a
   CDN): previews drop it so the preview host is forwarded instead, and
   `tako validate` warns about it.
6. `preview.env` and `preview.services.<name>.env` are merged in.

Preview data is never cloned from the base. Seed it with a `kind: run` step
or a post-deploy hook if you need fixtures.

## DNS and certificates

Create one wildcard DNS record that points at the preview server's proxy,
for example `*.preview.example.com` (or `*.shop.example.com` with the default
`{name}.{domain}` template). Rendered hosts must be valid DNS names (labels up
to 63 characters) and must not collide with a host any declared environment
serves.

Certificates are issued per host, by HTTP-01 or the environment's DNS-01
provider, exactly as for other environments. Redeploying the same preview
reuses its host and certificate. Let's Encrypt allows 50 certificates per
registered domain per week, so busy repositories should use a separate
preview domain, and `proxy.tls.staging: true` in the template while trying
previews out. Domain checks warn by default; `--strict-domains` makes them
fail the deploy.

## Secrets

A preview reads the secrets files of its **base**: `.tako/secrets`, then
`.tako/secrets.<base>`. With the abstract template above that is
`.tako/secrets.preview`, kept apart from staging and production:

```bash
tako secrets set DATABASE_PASSWORD=preview-only-value -e preview
```

`secretSources` are inherited through `extends` as usual. Use
non-production credentials for previews.

## Lifetime, limits and pruning

- Each deploy sets `expiresAt` to now plus the ttl (`--ttl` for that deploy,
  else the template's `ttl`). A failed deploy does not extend it.
- `tako preview prune` destroys every expired preview. A preview that was
  redeployed while the prune waited for its lease is skipped. `tako deploy
  --preview` runs the same prune just before it changes anything (best
  effort; `--no-prune` skips it), so `--plan-only` or a declined
  confirmation never prunes or records anything.
- A new preview is checked against `max` for the base. Reaching `max` exits
  2 and lists the existing previews. Previews left by failed first deploys
  count, because they still hold containers and volumes. Redeploying an
  existing preview is never blocked by `max`, even after lowering it.
- `tako preview list` marks expired previews, and `tako doctor` warns about
  them.
- Schedule `tako preview prune --yes` (for example nightly in CI) so expired
  previews are removed even when nobody deploys.

## Destroying a preview

`tako preview destroy NAME` removes, on every server the preview touched:
its proxy routes, containers, Docker network, volumes, history and takod
state, its mesh peer set (on nodes whose takod keeps per-environment peer
sets), and its record last. It does not remove images (they share
repositories with the base) or the project's `/opt/<project>` files. If a
step fails, the record stays and rerunning the same command retries.
Destroying a preview that does not exist reports `not_found` and exits 0.

Preview deploys skip image retention on servers that a declared environment
also uses; there, preview images are reclaimed by the base's next deploy or
`tako cleanup`. On dedicated preview servers retention runs normally.

## Other commands

`ps`, `logs`, `exec`, `history` and `rollback` accept `--preview NAME`. They
look up the preview record first and exit 2 when the preview does not exist
or when `-e` names a different base. `tako config explain --preview NAME`
works offline and shows what the next preview deploy would produce. Every
other command rejects `--preview` with exit 2, including `tako mesh prune`:
a preview's peer set is removed by `tako preview destroy` and
`tako preview prune`.

## Requirements

Every server a preview uses must run a takod with the `preview.lifecycle-v1`
capability. Run `tako upgrade servers` first; deploys, destroys and prunes
fail before changing anything on a server that lacks it, and `preview list`
skips such servers with a warning.

## Troubleshooting

| Symptom | Cause and fix |
|---|---|
| `no environment declares a preview: block` | Add a `preview:` block to the template. |
| `several environments declare a preview: block` | Choose the base with `-e`. |
| `preview pr-42 was created from preview, not staging` | A preview keeps its base; drop `-e` or destroy it first. |
| `preview limit reached` | Destroy a preview, run `tako preview prune --yes`, or raise `max`. |
| `environment preview-NAME already has desired state … it is a declared environment` | A declared environment uses the preview's identity (perhaps on another branch). Choose another preview name. |
| `environment preview-NAME is preview NAME …` on `deploy -e preview-NAME` | A preview owns that name; destroy it before deploying a declared environment of that name. |
| `service web publishes host ports` (or bind mounts, shared volumes, imports) | Override the key in the template, for example `ports: null`. |
| `host … is already served by environment …` | Change `preview.domain` so previews get their own hosts. |
| Domain check pending | Create the wildcard DNS record. |
| `takod does not support preview environments (missing capability preview.lifecycle-v1)` | Run `tako upgrade servers`. |
| Exit 3 | Another deploy or destroy of the same preview holds its lease; retry. |
| Exit 6 from `preview list` or `prune` | A server was unreachable or a preview could not be destroyed; check the warnings and rerun. |

Machine-readable output (`--output json`, `--events ndjson`) is described in
[MACHINE-INTERFACE.md](./MACHINE-INTERFACE.md#preview-environments).
