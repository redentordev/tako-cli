# Goal: Jardin CMS Can Deploy With Tako

Date: 2026-06-15

Objective: make Tako support the missing deployment features required for a
project shaped like `/Users/redentordev/Work/jardin/cms`, so the project can
replace its current CapRover/SST app deployment flow with `tako` commands from
inside the project checkout.

This goal is not to copy CapRover. The target is a clean Tako-native path:

```text
cd ~/Work/jardin/cms
tako setup production
tako env push production --from-file .env.production
tako deploy production --yes
tako ps production
tako logs production admin
```

## Scope Update

The migration includes both the main Jardin app project and a separate
edge/export project:

- `jardin-cms` owns normal app runtime: `admin`, `site-renderer`, `mongo`,
  app volumes, app env, app deploys, and explicit private exports.
- `jardin-edge` owns public dynamic-domain ingress: Caddy, public `80/443`,
  Caddy data/config volumes, and imports from `jardin-cms`.
- Export discovery is not a standalone global database. Export records live in
  the producing project's desired state, and consumers resolve them through
  `takod` with project/environment/service/port scoping.
- Custom Caddy setup is supported by a checked-in Caddyfile referenced from
  `tako.yaml` as a managed config artifact, or by a generated Caddyfile
  artifact that consumes `imports`.
- Custom Caddy setup must not depend on editing files on the remote host,
  arbitrary remote host bind mounts, or server-side symlinks. The deployable
  source of truth must remain the checkout plus Tako state, so a fresh laptop
  or CI runner can reconcile the same result.
- This goal explicitly includes the export/edge project work. The export side is
  not an afterthought: the app project must publish scoped export records that a
  separate edge project can resolve from any developer machine or CI runner.
- Clarification: "the export project" means the dedicated `jardin-edge`
  deployment that imports app exports and owns Caddy/public `80/443`; it is not
  a third state service or a mutable host-side configuration project.

## Progress

### 2026-06-15

- Implemented service-level `dockerfile:` support for build-backed services.
- The selected Dockerfile is validated as a regular file inside the build
  context, passed to `takod`, included in config hashes, recorded in desired
  state, exposed in the JSON schema, and documented in the starter config.
- The remote `takod` build API validates the Dockerfile path again after
  extracting the uploaded build context and passes `--file <dockerfile>` to
  Docker or Buildx builds.
- The deployer force-includes the selected Dockerfile in the build archive even
  when `.dockerignore` excludes it. This matches the practical Docker client
  behavior Jardin relies on, because Jardin's `.dockerignore` excludes
  `Dockerfile`.
- Added regression coverage for config validation, invalid path traversal,
  missing Dockerfile paths, endpoint query encoding, build args, config hashes,
  desired state, and ignored-Dockerfile archive inclusion.
- Verification passed: `go test ./...`.
- Built local and Linux amd64 binaries, refreshed `takod` on both playground
  nodes, and deployed a disposable fixture whose `.dockerignore` excluded both
  `Dockerfile` and the selected `Dockerfile.renderer`.
- Playground evidence: deployment logs showed `Found Dockerfile:
  Dockerfile.renderer`; Docker build output showed the selected Dockerfile was
  transferred and used; `https://tako-dockerfile.playground-app-node.sslip.io`
  returned `custom-dockerfile-ok`; `ps`, `inspect`, `discovery`, `metrics`,
  `logs`, `state status`, and `doctor` were exercised.
- Cleanup evidence: `destroy --purge-all --force` removed app-owned resources
  from the active playground node, the leftover image from an interrupted
  two-node attempt was removed from the peer node, and shared `takod` plus
  `tako-proxy` remained running on both playground nodes.
- Separate follow-up found while testing: two-node image peer distribution can
  leave the CLI waiting even after the peer image is present. That is not part
  of `dockerfile:` support, but it should be investigated before relying on
  two-node Jardin deploys.
- Feasibility review: a separate edge/export project is the preferred migration
  shape for dynamic CMS domains. Jardin app services should stay in the
  `jardin-cms` project, export only the admin/renderer upstreams they intend to
  expose, and let a small `jardin-edge` project own Caddy plus public `80/443`.
  This avoids disabling `tako-proxy` on general app nodes.
- Feasibility review: linking a checked-in Caddyfile is appropriate only as a
  managed Tako config artifact, not a relative host bind mount. The required
  implementation is top-level `configs:` plus service-level read-only mounts
  stored under Tako-owned per-project/per-stage paths on each node.
- Implemented managed config-file artifacts for services. Top-level `configs:`
  sources are validated as regular files inside the checkout, service mounts
  are validated as absolute read-only container file targets, file contents are
  SHA-256 hashed for reconciliation, and the bytes are uploaded to `takod` as
  base64 for byte-preserving writes.
- The remote `takod` reconcile path writes config files under scoped
  `/var/lib/tako/configs/<project>/<stage>/<service>/<config>/<hash>/content`
  paths, mounts them read-only, records config hashes in desired state, and
  removes scoped config state during destroy cleanup.
- Verification passed: focused package tests and `go test ./...`.
- Playground evidence: deployed a disposable Caddy fixture using a checked-in
  `ops/caddy/Caddyfile` mounted at `/etc/caddy/Caddyfile`; endpoint
  `https://tako-configfile.playground-app-node.sslip.io` returned
  `config-file-v1`, then a Caddyfile-only change produced a deploy plan with
  `~ UPDATE` / `Service configuration changed` and the endpoint returned
  `config-file-v2`.
- Cleanup evidence: `destroy --purge-all --force` removed the disposable
  app; the route returned `404`; `takod` stayed `active`, `tako-proxy` stayed
  running, and `/var/lib/tako/configs/tako-configfile/production` was removed.
