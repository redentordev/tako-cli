# Proposal

## Why

Disks fill up on Tako nodes in three ways that Tako does not handle well today:

- **Unbounded container logs.** Rotation exists only when a service sets `logging:` (`pkg/takod/reconcile.go:1355-1364`). Without it, takod passes no log options, so Docker's default `json-file` driver grows each log forever. Setup does not write `/etc/docker/daemon.json` either.
- **Images are only cleaned during a deploy or `tako cleanup`.** v0.14.2 made that cleanup safe (`pkg/takod/image_cleanup.go`, `cleanupOldProjectImages`/`planOldImageRemovals`): it keeps the newest N images per repository among the target environment's own images, as named by the node's desired revisions and deployment history; it never removes an image another environment, any current desired or actual state, or any container uses; it removes by ID without `-f`; and it removes nothing when that state cannot be read. But nothing runs it between deploys, so a node that stops deploying, or whose disk fills from other causes, keeps every image until someone runs `tako cleanup`.
- **Low disk only refuses work.** `checkFreeDisk` (`pkg/takod/server.go:2148`) rejects operations below `minimumFreeDiskBytes` (10 GiB by default, `pkg/platform/types.go:54`). Nothing tries to free space first, and nothing acts before the floor is reached. The only autonomous cleanup is the 24h build-cache prune (`server.go:659-712`).

## What Changes

- **Default log rotation.** When a service has no `logging:` block, takod applies `json-file` with `max-size=20m`, `max-file=5` (at most 100 MiB per container). It skips this when the Docker daemon's default driver is not `json-file`, or when `daemon.json` sets `log-driver` or any `log-opts`. An explicit `logging:` block always wins. The default is not part of the desired document or the config hash, so upgrading takod forces no restarts. Existing containers pick it up on their next recreate. Setup does **not** write `daemon.json`.
- **Autonomous image GC built on v0.14.2's selector.** No new selector is introduced. The scheduled GC and the reclaim call the same `planOldImageRemovals` for each (project, environment) on the node, with:
  - the built-image repositories the CLI last sent for that environment in a deploy-time or `tako cleanup` request, which takod now records (pulled `image:` repositories are never candidates);
  - the node retention `--image-retention` (default **3**, the same N that deploy-time cleanup and `tako cleanup --keep` use);
  - one extra protection for autonomous runs only: images created less than 24h ago are kept;
  - environments whose operation lease is held, or that have a pending restore-on-failure retention, are skipped.

  Everything v0.14.2 guarantees (per-environment ownership, cross-environment and current-state protection, in-use protection, `rmi` by ID without `-f`, fail closed) carries over unchanged. Deploy-time cleanup and `tako cleanup` keep v0.14.2's request semantics; they gain only `DryRun` and an additive report.
- **Autonomous hygiene loop.** The existing build-cache loop becomes a hygiene loop. It does the daily image GC and build-cache prune, plus an hourly cheap disk check with two pressure levels:
  - **soft** (free space below `max(1.5 × minimumFreeDiskBytes, min(10% of the filesystem, 50 GiB))`): runs the daily maintenance early (dangling images, build cache to its normal budget, image GC);
  - **hard** (free space below `1.5 × minimumFreeDiskBytes`, an admission denial, or an explicit request): additionally prunes build cache to a small pressure budget and removes eligible exited Tako containers.

  A proactive run that cannot clear its level backs off for 6 hours. A reclaim never touches volumes, backups, logs, non-Tako resources, retained or self-healing containers, or environments with a held lease.
- **Reclaim before refusing.** When a takod endpoint denies work with HTTP 507 (`writeDiskAdmissionError`), the CLI calls the new `POST /v1/hygiene/reclaim`, waits up to 5 minutes, and retries the request once. Scheduler denials (backup, job, certificate) signal the loop to run a reclaim asynchronously. Admission itself never cleans synchronously, which avoids lock nesting.
- **Observability.** takod keeps a hygiene journal (the last 50 runs: trigger, bytes freed per category, items removed, skip reasons), exposed at `GET /v1/hygiene`. The CLI gets:
  - `tako cleanup --dry-run`, which shows what the selector would remove and why the rest is kept;
  - a "Disk hygiene" section in `tako doctor` (last run, freed bytes, containers still without log rotation and their log sizes);
  - additive machine output.

  Alerts go through the source API of `add-takod-health-monitoring` (kind `hygiene`: `reclaim_insufficient`, `state_unreadable`; design D13 there). That change owns the `high_disk` and `high_inodes` threshold alerts; this one never raises them.
- **Gating and opt-out.** All of this needs takod capability `node.disk-hygiene-v1`. Defaults live in the takod binary; operators override them with a systemd drop-in: `--hygiene-interval` (`0` disables the loop), `--image-retention`, `--default-container-logging=off`. Setup does not write these flags, so binary-only upgrades get them and later default changes are not pinned. Against nodes without the capability, deploy-time cleanup omits `CleanOldImages`, because the CLI cannot tell a v0.14.2 node from an older one that still runs the unsafe combined-N selector; `tako cleanup` reports old-image removal as skipped there with a `tako upgrade servers` hint.

## Capabilities

### New Capabilities

- `disk-hygiene`: default container log rotation, autonomous image GC on top of the v0.14.2 selector, the hygiene loop and pressure reclaim, the reclaim-and-retry admission path, and hygiene observability.

### Modified Capabilities

None. No existing spec covers image cleanup, log rotation or disk admission.

## Impact

- **Prerequisite:** v0.14.2's `pkg/takod/image_cleanup.go` (and `CleanupProjectWithDataDir`) must be on master; this change is rebased on it and extends it rather than replacing it.
- **takod:**
  - `pkg/takod/image_cleanup.go`: an optional `createdAfter` grace and kept-image reasons in `planOldImageRemovals`; a dry-run path; nothing else changes in the selection rules.
  - `pkg/takod/cleanup.go`: records the request's `ImageRepositories` per (project, environment); exited-container reclaim with the D5 exclusions.
  - `pkg/takod/server.go`: the hygiene loop replaces `runBuildCachePruneLoop`; new routes, capability, and flags; 507 responses gain a reclaim hint.
  - `pkg/takod/reconcile.go`: default logging args.
  - New files `pkg/takod/hygiene.go`, `hygiene_scopes.go`, `hygiene_journal.go`.
- **CLI:**
  - `cmd/cleanup.go`: `--dry-run`, capability handling.
  - `cmd/doctor.go`: hygiene section.
  - `cmd/takod.go`: flags.
  - `pkg/engine/deploy.go` (`:1255-1292`): deploy-time cleanup gating.
  - Takod client: 507 → reclaim → retry once.
- **Setup:** none. The new flags are takod defaults, documented as a systemd drop-in.
- **Docs:**
  - `docs/MACHINE-INTERFACE.md`: `ActionResult.hygiene`, `HygieneReport`.
  - `docs/CONFIGURATION.md`: logging default.
  - `docs/RESOURCE-MONITORING.md`.
  - `tako cleanup` help.
- **Dependencies:**
  - `add-takod-health-monitoring` lands first and provides the alert-source API; this change registers kind `hygiene` with it.
  - Independent of `add-server-status-history`, which may chart the journal's freed bytes later.
  - `add-local-self-healing` lands after this change; the reclaim never removes containers listed in a self-healing converged record, nor exited replicas of the current desired revision.
