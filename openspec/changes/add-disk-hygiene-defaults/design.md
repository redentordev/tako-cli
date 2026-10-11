# Design

## Context

### What exists

- **Log rotation.** `ServiceLoggingConfig{Driver, MaxSize, MaxFiles}` (`pkg/config/types.go:416-422`) is opt-in per service. It is validated to `local|json-file`, at most 10g and 1–100 files (`validator.go:1897-1927`). takod adds `--log-driver/--log-opt` only when `req.Logging != nil` (`reconcile.go:1355-1364`), gated by `container.logging-v1`. Without it, the daemon default applies. On stock Docker that is `json-file` with no size limit, and Tako setup never writes `/etc/docker/daemon.json`.
- **Image cleanup (v0.14.2, `pkg/takod/image_cleanup.go`).**
  - Deploy-time cleanup sends `CleanupRequest{CleanOldImages, KeepImages: 3, ImageRepositories}` to every target server after success (`pkg/engine/deploy.go:1255-1292`). `tako cleanup` uses the same path with `--keep` (default 3, or 2 with `--full`). Preview deploys skip `CleanOldImages` on servers shared with declared environments (`cleanOldImagesOn`).
  - Repositories come from `CleanupImageRepositories` (`pkg/engine/takod.go:257-284`): built and shared-build services only; pulled `image:` services are excluded. Repositories are `[registry/]<project>/<service>`, with the environment only in the tag.
  - `CleanupProjectWithDataDir` removes stopped containers first, then calls `cleanupOldProjectImages(ctx, dataDir, project, environment, keep, repositories)`.
  - `readImageCleanupState` builds the evidence: *owners* (image ref → environments of the project whose `desired/<p>/<e>/revision.json`, archived `desired/<p>/<e>/revisions/*.json`, `history/<p>/<e>/history.json` or `deployments/*.json` name it, plus the legacy `<version>-<env>` tag suffix); *protected refs* (every current desired and actual document of **every** project on the node); and *in use* (image ID and ref of every container in any state). Any unreadable document fails the whole read.
  - `planOldImageRemovals` ranks, per repository and in Docker's newest-first order, the images owned by the target environment, keeps the newest N, and removes an image only if every ref on it is owned by the target environment alone, protected by nothing and used by no container. Removal is `docker rmi` by ID (or by its refs when it has several) without `-f`.
- **The desired archive is unbounded.** `PutStateDocument` archives every desired write to `desired/<p>/<e>/revisions/<id>.json` (`pkg/takod/state.go:86-99`) and nothing prunes it. Treating archived revisions as *protection* would protect every image ever deployed; v0.14.2 correctly uses them only as *ownership*.
- **Autonomous loops.** `runBuildCachePruneLoop` runs every 24h, with the first run after up to 5 minutes. It holds the maintenance barrier, checks the worker operation fence and takes the mutation lock (`server.go:659-712`). Both locks are *shared* flocks (`pkg/recovery/lock_unix.go`): they exclude snapshots and node upgrades, not other mutators. Per-environment exclusion is the operation lease (`<dataDir>/leases/<p>/<e>.json`).
- **Disk admission.**
  - `checkFreeDisk`, `requireFreeDisk` and `reserveFreeDisk` (`server.go:2137-2300`) deny work below `minimumFreeDiskBytes` with HTTP 507, or 503 when the probe fails. Every such message starts with `operation denied`.
  - Schedulers admit through `checkFreeDisk` (`server.go:293-295`).
  - The platform worker has its own `AdmissionController` (`pkg/platform/admission.go`).
- **Retention and failure containers.** Restore-on-failure keeps the previous containers stopped under a retention record (`retainedStatePath`, `restore.go:161`) until finalize or restore; they are the only way back if the CLI dies mid-rollout. `retain-failed` keeps failed new containers stopped for their logs.
- **Unit flags.** The setup-written takod unit pins `--build-cache-prune-interval` and `--build-cache-keep-storage` (`provisioner.go:1048`). `tako upgrade servers` replaces only the binary.

### Constraints

- Never delete a rollback target, a running image, a volume, a backup, a retained container, or anything not owned by Tako.
- Shared nodes host many projects and environments. Any global Docker operation (build cache, dangling prune) must already be accepted as shared, as the build-cache loop is today.
- No forced container restarts on upgrade.
- Existing servers only, with no cloud APIs.
- One image selector: v0.14.2's. This change extends it; it does not add a second rule set.

## Goals / Non-Goals

