# Design

## Context

### What exists

- **Log rotation.** `ServiceLoggingConfig{Driver, MaxSize, MaxFiles}` (`pkg/config/types.go:416-422`) is opt-in per service. It is validated to `local|json-file`, at most 10g and 1–100 files (`validator.go:1897-1927`). takod adds `--log-driver/--log-opt` only when `req.Logging != nil` (`reconcile.go:1355-1364`), gated by `container.logging-v1`. Without it, the daemon default applies. On stock Docker that is `json-file` with no size limit, and Tako setup never writes `/etc/docker/daemon.json`.
- **Image cleanup.**
  - Deploy-time cleanup sends `CleanupRequest{CleanOldImages, KeepImages: 3, ImageRepositories}` to every target server after success (`pkg/engine/deploy.go:1255-1292`).
  - Repositories come from `CleanupImageRepositories` (`pkg/engine/takod.go:257-284`). It covers built and shared-build services only; pulled `image:` services are excluded.
  - takod's `cleanupOldProjectImages` (`cleanup.go:396-435`) lists every image in those repositories (newest first), keeps the first `KeepImages` across all of them combined, and runs `rmi -f` on the rest.
  - Repositories are `[registry/]<project>/<service>` (`types.go:1175-1193`), with the environment only in the tag.
  - Preview deploys skip `CleanOldImages` on servers shared with declared environments (`cleanOldImagesOn`).
  - `tako cleanup` uses the same path with `--keep` (default 3, or 2 with `--full`).
- **Image ownership.** Images takod builds carry no Tako label (`image.go:411`, `docker build -t image`). Compose images are excluded by `label!=com.docker.compose.project`.
- **Node state documents.** All of these live under `<dataDir>`:
  - `desired/<project>/<env>/revision.json`
  - retained `desired/<project>/<env>/revisions/<id>.json`
  - `history/<project>/<env>/history.json` and `deployments/<id>.json` (`pkg/takod/state.go:279-291`)

  History is capped at 50 entries (`internal/state/manager.go:18`). Service image refs live in `DesiredServiceDocument.Image` and `ServiceStateDocument.Image` (`pkg/takoapi/state.go`).
- **Autonomous loops.** `runBuildCachePruneLoop` runs every 24h, with the first run after up to 5 minutes. It holds the maintenance barrier, checks the worker operation fence and takes the mutation lock (`server.go:659-712`).
- **Disk admission.**
  - `checkFreeDisk`, `requireFreeDisk` and `reserveFreeDisk` (`server.go:2137-2300`) deny work below `minimumFreeDiskBytes` with HTTP 507, or 503 when the probe fails.
  - Schedulers admit through `checkFreeDisk` (`server.go:293-295`).
  - The platform worker has its own `AdmissionController` (`pkg/platform/admission.go`).

### Constraints

- Never delete a rollback target, a running image, a volume, a backup, or anything not owned by Tako.
- Shared nodes host many projects and environments. Any global Docker operation (build cache, dangling prune) must already be accepted as shared, as the build-cache loop is today.
- No forced container restarts on upgrade.
- Existing servers only, with no cloud APIs.

## Goals / Non-Goals

**Goals:** bounded logs by default; an image cleanup that cannot remove rollback targets; reclaiming space before refusing work; making every automatic deletion visible.

**Non-Goals:**
- Volume GC.
- Backup archive pruning beyond the existing retention.
- Truncating live container log files.
- Rotating proxy or takod logs.
- Rewriting Docker `daemon.json`.
- Retroactive log rotation for running containers.
- Metrics history. That belongs to `add-server-status-history`.

## Decisions

### D1. Default log rotation is applied by takod, not written into desired state

When `req.Logging == nil`, takod calls `appendDockerLoggingArgs` with the node default `json-file / 20m / 5`. Other options considered:

- **CLI injects a default into every desired service.** This changes `tako.configHash`, so the first deploy after an upgrade recreates every service. Rejected.
- **Write `daemon.json` in setup.** This needs a Docker restart, which stops every container on the node unless `live-restore` is set. It also overrides operator log shipping. Rejected.
- **Use the `local` driver.** It is compressed, but breaks tools that tail `/var/lib/docker/containers/*/*-json.log`, such as promtail. Using `json-file` keeps the format the daemon default already produces.