- Feasibility review: the edge/export project path is feasible and should be
  part of this goal. The clean model is that `jardin-cms` exports explicit
  private upstreams, while a separate `jardin-edge` project imports those
  upstreams and owns public Caddy routing. There is no need for a third
  standalone "exports database" project; export records live in the producing
  project's desired state and the edge project resolves them through `takod`.
- Feasibility review: a customized Caddy setup can be supported by linking a
  checked-in `Caddyfile` in `tako.yaml` as a managed config file. This is a
  source-path reference that Tako uploads, hashes, stores under scoped
  `/var/lib/tako/configs/...`, and mounts read-only into Caddy. It should not
  be implemented as a remote host symlink or arbitrary relative bind mount.
- Implemented the first cross-project export/import slice: services now declare
  explicit `export.ports`, projects can declare top-level `imports`, desired
  state records exported ports, `tako discovery --import <alias>` resolves the
  remote export and returns healthy live endpoints, and exported internal ports
  are published on the private mesh IP for cross-project edge consumption.
- Verification passed: focused package tests and `go test ./...`.
- Playground evidence: deployed disposable `tako-export-app` on
  `playground-edge-node` exporting `api:web`, refreshed `takod` on both playground
  nodes, and resolved it from a separate `tako-export-edge` config on
  `playground-app-node` with `tako discovery --import app_api`.
- Export/import evidence: the app endpoint returned HTTP 200 through
  `https://tako-export-app.playground-edge-node.sslip.io`, local discovery showed a
  mesh endpoint `10.210.0.1:44153` for target port `80`, edge import discovery
  returned the same healthy upstream, and desired state contained the export
  record for `tako-export-app/production/api:web -> 80`.
- Cleanup evidence: `destroy --purge-all --force` removed app-owned
  containers, networks, volumes, and the public route returned `404`; shared
  `takod` and `tako-proxy` stayed running on both playground nodes. Historical
  deployment records remained under `/var/lib/tako/history/tako-export-app`,
  which is acceptable as audit history but should be documented separately from
  runtime cleanup.
- Implemented the first dedicated-edge safety guard: host-port allocations now
  detect shared `tako-proxy` on public `80/443` and return an actionable
  dedicated-edge error before service reconciliation attempts to replace shared
  ingress.
- Added `tako discovery --format upstreams` so edge projects can resolve an
  import into a space-separated Caddy-compatible upstream list from a local
  checkout or CI job.
- Added `examples/deployment-patterns/cms-dynamic-domains` as a two-project
  pattern: `app/` owns Mongo plus exported admin/renderer services, and
  `edge/` owns Caddy, imports app exports, mounts a Caddyfile through managed
  configs, and binds host `80/443` directly.
- Verification passed: `go test ./...`, `go test ./examples`,
  `bash examples/deployment-patterns/validate.sh`, and `git diff --check`.
- Playground evidence: refreshed `takod` on `playground-app-node` with the new
  binary, deployed disposable `tako-edge-guard`, and confirmed the deploy
  failed before service reconciliation with
  `host port 80 is already owned by shared tako-proxy; use a dedicated edge
  node with tako-proxy disabled...`.
- Cleanup evidence: `destroy --purge-all --force` completed for the disposable
  edge project; no app-owned containers, networks, volumes, or port allocations
  remained; shared `takod` stayed `active` and `tako-proxy` stayed running.
  Historical audit records remained under `/var/lib/tako/history`, as expected.
- Implemented `tako setup --dedicated-edge`. After installing or refreshing
  `takod`, setup calls `DELETE /v1/proxy`; `takod` refuses to remove
  `tako-proxy` while any `.yml` or `.yaml` route files remain under
  `/etc/tako/proxy/dynamic`, and otherwise removes the shared proxy container.
- The setup version metadata now records either `tako-proxy` for normal nodes
  or `dedicated-edge` for nodes prepared to bind project-owned public `80/443`.
- Verification passed again after dedicated-edge setup work: `go test ./...`,
  `bash examples/deployment-patterns/validate.sh`, and `git diff --check`.
- Playground evidence: `tako setup --dedicated-edge --takod-binary ...` was
  run against `playground-edge-node` after refreshing `takod`; it refused to disable
  the shared proxy because
  `tako-mesh-playground-production-91d501741e.yml` still exists under
  `/etc/tako/proxy/dynamic`.
- Preservation evidence: after the refused dedicated-edge setup, `takod`
  remained `active`, `tako-proxy` remained running on public `80/443`, the
  existing route file remained in place, and no
  `tako-dedicated-edge-refusal` runtime state was created.
- Implemented generated Caddy config artifacts for edge projects. Top-level
  `configs.<name>.generate.caddy` now references import aliases plus static
  admin/site hostnames; deploy resolves imports through `takod`, renders a
  Caddyfile before plan computation, writes the rendered content hash into the
  service config, and uploads the generated bytes through the same managed
  config-file path as checked-in files.
- The generated Caddy renderer preserves `Host` and `X-Forwarded-Host`, emits
  static admin/site routes, emits the Caddy on-demand TLS ask policy and
  catch-all `https://` route when enabled, and rejects unsafe Caddy token
  characters in user-controlled fields.
- The `cms-dynamic-domains/edge` example now uses generated Caddy config from
  imports instead of manual upstream env exports. The reusable import/export
  validation used by `tako discovery --import` was moved into a shared package
  so deploy-time config generation and CLI discovery validate export records
  and private healthy endpoints the same way.
- Verification passed after generated-config work: `go test ./...`,
  `bash examples/deployment-patterns/validate.sh`,
  `python3 -m json.tool schema/tako.schema.json`, and `git diff --check`.