**Goals:** bounded logs by default; image cleanup that runs between deploys with exactly v0.14.2's safety rules; reclaiming space before refusing work; making every automatic deletion visible.

**Non-Goals:**
- Volume GC.
- Backup archive pruning beyond the existing retention.
- Truncating live container log files.
- Rotating proxy or takod logs.
- Rewriting Docker `daemon.json`.
- Retroactive log rotation for running containers.
- Garbage-collecting pulled (`image:`) images, which other projects on the node may share.
- Disk or inode *alerts*: `add-takod-health-monitoring` owns them.
- Metrics history. That belongs to `add-server-status-history`.

## Decisions

### D1. Default log rotation is applied by takod, not written into desired state

When `req.Logging == nil`, takod calls `appendDockerLoggingArgs` with the node default `json-file / 20m / 5`. Other options considered:

- **CLI injects a default into every desired service.** This changes `tako.configHash`, so the first deploy after an upgrade recreates every service. Rejected.
- **Write `daemon.json` in setup.** This needs a Docker restart, which stops every container on the node unless `live-restore` is set. It also overrides operator log shipping. Rejected.
- **Use the `local` driver.** It is compressed, but breaks tools that tail `/var/lib/docker/containers/*/*-json.log`, such as promtail. Using `json-file` keeps the format the daemon default already produces.

**Guard.** At startup and every hour, takod reads `docker info --format '{{.LoggingDriver}}'` and `/etc/docker/daemon.json`, if present. It applies the default only when the driver is `json-file` and `daemon.json` sets neither `log-driver` nor any `log-opts` key. Otherwise operator policy wins (a partial `log-opts` such as only `max-file` is still operator policy). A non-JSON or unreadable `daemon.json` counts as "operator-managed", so the default is skipped and the skip is reported in hygiene status.

**Values.** 20m × 5 is 100 MiB per container. That is enough for incident debugging, and 50 containers stay at 5 GiB or less. Exposed as takod flags `--default-container-log-max-size` and `--default-container-log-max-file`, and disabled with `--default-container-logging=off`.

**Spec and hash.** The default is not part of the desired document or the config hash. A recreated container gets it, and an untouched one keeps its current options. Docker cannot change log options on an existing container, so this is the only non-disruptive path. The hygiene status lists containers still running without rotation, with their log file sizes, so operators can redeploy them deliberately.

### D2. Autonomous image GC reuses v0.14.2's selector

No new selector. Every image removal (deploy-time cleanup, `tako cleanup`, the scheduled GC, the reclaim and dry-run) goes through `planOldImageRemovals` and `readImageCleanupState` from `pkg/takod/image_cleanup.go`, called once per (project, environment) scope. This change adds only what an *unattended* caller needs:

- **Which scopes and repositories.** takod has no `tako.yaml`, so it cannot run `CleanupImageRepositories` itself. Every accepted scoped cleanup request (deploy-time or `tako cleanup`) records its `ImageRepositories` in `<dataDir>/hygiene/scopes/<project>/<env>.json` (atomic, 0600, with `recordedAt`). The autonomous GC iterates over recorded scopes that still have a desired document, with those repositories.
  - A scope with no record (no deploy since the upgrade) gets no autonomous image GC. That leaks, never deletes.
  - Pulled `image:` repositories are never recorded (the CLI never sends them), so they are never candidates. Public repositories like `postgres` are shared across projects, and v0.14.2's ownership evidence is per project.
  - Environment cleanup and preview teardown delete the scope record together with the environment's state.
- **Retention.** `--image-retention` (default **3**) is the N for autonomous runs. It equals the deploy-time `KeepImages` and the `tako cleanup --keep` default, so the GC never removes anything the next deploy-time cleanup would have kept. Scoped requests keep using their own `KeepImages`, exactly as in v0.14.2.
- **Grace (autonomous runs only).** `planOldImageRemovals` gains an optional `createdAfter` cut-off: images created less than 24h ago are kept, with reason `recent`. Scoped deploy-time cleanup runs after its own deploy wrote desired state, inside its lease, and keeps v0.14.2 behaviour (no grace).
- **Scope skips.** An autonomous run skips a scope, recording why, when its operation lease is held by anyone, when the node's worker fence is active, or when a restore-on-failure retention record exists for one of its services. The other scopes still run.
- **Kept reasons.** The planner reports why each candidate was kept: `in use`, `current` (protected ref), `other environment`, `retention`, `recent`. This feeds dry-run and the journal; it does not change any decision.
- **Fail closed** is v0.14.2's: any unreadable document or failed Docker listing removes nothing in that scope, and the run records `skipped: unreadable state <path>` (which feeds the `state_unreadable` alert, D7).