**Guard.** At startup and every hour, takod reads `docker info --format '{{.LoggingDriver}}'` and `/etc/docker/daemon.json`, if present. It applies the default only when the driver is `json-file` and `log-opts.max-size` is unset. Otherwise operator policy wins. A non-JSON or unreadable `daemon.json` counts as "operator-managed", so the default is skipped and the skip is reported in hygiene status.

**Values.** 20m × 5 is 100 MiB per container. That is enough for incident debugging, and 50 containers stay at 5 GiB or less. Exposed as takod flags `--default-container-log-max-size` and `--default-container-log-max-file`, and disabled with `--default-container-logging=off`.

**Spec and hash.** The default is not part of the desired document or the config hash. A recreated container gets it, and an untouched one keeps its current options. Docker cannot change log options on an existing container, so this is the only non-disruptive path. The hygiene status lists containers still running without rotation, with their log file sizes, so operators can redeploy them deliberately.

### D2. One image selector, used everywhere

`selectImageRemovals(ctx, scope) (Plan, error)` lives in `pkg/takod/hygiene_images.go`. Deploy-time cleanup, `tako cleanup`, the scheduled GC, the reclaim and dry-run all use it, and `cleanupOldProjectImages` is deleted.

**Candidate set C.** These are local images, tagged or dangling, with all of:
- no `com.docker.compose.project` label;
- a repository in `R`, where `R` is the set of repositories of every image ref found in any desired, retained-revision or history document on the node, across all projects and environments;
- for scoped calls (deploy or `tako cleanup`), a repository also in the request's `ImageRepositories`.

Images whose repository Tako has never referenced on this node are never candidates. That protects non-Tako images, including `mongo:7` pulled outside Tako.

**Protected set P,** resolved to image IDs with one batched `docker image inspect`:
1. **In use:** the image of every container on the node, in any state and from any owner (`docker ps -a --format '{{.ImageID}}'`).
2. **Desired:** every service image in every `desired/*/*/revision.json` and every retained `revisions/*.json`, for every project and environment. This covers previews, blue-green inactive revisions, warmed promotions and preview bases.
3. **Rollback window:** for each (project, environment, service), the images of the newest `K` distinct image refs in that environment's history, ordered by deployment time. Records of any status count, because a failed deploy can still be a project-rollback source. Default `K = 5`. The takod flag is `--image-retention`, and the request field `KeepImages` overrides it for scoped calls but cannot go below 1.
4. **Grace:** images created less than 24h ago. This covers in-flight deploys that built or pulled an image but haven't written desired state yet.
5. **Active operation:** if any mutation lock, worker operation or deploy lease journal is active on the node, the *unscoped* GC is skipped entirely. Scoped deploy-time cleanup runs in the deploy's own lease, so only the protected set applies to it.

**Removal.** `R_remove = C − P`. Removal is by image ID with `docker rmi <id>`, without `-f`. A Docker conflict, such as an image used by a container created between list and remove, is a skip with a reason, never a forced delete.

**Fail-closed.** If any desired or history document fails to parse, or `docker ps -a` or `docker image inspect` fails, the whole selection returns an error and removes nothing. The journal records `skipped: unreadable state <path>`.

**Out-of-window rollback.** Rolling back to an image beyond `K` that was built on the node now fails at the existing image preflight. Its message gains: "image was removed by node retention (keeps K per service); redeploy that revision or raise --image-retention". Pulled images are simply pulled again.

### D3. Scoped calls change meaning safely

- `CleanupRequest.KeepImages` now means "per (project, environment, service) rollback window". It used to mean the newest N across the combined repositories.
- `ImageRepositories` stays a filter on C, but protection always considers *all* projects and environments on the node. A staging cleanup can no longer remove production's rollback targets.
- The request gains `DryRun bool`; the response gains `ImagesKept []KeptImage{ID, Repository, Reason}` and `BytesFreed`.
- The CLI sends the new semantics only to nodes advertising `node.disk-hygiene-v1`. Old nodes:
  - **Deploy-time cleanup:** `CleanOldImages` is dropped from the request, and one debug-level note says "old-image cleanup skipped: node needs tako upgrade servers". Stopped containers, dangling images and build cache are sent as before.
  - **`tako cleanup`:** fails old-image removal for that server with a warning and suggests `tako upgrade servers`. Other cleanup steps still run. The exit code follows the existing ActionResult rules.

### D4. The hygiene loop replaces the build-cache loop

`runHygieneLoop` keeps the existing flags (`--build-cache-prune-interval`, `--build-cache-keep-storage`) working:

