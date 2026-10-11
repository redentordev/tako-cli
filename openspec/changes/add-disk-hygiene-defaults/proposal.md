# Proposal

## Why

Disks fill up on Tako nodes in three ways that Tako does not handle well today:

- **Unbounded container logs.** Rotation exists only when a service sets `logging:` (`pkg/takod/reconcile.go:1355-1364`). Without it, takod passes no log options, so Docker's default `json-file` driver grows each log forever. Setup does not write `/etc/docker/daemon.json` either.
- **Images are only cleaned during a deploy or `tako cleanup`.** And that cleanup is unsafe:
  - `cleanupOldProjectImages` (`pkg/takod/cleanup.go:396-435`) keeps the newest N images *across all repositories in the request combined*. `tako cleanup` claims it keeps "N latest images per service" (`cmd/cleanup.go:130`).
  - Repositories are `<project>/<service>`, with the environment only in the tag (`config.GetFullImageNameWithTag`, `types.go:1175-1193`). A staging deploy on a server that also runs production therefore counts and removes production's images.
  - Nothing consults deployment history. A rollback target built on the node can be deleted (`rmi -f`) while it is still listed as a rollback candidate.
- **Low disk only refuses work.** `checkFreeDisk` (`pkg/takod/server.go:2148`) rejects operations below `minimumFreeDiskBytes` (10 GiB by default, `pkg/platform/types.go:54`). Nothing tries to free space first, and nothing acts before the floor is reached. The only autonomous cleanup is the 24h build-cache prune (`server.go:659-712`).

## What Changes

- **Default log rotation.** When a service has no `logging:` block, takod applies `json-file` with `max-size=20m`, `max-file=5` (at most 100 MiB per container). It skips this when the Docker daemon's default driver is not `json-file`, or when `daemon.json` already sets `log-opts.max-size`. An explicit `logging:` block always wins. The default is not part of the desired document or the config hash, so upgrading takod forces no restarts. Existing containers pick it up on their next recreate. Setup does **not** write `daemon.json`.
- **Retention-aware image cleanup in takod.** A new node-local image selector decides which images may go. It removes only images whose repository Tako has referenced on this node. It never removes an image that is:
  - used by any container on the node (any state, any owner);
  - referenced by any current or retained desired revision;
  - among the newest `K` distinct images per (project, environment, service) in deployment history (default `K = 5`);
  - created less than 24h ago.

  It fails closed when any state document cannot be read. The same selector replaces `CleanOldImages` in deploy-time cleanup and `tako cleanup`, so `--keep` now really means per service, per environment.
- **Autonomous hygiene loop.** The existing build-cache loop becomes a hygiene loop. It does the daily image GC and build-cache prune, plus an hourly cheap disk check. When free space drops under a proactive threshold (`1.5 × minimumFreeDiskBytes`, or 90% used), it runs a **reclaim**. A reclaim prunes build cache, dangling images, retention-eligible images, and exited Tako containers that are not part of the current desired revision. It never touches volumes, backups, or non-Tako resources.
- **Reclaim before refusing.** When a takod endpoint denies work with HTTP 507 (`writeDiskAdmissionError`), the CLI calls the new `POST /v1/hygiene/reclaim`, waits up to 5 minutes, and retries the request once. Scheduler denials (backup, job, certificate) signal the loop to run a reclaim asynchronously. Admission itself never cleans synchronously, which avoids lock nesting.
- **Observability.** takod keeps a hygiene journal (the last 50 runs: trigger, bytes freed per category, items removed, skip reasons), exposed at `GET /v1/hygiene`. The CLI gets:
  - `tako cleanup --dry-run`, which shows what the selector would remove and why the rest is kept;
  - a "Disk hygiene" section in `tako doctor` (last run, freed bytes, containers still without log rotation and their log sizes);
  - additive machine output.

  Cleanup events go to the alert-source interface owned by `add-takod-health-monitoring`, which owns the `high_disk` alert.
- **Gating and opt-out.** All of this needs takod capability `node.disk-hygiene-v1`. takod flags, written by setup into the systemd unit, control it: `--hygiene-interval` (`0` disables the loop), `--image-retention`, `--default-container-logging=off`. Against nodes without the capability, deploy-time cleanup **stops sending `CleanOldImages`**, because the old selector is the unsafe one. `tako cleanup` refuses old-image removal on those nodes and suggests `tako upgrade servers`.

## Capabilities

### New Capabilities

- `disk-hygiene`: default container log rotation, a retention-aware image selector, the autonomous hygiene loop and pressure reclaim, the reclaim-and-retry admission path, and hygiene observability.

### Modified Capabilities

None. No existing spec covers image cleanup, log rotation or disk admission.

## Impact

- **takod:**
  - `pkg/takod/cleanup.go`: the new selector replaces `cleanupOldProjectImages`, plus exited-container reclaim.
  - `pkg/takod/server.go`: the hygiene loop replaces `runBuildCachePruneLoop`; new routes, capability, and flags; 507 responses gain a reclaim hint.
  - `pkg/takod/reconcile.go`: default logging args.
  - New files `pkg/takod/hygiene.go`, `hygiene_images.go`, `hygiene_journal.go`.
- **CLI:**
  - `cmd/cleanup.go`: `--dry-run`, per-service `--keep`, capability handling.
  - `cmd/doctor.go`: hygiene section.
  - `cmd/takod.go`: flags.
  - `pkg/engine/deploy.go` (`:1255-1292`): deploy-time cleanup gating.
  - Takod client: 507 → reclaim → retry once.
- **Setup:** `pkg/provisioner/provisioner.go` (`:1054`) writes the new flags into the takod unit.
- **Docs:**
  - `docs/MACHINE-INTERFACE.md`: `ActionResult.hygiene`, `HygieneReport`.
  - `docs/CONFIGURATION.md`: logging default.
  - `docs/RESOURCE-MONITORING.md`.
  - `tako cleanup` help.
- **Dependencies:**
  - `add-takod-health-monitoring` provides the alert-source interface. Until it lands, hygiene events are recorded in the journal only.
  - Independent of `add-local-self-healing`. The reclaim never removes containers of the current desired revision, so it doesn't fight self-healing.
  - `add-server-status-history` may chart the journal's freed bytes. That's out of scope here.