- Playground evidence: copied the `cms-dynamic-domains/app` and `edge`
  examples into temporary clean Git repos, deployed `pattern-cms-app` to
  `playground-edge-node`, and resolved imports from the edge checkout. Admin resolved
  to `http://10.210.0.1:26025`; renderer resolved to
  `http://10.210.0.1:22773 http://10.210.0.1:22774`, proving export records
  and two healthy renderer replicas were visible through the mesh.
- Playground edge evidence: ran the generated-config edge deploy against
  `playground-app-node` with verbose output; deploy printed
  `Rendered generated Caddy config caddyfile` before plan computation, then
  failed at the expected shared-proxy guard:
  `host port 80 is already owned by shared tako-proxy...`. No project-owned
  edge container was created.
- Playground cleanup evidence: `destroy --purge-all --force` removed the app
  fixture from `playground-edge-node`; post-cleanup SSH checks on both
  `playground-edge-node` and `playground-app-node` showed `takod=active`,
  `tako_proxy=true`, and no `pattern-cms-*` containers.
- Tightened the env import workflow for Jardin-style stage files. `tako env
  push [environment] --from-file .env.production` now accepts a positional
  environment argument, stores `.env` and `.env.<stage>` files under their
  basename in the encrypted bundle, preserves `.tako/secrets*`, and keeps
  pull restore constrained to `.env*` plus `.tako/secrets*` paths.
- Added regression coverage for positional env handling, `--from-file`
  bundling, comments and quoted values, `.env.production` restore,
  noninteractive `TAKO_ENV_PASSPHRASE`, and symlink rejection.
- Verification passed after env workflow work: `go test ./...` and
  `git diff --check`.
- Playground evidence: using a throwaway `tako-env-workflow` config on
  `playground-app-node`, ran
  `TAKO_ENV_PASSPHRASE=... tako env push production --from-file
  .env.production`; output included only the filename and not values. Removed
  the local file, ran `tako env pull production --force`, and verified
  `.env.production` was restored with comments and quoted values intact.
- Playground cleanup evidence: removed the throwaway
  `/var/lib/tako/env/tako-env-workflow` and
  `/var/lib/tako/leases/tako-env-workflow` state over SSH; post-cleanup checks
  showed `takod=active`, `tako_proxy=true`, and no `tako-env-workflow`
  leftovers.
- Scope clarification from migration review: keep the export/edge project in
  this goal, not as later optional polish. Jardin needs a first-class
  app-plus-edge migration path where `jardin-cms` exports private admin and
  renderer upstreams, and `jardin-edge` imports those upstreams, owns Caddy,
  and owns public `80/443` on a dedicated edge node.
- Scope clarification from migration review: custom Caddy remains feasible and
  supported through two declarative paths. Prefer generated Caddy config when
  the file only needs imported upstreams and standard Jardin routing; use a
  checked-in `Caddyfile` referenced by top-level `configs.<name>.source` when
  Jardin needs hand-written Caddy behavior. Do not support remote host
  symlinks or arbitrary host bind mounts for this path.
- Tightened health-gated migration behavior for Jardin-style admin/renderer
  services. `takod` health failures now consistently include recent container
  logs for failed health checks, health timeouts, monitor failures, and inspect
  failures; empty or unreadable logs are reported explicitly instead of
  returning a bare timeout/inspect error.
- `takod` actual state now records health counters parsed from Docker status:
  healthy, unhealthy, starting, no-healthcheck, and unknown replicas. The
  counters flow through replicated actual snapshots and fresh-machine recovery
  state without changing the existing total `replicas` semantics.
- `tako ps` now keeps the total running/desired replica count but adds a
  `HEALTH` column and marks services with unhealthy replicas as `unhealthy`.
  `tako inspect` already showed per-container Docker health and remains the
  slot-level debugging view.
- Rolling `start-first` replacement has explicit regression coverage: if a
  replacement renderer/admin container fails `/api/health`, `takod` removes
  only the temporary replacement, does not remove or rename over the old slot,
  and returns the health failure with recent logs.
- Verification passed after health-gating work: `go test ./pkg/takod -run
  'ActualState|ReconcileServiceRollingStartFirstKeepsOldContainerOnReplacementHealthFailure|ReconcileServiceCleansStartedContainersOnHealthFailure|ContainerHealth'`,
  `go test ./cmd -run 'PS|State'`, `go test ./pkg/reconcile
  ./pkg/takodstate`, `go test ./...`, and `git diff --check`.
- Playground evidence: built `/tmp/tako-health-gate-test` and Linux amd64
  `/tmp/tako-health-gate-linux-amd64`, refreshed `takod` on both
  `playground-app-node` and `playground-edge-node`, and deployed a disposable
  `tako-health-gate` fixture whose `/api/health` intentionally returned 500.
  Deploy failed as expected with
  `container health check failed ... last logs:` followed by the fixture log
  lines `tako-health-gate fixture started...` and
  `health check intentionally failing...`.
- Playground health-visibility evidence: created a short-lived app-labeled
  container with a failing Docker health check on `playground-app-node`; `tako ps`
  showed `web 1/1 x unhealthy 1 unhealthy`, and `tako inspect web` showed the
  same container as `running` with `HEALTH unhealthy`.
- Playground cleanup evidence: removed the short-lived health container and ran
  `destroy --purge-all --force`; post-cleanup SSH checks on both playground
  nodes showed `takod=active`, `tako_proxy=true`, no `tako-health-gate`
  containers, and no `tako-health-gate` images. The test route returned `404`
  after cleanup.
- Follow-up feasibility review: the separate export/edge project remains the
  right path for Jardin. It lets normal app nodes keep shared `tako-proxy` for
  ordinary static-domain projects while a purpose-selected edge node runs Caddy
  on public `80/443` and imports Jardin upstreams over the mesh.