- **Hourly "check" tick** (`--hygiene-check-interval`, default 1h). It refreshes the log-driver guard, measures free space on `dataDir` and the Docker data root, and triggers a **reclaim** when free space is below the proactive threshold `max(1.5 × minimumFreeDiskBytes, 10% of the filesystem)`.
- **Daily "maintenance" tick** (`--hygiene-interval`, default 24h; takes the old build-cache interval when only that flag is set). It runs the unscoped image GC, then the build-cache prune to `--build-cache-keep-storage`.
- **Reclaim signal.** A buffered channel of size 1 that schedulers send to (non-blocking) when admission denies them. It coalesces bursts.

Every run takes the maintenance barrier, checks `activeWorkerOperation`, and takes the mutation lock, exactly as the current loop does. If any of these is busy, the run is skipped and retried at the next tick (or after 5 minutes for a reclaim). Setting `--hygiene-interval=0` disables autonomous runs. The `/v1/hygiene/reclaim` endpoint and scoped cleanup keep working.

### D5. Reclaim levels are strictly safe and ordered

Each step stops as soon as free space is at least the proactive threshold:

1. **Dangling images:** `image prune -f` with the compose exclusion, as in `cleanupDanglingImages`.
2. **Build cache:** prune to `--build-cache-keep-storage`. If still under the threshold, prune to the pressure budget `--build-cache-pressure-keep-storage` (default `2GB`).
3. **Retention-eligible images,** using the D2 selector, unscoped.
4. **Exited Tako containers.** These are containers labelled `tako.project` that are exited or dead, finished more than 1h ago, and are not replicas of their environment's current desired revision. One-off containers (exec, jobs, backup helpers) qualify after 24h. Crashed replicas of the current revision are kept for debugging and for `add-local-self-healing`.

The reclaim never touches:
- volumes or backup archives;
- `/opt/<project>` files;
- running containers;
- non-Tako containers or images;
- container log files.

### D6. Reclaim-and-retry on 507

- **Endpoint.** `POST /v1/hygiene/reclaim {dryRun?: bool}` runs D5 synchronously. It is bounded at 5 minutes, and only one reclaim runs at a time; a concurrent caller waits for the running one. It returns a `HygieneRun`.
- **CLI retry.** In the shared takod client (`nodeclient`), a 507 response from a node with `node.disk-hygiene-v1` triggers one reclaim call and one retry of the original request. All current 507 sources are admission checks at handler entry, before any mutation (`writeDiskAdmissionError`, `reserveFreeDisk`, plus the PostgreSQL verify memory check, which is excluded by matching the message prefix `operation denied`). So the retry is safe. The client:
  - retries only idempotent-at-admission routes, which is every route that returns 507 today;
  - never retries a streamed upload body it cannot replay. For upload routes it instead surfaces "freed X; re-run the command".
- **Human output.** One line: `Disk low on app: reclaimed 3.2 GB, retrying…`. With `--events ndjson`, a `hygiene.reclaimed` event (phase `preflight`).
- **Schedulers.** A denied scheduler run (backup, job, certificate) sends the reclaim signal and records the denial as today. Its retry happens on the next schedule, so scheduling semantics don't change.

### D7. Observability

- **Journal.** `<dataDir>/hygiene/journal.json` is a ring of the last 50 `HygieneRun` records:
  - `trigger`: `schedule|pressure|request|deploy|cleanup|scheduler-denied`
  - `startedAt`, `finishedAt`
  - free bytes before and after, per filesystem
  - per category: removed count and bytes freed (`danglingImages`, `buildCache`, `images`, `containers`)
  - `skipped` reasons
  - `dryRun`

  It is written atomically, the same way the other takod JSON state is written.
- **`GET /v1/hygiene`.** Returns:
  - the current policy: retention `K`, intervals, thresholds, and whether the default logging is active plus why;
  - the last 10 runs;
  - containers running without log rotation, with log bytes (from `docker inspect` `LogPath` plus a file size), capped at 50.
- **CLI:**
  - **`tako cleanup --dry-run`** asks every target server for a scoped dry-run. It prints, per server, what would be removed (image ref, size) and a kept summary grouped by reason (`in use`, `desired`, `rollback window`, `recent`).
  - **`tako doctor`** adds a "Disk hygiene" check per server:
    - **pass:** a run in the last 48h and no pressure;
    - **warn:** containers without rotation whose logs exceed 1 GiB, the logging default skipped because of operator policy, or the last run skipped for unreadable state;
    - **fail:** free space below the floor after a reclaim.
  - With `--output json`: `ActionResult` for `cleanup` gains an additive `hygiene` object per server (`removed`, `kept`, `bytesFreed`, `dryRun`). `DoctorResult` gets a new check id, `disk-hygiene`. Both are additive under the MACHINE-INTERFACE compatibility rules.