**Out-of-window rollback.** Rolling back to an image beyond N that was built on the node now fails at the existing image preflight. Its message gains: "image was removed by node retention (keeps N per service and environment); redeploy that revision or raise --image-retention". Pulled images are simply pulled again.

### D3. Scoped calls keep v0.14.2 semantics

- `CleanupRequest.KeepImages` keeps v0.14.2's meaning: newest N per repository among the target environment's own images. Nothing in its selection changes.
- The request gains `DryRun bool`; the response gains an additive `Hygiene{Removed, Kept, BytesFreed, DryRun}` with kept reasons.
- **Old nodes.** The CLI sends `CleanOldImages` only to nodes advertising `node.disk-hygiene-v1`. It cannot tell a v0.14.2 node from an older one whose combined-N `rmi -f` selector deletes other environments' rollback targets, and safety wins over disk use:
  - **Deploy-time cleanup:** `CleanOldImages` is dropped for that node, with one warning line per deploy: "old-image cleanup skipped on X: node agent needs tako upgrade servers". Stopped containers, dangling images and build cache are sent as before.
  - **`tako cleanup`:** reports old-image removal as not performed for that server with the same hint. Other cleanup steps still run. The exit code follows the existing ActionResult rules (warnings exit 6).
  - `DryRun` and the reclaim endpoint are never sent without the capability.

### D4. The hygiene loop replaces the build-cache loop

`runHygieneLoop` keeps the existing flags (`--build-cache-prune-interval`, `--build-cache-keep-storage`) working:

- **Hourly "check" tick** (`--hygiene-check-interval`, default 1h). It refreshes the log-driver guard, measures free space on `dataDir` and the Docker data root with `statfs`, and triggers a reclaim at the pressure level it finds (D5).
- **Daily "maintenance" tick** (`--hygiene-interval`, default 24h; takes the old build-cache interval when only that flag is set, which is the case for every setup-written unit). It runs the image GC over all recorded scopes, then the build-cache prune to `--build-cache-keep-storage`.
- **Reclaim signal.** A buffered channel of size 1 that schedulers send to (non-blocking) when admission denies them. It coalesces bursts.

Every run takes the maintenance barrier and the mutation lock, exactly as the current loop does, and skips entirely while the worker fence is active or a node upgrade holds the lifecycle exclusion. These shared locks do **not** detect deploys; per-scope lease checks do (D2). Setting `--hygiene-interval=0` disables autonomous runs. The `/v1/hygiene/reclaim` endpoint and scoped cleanup keep working.

**No unit changes.** Defaults live in the binary. Setup writes none of the new flags, so binary-only upgrades get them and a later default change is not pinned by old units. Overrides are a documented systemd drop-in.

### D5. Reclaim levels are strictly safe and ordered

Two levels, measured on the takod data directory and the Docker data root:

- **Soft:** free space below `max(1.5 × minimumFreeDiskBytes, min(10% of the filesystem, 50 GiB))`. The 50 GiB cap keeps large disks that are mostly volumes from triggering forever.
- **Hard:** free space below `1.5 × minimumFreeDiskBytes`, or the run was triggered by an admission denial or `POST /v1/hygiene/reclaim`.

Steps, each skipped once free space is above the soft threshold:

1. **Dangling images:** `image prune -f` with the compose exclusion, as in `cleanupDanglingImages`. (soft, hard)
2. **Build cache** to `--build-cache-keep-storage`. (soft, hard)
3. **Image GC** (D2) over all recorded scopes. (soft, hard)
4. **Build cache** to `--build-cache-pressure-keep-storage` (default `2GB`). (hard only)
5. **Exited Tako containers** (hard only): labelled `tako.project`, exited or dead, finished more than 1h ago, and not a replica of the current desired revision. One-off containers (exec, jobs, backup helpers) qualify after 24h. Never removed, whatever their age:
   - containers of a scope whose lease is held or that has a restore-on-failure retention record;
   - containers listed in a self-healing converged record (`add-local-self-healing`);
   - exited replicas of the current desired revision (kept for debugging and self-healing).

**Backoff.** A *proactive* run (from the hourly check) that ends still below its level's threshold suppresses further proactive runs at that level for 6 hours and raises `reclaim_insufficient` when below the hard threshold (D7). Admission denials and explicit requests are never backed off. This stops hourly build-cache churn on disks that are full of volumes the reclaim may not touch.