- Follow-up feasibility review: linking a custom Caddyfile is feasible when the
  link is declarative in `tako.yaml` via managed `configs.<name>.source`.
  Arbitrary host-file links, remote symlinks, and mutable server-local Caddyfiles
  stay out of scope because they break fresh-machine and CI reconciliation.
- Playground finding from the Jardin-shaped Caddy fixture: colocated Caddy behind
  shared `tako-proxy` surfaced a routing bug where Traefik upstreams used Docker
  container names containing underscores. Docker DNS does not resolve those
  names reliably, so proxy upstreams should route to per-slot DNS-safe container
  network aliases instead.
- Implemented the proxy alias fix for that finding. `takod` container specs now
  accept per-container network aliases, the deployer assigns each slot a
  DNS-safe alias, and local `tako-proxy` upstreams route to that alias instead
  of the underscore-heavy Docker container name.
- Playground evidence: refreshed `takod` on `playground-app-node`, redeployed the
  Jardin-shaped `next-admin-renderer-mongo` fixture, and confirmed the generated
  Traefik route used
  `tako-pattern-next-cms-app-production-edge-colocated-...` as the upstream
  host. `docker exec tako-proxy wget http://<alias>:80/api/render` succeeded.
- Playground evidence: public
  `https://cms-colocated.playground-app-node.sslip.io/api/render` returned renderer
  JSON with `adminReachable: true` and `tokenValueExposed: false`; `ps` showed
  `admin` 1/1 healthy, `renderer` 2/2 healthy, `edge-colocated` 1/1 healthy,
  and `mongo` 1/1 running.
- Playground evidence: `logs`, `metrics`, `state status`, `discovery renderer
  --port 3000`, and `doctor` all worked for the fixture. `discovery` returned
  both renderer mesh endpoints, and `doctor` had zero failures with only expected
  temp-checkout warnings.
- Playground evidence: wrote `{_id: "volume", value: "v1"}` through `tako exec
  mongo`, restarted the Mongo container, and read the same record back, proving
  the fixture's named Mongo volume survived container restart.
- Full dedicated-edge playground test exposed a cross-project mesh gap: generated
  Caddy config resolved correct imported upstreams, but the edge node could not
  reach producer mesh IPs because the edge project only prepared its own node in
  WireGuard. Fixed by preparing import source servers as mesh peers before
  environment service nodes, while still deploying services only to the edge
  environment nodes.
- The same test exposed stale WireGuard interface addresses after changing a
  node from edge-only `10.210.0.1` to app-plus-edge `10.210.0.2`. Fixed mesh
  apply so `takod` removes stale IPv4 addresses from the interface before
  replacing the desired address.
- The cleanup path exposed that normal `tako setup` did not restore shared
  `tako-proxy` after a node had been switched to `--dedicated-edge`. Fixed setup
  refresh so non-dedicated setup explicitly reconciles shared `tako-proxy` and
  rewrites normal setup feature metadata.
- Playground evidence: cleaned old disposable route owners, prepared
  `playground-app-node` as the app node and `playground-edge-node` as a true dedicated edge
  node, deployed clean Git checkouts of `cms-dynamic-domains/app` and
  `cms-dynamic-domains/edge`, and confirmed edge deploy prepared both `app` and
  `edge` mesh nodes while reconciling only the edge service.
- Playground evidence: edge generated Caddy config from imports; admin resolved
  to `http://10.210.0.1:26025`, renderer resolved to
  `http://10.210.0.1:22773 http://10.210.0.1:22774`, the edge container could
  fetch the admin upstream over WireGuard, and public
  `https://cms-admin.playground-edge-node.sslip.io` plus
  `https://cms-site.playground-edge-node.sslip.io` returned HTTP 200 through Caddy.
- Playground evidence: repeated site requests returned both renderer container
  identifiers, proving the dedicated edge used the imported multi-replica
  renderer upstream list. `ps`, `logs`, `metrics`, `state status`, `doctor`, and
  `discovery --import jardin_renderer --format upstreams` passed for the live
  app-plus-edge flow.
- Playground cleanup evidence: destroyed both disposable projects, restored
  normal setup on the edge playground, and verified both `playground-app-node` and
  `playground-edge-node` had `takod=active`, `tako-proxy=true`, no pattern CMS
  containers, no route files, no pattern CMS volumes/images, and the test admin
  and site URLs returned `404`.
- Scope note from follow-up review: keep the export/edge project in this goal
  and treat custom Caddy as a supported deployment shape. Prefer generated Caddy
  config for the standard Jardin dynamic-domain path; use a checked-in
  `ops/caddy/Caddyfile` linked through `configs.<name>.source` when Jardin needs
  hand-written Caddy behavior. Do not support remote host symlinks, mutable
  server-local Caddyfiles, or arbitrary host bind mounts.
- Implemented `deployment.source` with `local` as the default and `git` as a
  committed-source build mode. `source: git` refuses dirty worktrees, builds
  service contexts from committed `HEAD`, applies the committed `.dockerignore`,
  and leaves local-source deploys available for dirty-worktree iteration with an
  explicit warning.
- Tightened build-context file-mode handling while testing git source. The CLI
  now normalizes Docker build-context file modes to readable Docker-style modes,
  and `takod` explicitly restores archive file modes after extraction so server
  umasks cannot make copied static files unreadable inside images.
- Added schema, starter config comments, CI/CD docs, orchestration docs, and
  Jardin-shaped examples for `deployment.source: git`.
- Playground evidence: created a disposable Git fixture using
  `deployment.source: git` on `playground-app-node`; a dirty `index.html` change caused
  deploy to fail before build with
  `cannot deploy git source with uncommitted changes`; after cleaning and
  refreshing `takod`, the fixture deployed from committed content and
  `https://tako-git-source.playground-app-node.sslip.io` returned
  `git-source-committed-v3`.