- **Alerts.** Hygiene emits the following through the alert-source interface defined by `add-takod-health-monitoring`:
  - `disk.reclaimed`: informational;
  - `disk.reclaim_insufficient`: free space is still below the floor after a full reclaim;
  - `hygiene.skipped_unreadable_state`: warning.

  `high_disk` threshold alerts belong to that change. If it has not landed, hygiene records these only in the journal and as takod state events.

### D8. Capability and rollout

- takod advertises `node.disk-hygiene-v1` once it has the D2 selector, the D4 loop, the D6 endpoint and the D1 default.
- The CLI never sends `DryRun` or relies on new `KeepImages` semantics without the capability.
- Rollout is `tako upgrade servers`. The first scheduled GC runs at most 5 minutes after the upgraded takod starts, and the first *image* GC is still bound by the 24h grace. An upgrade therefore never removes images that a freshly started deploy needs.

## Risks / Trade-offs

- **Deleting a rollback target (main risk).** Mitigations:
  - protection spans all projects and environments on the node;
  - the selection fails closed on unreadable state;
  - removal uses `rmi` without `-f`;
  - the 24h grace and the per-service `K` window;
  - the active-operation skip;
  - dry-run in the CLI.

  Required tests:
  - a staging-scoped cleanup with production history on the same node removes nothing of production's;
  - project rollback sources are kept;
  - a retained revision is kept;
  - an unreadable `history.json` means zero removals;
  - an image used only by a stopped non-Tako container is kept.
- **History is capped at 50 entries.** A service that deploys rarely in a busy environment may have fewer than `K` entries. The window counts per service, so this only loses images whose history entries are gone, and those were never rollback targets anyway (`rollback_target.go:178-244`).
- **Repositories referenced only by evicted history.** These leave R, and their images become invisible to the GC. That is safe (leaked, not deleted) and can be removed with an explicit `tako cleanup`.
- **Default logging on a node whose operator wants unbounded logs.** Mitigated by the `daemon.json` guard and `--default-container-logging=off`.
- **Behaviour change in `tako cleanup --keep`.** It now means per service per environment, so cleanup removes fewer images than before on multi-service projects. That direction is safe, and the help text is corrected.
- **Retry cost.** A 507 now adds up to 5 minutes before the command fails, against an instant failure today. It is bounded and visible, and `--no-reclaim` is not added (see Open questions).

## Migration / Compatibility

- **Config:** no changes. `logging:` keeps its meaning and always wins.
- **Machine interface:** additive fields and event only.
- **Old CLI against a new takod:** the old CLI still sends `CleanOldImages` with combined-N intent. The new takod interprets it with D2 semantics, which removes the same or fewer images. That is safe.
- **New CLI against an old takod:** old-image removal is skipped or refused (D3).
- **Existing containers:** unchanged until they are recreated.

## Open questions

1. **Default rollback window `K`.** *Recommendation: 5 per (project, environment, service).* It is more generous than today's effective "3 combined", it's cheap on typical image sizes, and it can be configured per node.
2. **Should pulled images (`image:` services) be GC'd too?** *Recommendation: yes.* Same selector, same protections. They re-pull on rollback if the registry still has them, and the `K` window keeps recent ones local regardless. Today they are never cleaned and accumulate.
3. **Add a project-level `runtime.logging` default in `tako.yaml`?** *Recommendation: not in this change.* Per-service `logging:` plus node policy covers it, and a project default would change config hashes and force recreates.
4. **Truncate oversized unrotated container logs under critical pressure?** *Recommendation: no.* It is destructive to incident evidence and racy with Docker's reader. Report it in `doctor`, and the operator redeploys.
5. **Add a `--no-reclaim` CLI flag to skip the 507 reclaim-and-retry?** *Recommendation: no.* The reclaim only removes things the scheduled GC would remove anyway. Add the flag later if someone needs instant failure.
6. **Should the 24h image grace be configurable?** *Recommendation: no flag in v1.* It is a safety margin, not a tuning knob.
7. **Write `daemon.json` with `live-restore` and log defaults during `tako setup` on fresh hosts only?** *Recommendation: no.* Fresh-host detection is unreliable, and the takod-side default (D1) already covers Tako containers.