The reclaim never touches:
- volumes or backup archives;
- `/opt/<project>` files;
- running containers;
- non-Tako containers or images, or pulled `image:` repositories;
- container log files.

### D6. Reclaim-and-retry on 507

- **Endpoint.** `POST /v1/hygiene/reclaim {dryRun?: bool}` runs a hard-level reclaim (D5) synchronously. It is bounded at 5 minutes, and only one reclaim runs at a time; a concurrent caller waits for the running one. It returns a `HygieneRun`. The lifecycle gate classifies it like `/v1/cleanup`.
- **Caller's own scope.** The CLI that hit the 507 usually holds its environment's lease, so the reclaim skips that environment's images and containers (D2). Dangling images, build cache and other idle environments are still reclaimed, which is where the space normally is.
- **CLI retry.** In the shared takod client, a 507 whose body starts with `operation denied`, from a node with `node.disk-hygiene-v1`, triggers one reclaim call and one retry of the original request. All current 507 sources of that form are admission checks at handler entry, before any mutation (`writeDiskAdmissionError`, `reserveFreeDisk`); the PostgreSQL verify memory check uses a different message and is excluded. So the retry is safe. The client:
  - retries only requests whose body it can replay;
  - never retries a streamed upload body it cannot replay. For those routes it instead surfaces "freed X; re-run the command".
- **Human output.** One line: `Disk low on app: reclaimed 3.2 GB, retrying…`. With `--events ndjson`, a `hygiene.reclaimed` event (phase `preflight`).
- **Schedulers.** A denied scheduler run (backup, job, certificate) sends the reclaim signal and records the denial as today. Its retry happens on the next schedule, so scheduling semantics don't change.

### D7. Observability

- **Journal.** `<dataDir>/hygiene/journal.json` is a ring of the last 50 `HygieneRun` records:
  - `trigger`: `schedule|pressure-soft|pressure-hard|request|deploy|cleanup|scheduler-denied`
  - `startedAt`, `finishedAt`
  - free bytes before and after, per filesystem
  - per category: removed count and bytes freed (`danglingImages`, `buildCache`, `images`, `containers`)
  - `skipped` reasons, per scope where relevant
  - `dryRun`

  It is written atomically, the same way the other takod JSON state is written. A write failure under disk pressure keeps the run in memory and never aborts the reclaim.
- **`GET /v1/hygiene`.** Returns:
  - the current policy: retention N, intervals, thresholds, and whether the default logging is active plus why;
  - the recorded scopes and their repositories;
  - the last 10 runs;
  - containers running without log rotation, with log bytes (from `docker inspect` `LogPath` plus a file size), capped at 50.
- **CLI:**
  - **`tako cleanup --dry-run`** asks every target server for a scoped dry-run. It prints, per server, what would be removed (image ref, size) and a kept summary grouped by reason (`in use`, `current`, `other environment`, `retention`, `recent`).
  - **`tako doctor`** adds a "Disk hygiene" check per server:
    - **pass:** a run in the last 48h and no pressure;
    - **warn:** containers without rotation whose logs exceed 1 GiB, the logging default skipped because of operator policy, the last run skipped for unreadable state, or no recorded scope (no deploy since upgrade);
    - **fail:** free space below the floor after a reclaim.
  - With `--output json`: `ActionResult` for `cleanup` gains an additive `hygiene` object per server (`removed`, `kept`, `bytesFreed`, `dryRun`). `DoctorResult` gets a new check id, `disk-hygiene`. Both are additive under the MACHINE-INTERFACE compatibility rules.
- **Alerts** go through the alert-source API of `add-takod-health-monitoring` as kind `hygiene` (node-scoped, subscribed by scopes with `notifications.alerts.host`; see D13 there):
  - `reclaim_insufficient` (warning, level, subject `root` or `docker`): free space is still below the hard threshold after a full hard reclaim; recovers once free space is above the soft threshold;
  - `state_unreadable` (warning, level, subject `image-gc`): image GC was skipped because node state could not be read; recovers after the next successful selection.

  A reclaim that freed space is informational and goes to the journal only. `high_disk` and `high_inodes` threshold alerts belong to `add-takod-health-monitoring`.

### D8. Capability and rollout