- Playground evidence: the successful git-source image had
  `/usr/share/nginx/html/index.html` mode `0644`, proving the CLI and remote
  `takod` build-context mode fixes worked together. `ps`, `logs`, `metrics`,
  `state status`, and `doctor` passed; `doctor` had only expected disposable
  fixture warnings for missing `.env` and `.gitignore`.
- Playground cleanup evidence: `destroy --purge-all --force` removed the
  disposable app; SSH checks showed `takod=active`, `tako-proxy=true`, and no
  `tako-git-source` containers, routes, images, or volumes. The public test URL
  returned `404` after cleanup.
- Added a real Jardin CMS draft in `/Users/redentordev/Work/jardin/cms`:
  `tako.yaml` for the app project, `tako.edge.yaml` for the dedicated Caddy edge
  project, `docs/deployment-tako.md`, and minimal `/api/health` routes for both
  the admin app and site renderer.
- Jardin draft model: `jardin-cms` deploys `mongo`, `admin`, and two
  `site-renderer` replicas from git-sourced clean commits; `jardin-edge` imports
  `admin:web` and `site-renderer:web`, generates the Caddyfile from live imports,
  and binds public `80/443` on a dedicated edge node.
- Feasibility decision recorded in the Jardin docs: generated Caddy is the
  default because upstreams stay resolved from Tako import state; a checked-in
  `ops/caddy/Caddyfile` linked through top-level `configs.<name>.source` is also
  feasible when custom Caddy directives are needed, but it must not hard-code
  live container IPs or depend on remote host symlinks.
- Live Jardin testing exposed and fixed a deployer bug: service `envFile` was
  validated but not emitted into the generated Docker env file. The fix loads
  `service.EnvFile`, emits its variables, lets explicit `env` override them, and
  keeps referenced `secrets` as the highest-priority override path.
- Live Jardin testing exposed and fixed a health-check compatibility gap:
  Next.js runner images often include `node` but not `curl` or `wget`. The
  generated takod health command now tries `curl`, then `wget`, then a small
  Node `fetch` probe before failing.
- Verification passed for the two live-run fixes:
  `go test ./pkg/secrets ./pkg/config ./cmd ./pkg/deployer` and focused
  `go test ./pkg/deployer ./pkg/secrets`.
- Jardin local verification passed:
  `pnpm typecheck && pnpm --filter @jardin/site-renderer typecheck`, plus
  `tako doctor -e production` for `tako.yaml` and
  `tako --config tako.edge.yaml doctor -e production` with only expected
  pre-deploy warnings.
- Playground evidence: deployed a clean temporary Git checkout of Jardin CMS to
  `playground-app-node`; after the env-file and Node-health fixes, `jardin-cms`
  reconciled `mongo`, `admin`, and two `site-renderer` replicas successfully.
  `tako ps -e production` reported `admin 1/1 healthy` and
  `site-renderer 2/2 healthy`.
- Playground edge evidence: prepared `playground-edge-node` with
  `tako setup --dedicated-edge`, deployed `jardin-edge` from git source, and
  verified `tako --config tako.edge.yaml ps -e production` showed the Caddy edge
  service `1/1 running` with `80->80` and `443->443`.
- Public Jardin evidence:
  `https://jardin-admin.playground-edge-node.sslip.io/api/health` returned HTTP 200
  with `{"ok":true,"service":"jardin-admin"}`, and
  `https://jardin-sites.playground-edge-node.sslip.io/api/health` returned HTTP 200
  with `{"ok":true,"service":"jardin-site-renderer"}` through Caddy.
- Export/import evidence:
  `tako --config tako.edge.yaml discovery --import jardin_renderer --format
  upstreams -e production` returned
  `http://10.210.0.1:24416 http://10.210.0.1:24417`, proving the edge project
  consumed the two healthy renderer replicas from app-project export state.
- Cleanup follow-up: destroying `jardin-edge` initially left an empty
  `/var/lib/tako/configs/jardin-edge` project directory. `takod` cleanup now
  removes empty project parent directories after stage-scoped state cleanup
  while preserving parents that still contain other stages.
- Restore follow-up: switching `playground-edge-node` from dedicated-edge mode back
  to shared-proxy mode initially left a running `tako-proxy` container with no
  active runtime 80/443 bindings because it was still tied to the removed edge
  network. `ReconcileProxy` now verifies actual `NetworkSettings.Ports` before
  reusing a running proxy and recreates it if public port bindings are missing.
- Cleanup verification after the fixes: refreshed `takod` on the playground,
  reran edge cleanup, and verified both nodes had `takod=active`, `tako-proxy`
  running, no Jardin containers/images/volumes/routes/config dirs, Docker
  reported `0.0.0.0:80->80` and `0.0.0.0:443->443` for `tako-proxy`, and the
  test Jardin domain returned HTTP 404 after cleanup.
- Tightened the Jardin-shaped `next-admin-renderer-mongo` fixture so runtime
  token delivery uses a service `envFile` selected by `JARDIN_ENV_FILE` instead
  of direct shell-only env injection. The fixture now ships a non-secret
  `.env.example`, validates with `JARDIN_ENV_FILE=.env.example`, and documents
  real deploys with `JARDIN_ENV_FILE=.env.production` restored by
  `tako env pull`.
- Added regression coverage that the fixture docs include the second-checkout
  and CI-style commands: `tako env push production --from-file .env.production`,
  `tako env pull production --force`, `tako state pull -e production`,
  `tako deploy -e production --yes`, and noninteractive dedicated-edge deploy
  commands. Verification passed: `go test ./examples` and
  `bash examples/deployment-patterns/validate.sh`.
- Playground evidence after the fixture env-file tightening: deployed a clean
  Git checkout of `next-admin-renderer-mongo/app` to `playground-app-node` with
  `JARDIN_ENV_FILE=.env.example`. Deploy output showed `Source: git HEAD`,
  `Build source: git`, and generated env files with four variables for the
  admin and renderer services.
- Updated fixture public proof:
  `https://cms-colocated.playground-app-node.sslip.io/api/render` returned HTTP 200
  with `adminReachable:true`, `tokenAccepted:true`, and
  `tokenValueExposed:false`, proving the runtime token came through the env file
  and was not exposed by the endpoint. `ps` showed admin 1/1 healthy, renderer
  2/2 healthy, edge-colocated 1/1 healthy, and Mongo running; `inspect`,
  `discovery`, `logs`, `metrics`, `state status`, and `doctor` all completed.
- Updated fixture cleanup evidence: `destroy --purge-all --force` removed the
  fixture; SSH checks showed `takod=active`, `tako-proxy` running, no
  `pattern-next-cms` containers/images/volumes/routes/config dirs, and the
  public fixture URL returned HTTP 404 after cleanup.

## Replacement Target

Jardin currently needs these runtime pieces:

- `admin`: Next.js standalone app from repo root `Dockerfile`.
- `site-renderer`: Next.js standalone app from repo root `Dockerfile.renderer`.
- `mongo`: MongoDB 7 with persistent `/data/db`.
- `edge`: public ingress for admin, platform site host, and arbitrary verified
  customer domains.
- AWS-backed media and jobs: S3, CloudFront, SQS, and Lambda stay external for
  this goal. Tako only needs to pass env/secrets and run containers reliably.

The desired Tako service graph is:

```text
jardin-cms / production
  mongo
    image: mongo:7
    volume: mongo-data:/data/db

  admin
    build: .
    dockerfile: Dockerfile
    internal: mongo, site-renderer
    public: admin.vixilabs.com

  site-renderer
    build: .
    dockerfile: Dockerfile.renderer
    internal: admin
    public: sites.vixilabs.com plus customer domains through edge

  edge
    image/build: caddy edge image
    host ports: 80, 443
    volumes: caddy-data, caddy-config
    ask endpoint: http://admin.tako.internal:3000/api/platform/domains/ask
```

For production dynamic domains, the safer topology is a dedicated edge project
on a tiny edge node instead of disabling `tako-proxy` on general-purpose app
nodes:

```text
general app node(s)
  takod
  tako-proxy for ordinary static-domain Tako apps
  jardin admin / renderer / mongo

edge node
  takod
  no tako-proxy on public :80/:443
  caddy edge service owns public :80/:443
  imports Jardin admin/renderer upstreams over the WireGuard mesh
```

That means `tako-proxy` is not globally disabled. It is only absent from nodes
whose explicit role is dynamic-domain edge traffic.

## Tako Features To Build

### P0: Build Config Parity

Add explicit Docker build options to service config:

```yaml
services:
  site-renderer:
    build: .
    dockerfile: Dockerfile.renderer
    platform: linux/amd64
```

Required behavior:

- `dockerfile` is relative to the build context unless absolute paths are
  explicitly supported and validated.
- Reject Dockerfiles outside the build context by default.
- Pass `-f <dockerfile>` to Docker and Buildx builds.
- Include `dockerfile`, `build`, `platform`, build cache settings, and future
  build args in config hashing and desired state.
- Update JSON schema, examples, completions, and config validation.
- Add regression tests for standard Docker, Buildx, cache-enabled builds, and
  invalid path traversal.

Stretch only if needed:

- `target`
- `buildArgs`
- `labels`

### P0: Dedicated Edge Service Mode

Support a project-owned edge service that can safely bind public ports `80` and
`443` without colliding with shared `tako-proxy`.

Required behavior:

- If a service requests host ports `80` or `443`, preflight must detect an
  already-running `tako-proxy` and fail with an actionable message.
- Add a safe command or setup option for a dedicated host to disable
  `tako-proxy` only when no active Tako proxy routes depend on it.
- Record host-port ownership in takod reservations so unrelated projects on the
  same node cannot claim the same bind.
- Preserve the current scoped cleanup model: destroying Jardin must remove its
  edge container and volumes only when those resources are app-owned; it must
  not remove shared `takod`.
- Document that dynamic customer-domain projects need a dedicated edge owner on
  that node unless Tako later supports dynamic domains inside `tako-proxy`.

The first supported edge implementation should be Caddy because Jardin already
has a Caddy on-demand TLS model and ask endpoint.

### P0: Custom Edge Config Files

Support project-owned config files that can be shipped to a service and mounted
read-only, so a Caddy edge service can use a checked-in `Caddyfile` without
unsafe host bind assumptions.

Example:

```yaml
configs:
  caddyfile:
    source: ./ops/caddy/Caddyfile

services:
  edge:
    image: caddy:2.9-alpine
    ports:
      - name: http
        target: 80
        mode: host
      - name: https
        target: 443
        mode: host
    configs:
      - source: caddyfile
        target: /etc/caddy/Caddyfile
        mode: "0444"
```

Required behavior:

- Config sources must be regular files inside the project checkout.
- A "linked Caddyfile" means a source file referenced by `configs.<name>.source`
  and uploaded by Tako. It does not mean a symlink to a remote host path or a
  general bind mount escape.
- Config files are uploaded through `takod`, stored under Tako-owned paths, and
  mounted read-only into the target container.
- Config names are app/stage scoped to avoid cross-project collisions.
- Config content changes must trigger reconcile for services that mount them.
- Secret-looking config values are allowed but discouraged; docs should point
  users to encrypted env/secrets for credentials.