- takod advertises `node.disk-hygiene-v1` once it has the D2 GC and scope records, the D4 loop, the D6 endpoint and the D1 default.
- The CLI never sends `DryRun`, calls the reclaim endpoint or sends `CleanOldImages` without the capability.
- Rollout is `tako upgrade servers`, then one deploy per environment (which records its scope). The first scheduled GC runs at most 5 minutes after the upgraded takod starts, it has no scopes until a deploy records one, and its image step is still bound by the 24h grace. An upgrade therefore never removes images that a freshly started deploy needs.

## Risks / Trade-offs

- **Deleting a rollback target (main risk).** Mitigations:
  - the selection rules are v0.14.2's, already reviewed and shipped;
  - protection spans all projects and environments on the node;
  - the selection fails closed on unreadable state;
  - removal uses `rmi` without `-f`;
  - N equals the deploy-time N, plus a 24h grace for unattended runs;
  - per-scope lease and retention skips;
  - dry-run in the CLI.

  Required tests:
  - a staging-scoped autonomous run with production history on the same node removes nothing of production's;
  - project rollback sources (any history status) stay owned and counted;
  - an image of the current desired or actual state of another project is kept;
  - an unreadable `history.json` means zero removals in that scope;
  - an image used only by a stopped non-Tako container is kept;
  - a scope with a held lease or a retention record is skipped.
- **History is capped at 50 entries.** Ownership comes from history *and* the unbounded desired archive, so an image whose history entry was evicted is still owned through the archive and keeps counting toward N. Nothing becomes unowned and removable just because history rolled over.
- **Scopes without a record.** They get no autonomous GC until the next deploy. That is safe (leaked, not deleted), and doctor warns about it.
- **Old nodes stop getting deploy-time old-image cleanup** when the CLI is newer than the node. Disk can grow until `tako upgrade servers`; every deploy says so. Accepted: the alternative risks deleting rollback targets on pre-v0.14.2 nodes.
- **Default logging on a node whose operator wants unbounded logs.** Mitigated by the `daemon.json` guard and `--default-container-logging=off`.
- **Retry cost.** A 507 now adds up to 5 minutes before the command fails, against an instant failure today. It is bounded and visible.

## Migration / Compatibility

- **Config:** no changes. `logging:` keeps its meaning and always wins.
- **Machine interface:** additive fields and event only.
- **Old CLI against a new takod:** the old CLI sends `CleanOldImages` with v0.14.2 semantics, which this takod applies unchanged (and records the scope). Safe.
- **New CLI against an old takod:** old-image removal is skipped or reported as not performed (D3).
- **Existing containers:** unchanged until they are recreated.

## Resolved questions

1. **Retention N.** **3 per repository and environment** (`--image-retention`), not 5. *Rationale: it equals the deploy-time and `tako cleanup --keep` default, so autonomous runs never remove what the next deploy would keep, and v0.14.2 already made it per environment.*
2. **GC pulled (`image:`) images too?** **No.** *Rationale: their repositories are shared across projects and v0.14.2's ownership evidence is per project, so the selector cannot prove another project does not need them.*
3. **Project-level `runtime.logging` default in `tako.yaml`?** **Not in this change.** *Rationale: it would change config hashes and force recreates; per-service `logging:` plus the node default covers it.*
4. **Truncate oversized unrotated logs under pressure?** **No.** *Rationale: destructive to incident evidence and racy with Docker's reader; doctor reports it instead.*
5. **`--no-reclaim` flag?** **No.** *Rationale: the reclaim removes only what scheduled hygiene would; add the flag only if someone needs instant failure.*
6. **Configurable 24h grace?** **No flag**, and it applies to unattended runs only. *Rationale: it is a safety margin, and scoped cleanup keeps v0.14.2 behaviour.*
7. **Write `daemon.json` (with `live-restore`) on fresh hosts?** **No.** *Rationale: fresh-host detection is unreliable, and the takod default covers Tako containers.*
8. **New selector or v0.14.2's?** **v0.14.2's**, extended only with scope records, a grace cut-off and kept reasons. *Rationale: one reviewed rule set; the earlier draft that protected every archived desired revision would never have removed anything.*
9. **Setup writes the new flags into the unit?** **No.** *Rationale: binary defaults reach upgraded nodes; pinned unit flags would freeze today's defaults.*

## Implementation notes

Where the plan and the code disagreed, the implementation takes the safest equivalent:

- **Alert subjects** are the plain `root`, `docker` and `image-gc` (monitoring D13), with event names `disk_reclaim_insufficient` / `resource_normal` and `image_gc_blocked` / `image_gc_resumed`. Detail keys are `freeBytes`, `softBytes`, `hardBytes` and `unreadableScopes`.
- **Locks.** Autonomous runs (schedule, pressure, scheduler denial) take the maintenance barrier, check the worker fence and take the mutation lock, in that order, before taking the single run slot. `POST /v1/hygiene/reclaim` takes no extra lock, because the lifecycle gate already holds the mutation lock for it like `/v1/cleanup`. For a request reclaim, an active worker fence skips only the image and container steps. Loop runs skip entirely and journal the skip.
- **Self-healing converged records** are read from `<dataDir>/heal/<project>/<environment>/services/*.json` when present. A container named there is never removed.
- **`--hygiene-interval=0`** (or a legacy `--build-cache-prune-interval=0` without `--hygiene-interval`) disables all autonomous runs, including the hourly pressure check and the scheduler-denial signal. The logging guard still refreshes hourly. The reclaim endpoint and scoped cleanup keep working.
- **Spec arithmetic.** The "Soft pressure" scenario said 14 GiB free is above the 15 GiB hard threshold. It is corrected to 17 GiB, which is between hard (15 GiB) and soft (20 GiB).
- **507 detection.** The node sends no header hint. The client recognises a body starting with `operation denied` and checks the capability through `GET /v1/status` before reclaiming. A node without the capability gets no reclaim call. Reclaim calls and the status probe never trigger a nested reclaim.
- **Bytes freed** are measured as the statfs free-space delta, with the removed image sizes as the fallback. A dry-run reclaim reports the image and container selection, but it does not simulate the dangling-image or build-cache prunes.
- **`CleanupRequest.Trigger`** (`deploy` | `cleanup`) is an additive field that labels journal entries. The CLI sets it only for capable nodes.
- **`ReconcileServiceRequest.DefaultLogging`** is server-set (`json:"-"`) from the logging guard, never read from the body, and never part of the desired document or `tako.configHash`.
- **Early stop** applies to every reclaim, including requests: once free space is above soft, the remaining steps are skipped.
- **CLI.**
  - Deploy-time cleanup checks the capability only when the deploy has image repositories to clean. It emits one warning per deploy that names every old node.
  - For `tako cleanup`, the old-node skip is a per-server warning, so the run is `partial` (exit 6, as D3 says). A dry run on an old node is reported as that server's `error`.
  - `--dry-run` still takes the cleanup leases, so it observes a consistent state.
  - In doctor, logging turned off with `--default-container-logging=off` is not a warning; only operator-managed daemon logging is. The check also warns on pressure, on no run in 48h, or on disabled autonomous runs.
  - The rollback hint says the image *may* have been removed by node retention ("keeps 3 per service and environment by default"), because the CLI cannot read a node's `--image-retention`.
- **Verification limits.** The takod package tests run in CI on Linux. Locally they ran on Windows with a temporary shim for the Unix-only helpers. The manual test-server checks (5.4) were not run in this change and are left to release validation.
- **Review follow-ups (ops integration):**
  - *Scheduler-denied reclaim backoff.* A reclaim triggered by a scheduler admission denial runs at most once per 15 minutes. One that ends below the hard threshold backs off further denial-triggered reclaims for the proactive backoff (6h). Skipped denials are not journaled. Explicit requests are never backed off.
  - *Logging probe.* A failed Docker probe keeps the last good policy (the reason notes the failure) and retries after 1 minute instead of waiting for the next hourly run. Operator-managed logging is also detected from the snap `daemon.json` (`/var/snap/docker/current/config/daemon.json`), a `--config-file` path that replaces the default, and `--log-driver`/`--log-opt` flags on the running dockerd (from `/proc`, else the systemd `ExecStart`).
  - *Retain-failed containers.* A `failurePolicy: retain-failed` reconcile writes `<dataDir>/retain-failed/<project>/<environment>/<service>.json` naming the kept containers. Reclaim never removes them, and it skips the whole scope when that record is unreadable. The next successful reconcile of the service clears the record. Caveat: revision-scoped strategies keep their failed revision by name and never write the record, so their failed containers rely on the revision ownership checks.
  - *Severity.* `reclaim_insufficient` is a **warning**. Monitoring's `high_disk` is already critical for the same full disk, so one condition pages once instead of twice. The two alerts stay separate: `high_disk` says the disk is full, `reclaim_insufficient` says automatic reclaim cannot fix it.
  - *Image state scan.* Image ownership state is read with `ReadDir` per path segment and fails closed: any error other than "does not exist" stops image GC for that run.