- Relative bind mounts remain rejected. This feature is a controlled file
  artifact, not a general host filesystem escape hatch.
- For edge projects, support a follow-up generated config artifact that renders
  Caddy upstreams from top-level `imports`, so CI and fresh developer machines
  do not need manual env exports before deploying the edge.

### P0: Caddy Edge Example

Add a first-class example for dynamic CMS hosting:

```text
examples/deployment-patterns/cms-dynamic-domains/
  app/tako.yaml
  edge/tako.yaml
  README.md
```

The example must show:

- Caddy binding `80` and `443`.
- Caddy preserving the original `Host` header.
- Caddy routing admin host to `admin`.
- Caddy routing platform/customer site hosts to `site-renderer`.
- On-demand TLS ask endpoint through internal discovery.
- Persistent Caddy data/config volumes.
- Clear note that this mode owns the node edge.
- Generated Caddyfile delivered through Tako config-file mounts.
- Optional checked-in `Caddyfile` delivery remains supported by the lower-level
  `configs.<name>.source` feature.
- Separate app and edge projects:

  ```text
  cms-dynamic-domains-app/
    tako.yaml      # exports admin:web and renderer:web

  cms-dynamic-domains-edge/
    tako.yaml      # imports app exports, generates and mounts Caddyfile
  ```

- The edge project must be deployable from a clean checkout without editing
  files on the remote node. If the Caddyfile needs generated upstreams, Tako
  should provide a deterministic render step that consumes `imports` and writes
  a managed generated config artifact before deployment.

### P0: Cross-Project Exports For Edge Projects

Support explicit service exports and imports so a dedicated edge project can
route to services owned by the Jardin app project without joining their Docker
network directly.

Example:

```yaml
# jardin-cms/tako.yaml
services:
  admin:
    export:
      ports:
        web: 3000
  site-renderer:
    export:
      ports:
        web: 3000

# jardin-edge/tako.yaml
imports:
  jardin-admin:
    project: jardin-cms
    environment: production
    service: admin
    port: web
  jardin-renderer:
    project: jardin-cms
    environment: production
    service: site-renderer
    port: web
```

Required behavior:

- Exports must be explicit; no cross-project service is discoverable by default.
- Imports resolve healthy endpoints from replicated `takod` state or live
  health-filtered discovery.
- Imported endpoints use WireGuard mesh IPs or other private node addresses, not
  public IPs.
- Removed/offline nodes must disappear from imported upstreams.
- `tako discovery` should show whether an endpoint is local, mesh-reachable, or
  unavailable.
- Caddy edge rendering can use imported upstreams for `reverse_proxy` targets.
- The failure mode must be clear: if no healthy imported upstream exists, the
  edge deploy should fail or render a maintenance response intentionally.
- Import resolution must work from local development machines and CI, not only
  from the original deploy machine. A fresh checkout with SSH access should be
  able to run `tako discovery --import`, render the Caddy config, reconcile the
  edge service, and inspect current state.
- Export records must be scoped by project and environment so unrelated
  projects sharing the same node cannot accidentally expose or consume each
  other's services.
- The edge project should not need to deploy a placeholder app container just
  to hold imports. If a project only owns edge routing, Tako should allow that
  shape or provide a small first-class edge service template.

This is feasible and important if the edge is its own smallest node/project.
Without it, the edge service would need admin/renderer containers colocated on
the edge node or hard-coded private upstream addresses.

For the Jardin migration, this becomes a first-class path:

```text
jardin-cms
  owns admin / site-renderer / mongo
  exports admin:web and site-renderer:web

jardin-edge
  owns Caddy and public 80/443
  imports jardin-cms production admin:web and site-renderer:web
  generates and mounts a Caddyfile through Tako config files
```

This is preferable to running a hand-edited Caddy container inside the app
project on a shared node, because port ownership and dynamic-domain TLS policy
stay isolated.

Implementation status:

- Explicit `export.ports` and top-level `imports` are implemented.
- Desired-state export records are implemented.
- `tako discovery --import <alias>` is implemented.
- `tako discovery --format upstreams` is implemented for Caddy env workflows.
- Mesh-published exported internal ports are implemented.
- Import-aware mesh peering is implemented so edge projects can reach producer
  mesh IPs without deploying services onto producer nodes.
- Host-port preflight for public `80/443` against shared `tako-proxy` is
  implemented.
- `tako setup --dedicated-edge` is implemented and route-file guarded.
- Normal `tako setup` restores shared `tako-proxy` after a node is no longer in
  dedicated-edge mode.
- A two-project CMS edge example is implemented.
- Fully automatic generated Caddy config artifacts from imports are
  implemented.
- Live playground proof of the full app-plus-edge Caddy flow on a true dedicated
  edge node is complete.

### P1: Git-Safe Build Source

Add an optional deploy/build source mode for committed Git content:

```yaml
deployment:
  source: git
```

Required behavior:

- Build archives from tracked committed files, similar to `git archive HEAD`.
- Fail or warn clearly when the working tree is dirty.
- Still honor `.dockerignore` where relevant, or document the exact precedence.
- Label deployments with Git commit metadata.
- Keep default local build behavior for fast iteration.

This is important because Jardin's current CapRover path intentionally avoids
uploading `.env`, `node_modules`, `.next`, and stale deploy archives.

Implementation status:

- `deployment.source` is implemented and validated with `local` and `git` modes.
- `source: git` deploys reject dirty worktrees before build; local-source deploys
  keep fast iteration and print an explicit dirty-worktree warning.
- Git-source build contexts are created from committed `HEAD` content, scoped to
  each service build context, and then filtered through the committed
  `.dockerignore`.
- Deployment history still records Git commit metadata; build output prints the
  selected source mode.
- Docker build context file modes are normalized on the CLI and preserved by
  `takod` extraction, with regression coverage for restrictive umasks.
- Playground proof for dirty guard, committed-source deploy, public endpoint,
  operator commands, and cleanup is complete.

### P1: Env And Secret Import Workflow

Make the env workflow easy enough that Jardin can stop using CapRover app env:

```text
tako env push production --from-file .env.production
tako env pull production
tako secrets list production
```

Required behavior:

- Never print secret values by default.
- Preserve current encrypted env push/pull behavior.
- Accept `.env` files with comments and quoted values.
- Make CI usage explicit: noninteractive push/deploy must work without a TTY.
- Add docs for importing SSM-generated env output without committing secrets.

If existing commands already cover this, tighten docs and tests instead of
adding new command surface.

Implementation status:

- Encrypted env push/pull existed and is preserved.
- Positional environment arguments for `tako env push production` and
  `tako env pull production` are implemented.
- `tako env push --from-file .env.production` is implemented and stores
  `.env.<stage>` files with their basename for fresh-checkout restore.
- Noninteractive `TAKO_ENV_PASSPHRASE` is implemented and playground-tested.
- Pull restore remains path-constrained and refuses symlink targets/directories.

### P1: Health-Gated Migration Defaults

Tako already has health-aware deploy behavior. For this migration path, make the
defaults and failure output strong enough for Next.js services:

- Example health path: `/api/health`.
- Failed health checks should show recent container logs.
- Rolling deploys must not remove the last healthy renderer/admin replica when
  replacement health fails.
- `tako ps` and `tako inspect` should make unhealthy slots obvious.

Jardin itself may need small app health routes, but Tako should provide the
expected config and operator feedback.

### P2: Jardin Migration Fixture

Add a realistic fixture that mirrors Jardin without carrying private code:

```text
examples/deployment-patterns/next-admin-renderer-mongo/
  admin app
  renderer app
  mongo
  caddy edge
  runtime API token
  persistent volumes
```

It should prove:

- Two Dockerfiles from one repo root.
- Admin to Mongo internal DNS.
- Renderer to admin internal DNS.
- Public edge routing, both colocated and dedicated-edge variants.
- Cross-project exports/imports from app project to edge project.
- Generated Caddyfile delivery through config-file mounts, plus checked-in
  source-file fallback coverage.
- Rolling deploy of admin and renderer.
- Volume persistence across redeploy.
- Env push/pull from a second checkout.
- CI-style noninteractive deploy.

## Acceptance Test Against Playgrounds

Every phase must be tested on the playground servers before it is marked done:

```text
playground1 = playground-app-node
playground2 = playground-edge-node
```

Minimum evidence per phase:

- `make ci-check` or focused tests for changed code.
- Local `tako` binary built.
- `takod` refreshed on both playground nodes if server code changed.
- Disposable app/stage deployed with `--yes`.
- `tako ps`, `tako logs`, `tako metrics`, `tako state status`, and
  `tako doctor` run against the fixture.
- Public endpoint tested through `sslip.io`.
- If edge mode is tested, verify `80/443` ownership and TLS issuance.
- If dedicated edge mode is tested, deploy two projects: the app project exports
  admin/renderer endpoints and the edge project imports them.
- If config-file mounts are tested, change a source config file or generated
  config input and verify the edge service reconciles.
- Cleanup verified over SSH: no app-owned containers, networks, volumes,
  config files, proxy files, backup files, or host-port reservations remain.
- Shared `takod` remains running after cleanup; shared `tako-proxy` remains
  running on non-edge nodes.

## Jardin Cutover Plan

After the Tako features above are done:

1. Add `tako.yaml` to `~/Work/jardin/cms`.
2. Add or reuse health routes for `admin` and `site-renderer`.
3. Generate `.env.production` from current SSM/CapRover env scripts without
   committing it.
4. Run a staging deploy on playground with `sslip.io` domains.
5. Test admin login, renderer runtime API, media env, Mongo persistence, and
   dynamic domain ask flow.
6. If using a dedicated edge node, deploy the edge as a separate Tako project
   that imports Jardin's exported admin/renderer endpoints.
7. Export current CapRover Mongo and restore into the Tako Mongo volume.
8. Deploy production Tako services.
9. Switch DNS for admin/site traffic to the Tako edge node.
10. Keep existing S3, CloudFront, SQS, and Lambda infra until there is a separate
   infrastructure migration goal.
11. Retire CapRover app deploy only after Tako state, logs, health, and rollback
    are verified.

## Non-Goals

- Do not replace S3, CloudFront, SQS, Lambda, or AWS IAM in this goal.
- Do not implement full Docker Compose compatibility.
- Do not make `tako-proxy` handle arbitrary CMS customer domains in this pass.
- Do not silently stop shared proxy services on nodes that may host unrelated
  projects.
- Do not use arbitrary relative host bind mounts for Caddyfile delivery.
- Do not commit Jardin production secrets, CapRover passwords, SSH keys, or
  playground-only throwaway credentials.

## Done Definition

This goal is done when:

- Tako supports `dockerfile:` for Jardin's two-image root build.
- Tako has a documented, tested dedicated Caddy edge service path.
- Tako can run a dedicated edge project that imports explicitly exported Jardin
  admin/renderer endpoints.
- Tako can deliver a checked-in Caddyfile as a scoped read-only config file.
- Host port ownership prevents cross-project conflicts.
- Env/secrets can be imported and used from local and CI workflows.
- A Jardin-shaped example deploys successfully on the playground.
- A draft `tako.yaml` can be added to Jardin CMS and used as the replacement
  deployment entrypoint.
