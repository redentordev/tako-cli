# Spec Delta

## Purpose

Tako nodes keep their disks healthy by themselves: container logs are bounded by default, unused images are removed between deploys under exactly the v0.14.2 cleanup rules (never another environment's, never a current or in-use image), and low disk triggers a safe reclaim before work is refused. Every automatic deletion is recorded and visible.

## ADDED Requirements

### Requirement: Default container log rotation
When a service has no `logging:` block, takod SHALL start its containers with the `json-file` driver, `max-size=20m` and `max-file=5`. An explicit `logging:` block SHALL always take precedence. The default MUST NOT be part of the desired state document or the container config hash.

#### Scenario: Service without logging block on a stock daemon
- **WHEN** a service without `logging:` is created on a node whose daemon default driver is `json-file` and whose `daemon.json` is absent or sets no logging keys
- **THEN** the container is started with `--log-driver json-file --log-opt max-size=20m --log-opt max-file=5`

#### Scenario: Explicit logging wins
- **WHEN** a service sets `logging: {driver: local, maxSize: 50m, maxFiles: 3}`
- **THEN** the container uses exactly those options

#### Scenario: Upgrade does not restart services
- **WHEN** takod is upgraded to a version with the logging default and the next deploy leaves a service's image and config unchanged
- **THEN** that service's containers are not recreated
- **AND** they keep their previous log options until they are next recreated

### Requirement: Operator logging policy wins
takod SHALL NOT apply the default log options when the Docker daemon's default logging driver is not `json-file`, when `/etc/docker/daemon.json` sets `log-driver` or any `log-opts` key or cannot be read or parsed, or when takod runs with `--default-container-logging=off`. `GET /v1/hygiene` SHALL report why the default is skipped.

#### Scenario: Operator-managed daemon logging
- **WHEN** `/etc/docker/daemon.json` sets `log-opts` (for example only `max-file`), or the daemon default driver is `journald`
- **THEN** takod adds no log options for services without `logging:`
- **AND** `GET /v1/hygiene` reports the default as skipped with the reason

### Requirement: One image selector for every removal path
Every image removal performed by takod (deploy-time cleanup, `tako cleanup`, the scheduled image GC, the reclaim and dry runs) SHALL use the v0.14.2 selector in `pkg/takod/image_cleanup.go`, applied per (project, environment) to that environment's built-image repositories. This change MUST NOT introduce a second selection rule set. Removal SHALL be by image ID without force.

#### Scenario: Docker refuses a removal
- **WHEN** `docker rmi` reports a conflict for a selected image
- **THEN** the image is kept and the run records a skip reason
- **AND** no forced removal is attempted

### Requirement: Image removal never deletes protected images
The selector SHALL keep the newest N images per repository among the images the target environment owns. It SHALL remove an image only when every reference on it is owned by the target environment alone, no current desired or actual document of any project on the node names it, and no container in any state uses it.

#### Scenario: Staging cleanup on a server shared with production
- **WHEN** production and staging of project `shop` run on the same server, production's history references `shop/web:a1` and `shop/web:b2`, and `tako cleanup -e staging --keep 1` runs
- **THEN** `shop/web:a1` and `shop/web:b2` are not removed

#### Scenario: Image used by a stopped non-Tako container
- **WHEN** an image owned only by the target environment is used only by an exited container not created by Tako
- **THEN** it is not removed

#### Scenario: Current image of another project
- **WHEN** an image ref owned by `shop` staging is also named by the current desired document of project `blog`
- **THEN** no cleanup of `shop` staging removes it

### Requirement: Image selection fails closed
If any desired, archived desired revision, history or deployment document on the node cannot be read or parsed, or the container or image inventory cannot be listed, the selector SHALL remove no images in that scope and SHALL record the reason.

#### Scenario: Corrupt history document
- **WHEN** `history/shop/production/history.json` is not valid JSON during scheduled hygiene
- **THEN** no image is removed
- **AND** the hygiene journal records `skipped: unreadable state` with the path

### Requirement: Autonomous image GC scope
takod SHALL record the `ImageRepositories` of every accepted scoped cleanup request per (project, environment) and remove the record with the environment. The autonomous image GC SHALL run only for recorded scopes that still have a desired document, with exactly the recorded repositories and `--image-retention` (default 3, minimum 1). Pulled `image:` repositories MUST NOT be recorded or removed.

#### Scenario: Rollback window per repository
- **WHEN** production history references seven distinct images of `shop/web` that are older than 24 hours and not used by any container, and `shop/web` is recorded for production
- **THEN** the scheduled image GC keeps the newest three and removes the four oldest

#### Scenario: Scope never recorded
- **WHEN** an environment has not been deployed or cleaned since takod was upgraded
- **THEN** the scheduled image GC removes none of its images
- **AND** `tako doctor` warns that the scope has no hygiene record

#### Scenario: Pulled images are never collected
- **WHEN** a service uses `image: postgres:16` and older `postgres` tags are on the node
- **THEN** no hygiene, cleanup or reclaim operation removes them

### Requirement: Autonomous image GC safeguards
The autonomous image GC SHALL keep images created less than 24 hours ago. It SHALL skip a scope, recording the reason, while that scope's operation lease is held, while the node's worker fence is active, or while a restore-on-failure retention record exists for one of its services.

#### Scenario: Recently built image
- **WHEN** an image owned by the scope was built 10 minutes ago and is beyond the newest three
- **THEN** the scheduled image GC does not remove it and reports it kept as `recent`

#### Scenario: Deploy in progress
- **WHEN** a deploy of `shop` production holds its operation lease at the daily maintenance tick
- **THEN** the image GC skips `shop` production and records `skipped: lease held`
- **AND** other idle scopes on the node are still collected

### Requirement: Autonomous hygiene loop
takod SHALL run a hygiene loop that replaces the build-cache prune loop. Every 24 hours by default it SHALL run the image GC followed by the build-cache prune, holding the maintenance barrier and the mutation lock, and SHALL skip the whole run while a worker operation fence is active. `--hygiene-interval=0` SHALL disable autonomous runs.

#### Scenario: Worker fence active
- **WHEN** the node's worker operation fence is active at a scheduled tick
- **THEN** the run is skipped and recorded, and retried at the next tick

### Requirement: Hygiene settings are takod defaults
The existing `--build-cache-prune-interval` and `--build-cache-keep-storage` flags SHALL keep working. New hygiene defaults SHALL live in the takod binary, and setup MUST NOT write the new flags into the takod unit.

#### Scenario: Legacy flag
- **WHEN** takod runs with `--build-cache-prune-interval=12h` and no `--hygiene-interval`
- **THEN** the maintenance tick runs every 12 hours

#### Scenario: Binary-only upgrade
- **WHEN** a node set up by an earlier release is upgraded with `tako upgrade servers`
- **THEN** the hygiene loop runs with the binary's defaults without any unit change

### Requirement: Proactive disk-pressure check
takod SHALL check free space on the takod data directory and the Docker data root every hour by default. Below the soft threshold `max(1.5 × minimumFreeDiskBytes, min(10% of the filesystem, 50 GiB))` it SHALL run a soft reclaim; below the hard threshold `1.5 × minimumFreeDiskBytes` it SHALL run a hard reclaim.

#### Scenario: Soft pressure
- **WHEN** the hourly check finds 14 GiB free with a 10 GiB floor on a 200 GiB disk
- **THEN** a soft reclaim runs, because 14 GiB is below the 20 GiB soft threshold and above the 15 GiB hard threshold

#### Scenario: Large disk mostly volumes
- **WHEN** a 2 TiB disk has 60 GiB free with a 10 GiB floor
- **THEN** no reclaim runs, because the soft threshold is capped at 50 GiB

### Requirement: Proactive reclaim backoff
A proactive reclaim that ends still below its threshold SHALL suppress further proactive reclaims at that level for 6 hours. Admission denials and explicit reclaim requests SHALL always run a hard reclaim and SHALL NOT be suppressed.

#### Scenario: Pressure that cannot be cleared
- **WHEN** a soft reclaim ends still below the soft threshold
- **THEN** no further proactive soft reclaim runs for 6 hours

#### Scenario: Denial during backoff
- **WHEN** an admission denial occurs while proactive reclaims are backed off
- **THEN** a hard reclaim runs immediately

### Requirement: Reclaim is ordered
A reclaim SHALL proceed in order through dangling images, build cache down to the configured budget, and the image GC; a hard reclaim SHALL then continue with build cache down to the pressure budget and eligible exited Tako containers. It SHALL stop once free space is above the soft threshold.

#### Scenario: Reclaim stops early
- **WHEN** pruning dangling images and build cache raises free space above the soft threshold
- **THEN** no images or containers are removed by that reclaim

### Requirement: Reclaim removes only eligible exited containers
A hard reclaim SHALL remove only exited or dead Tako-labelled containers that finished more than one hour ago (one-off exec, job and backup containers after 24 hours). It MUST NOT remove replicas of the current desired revision, containers of a scope whose lease is held or that has a restore-on-failure retention record, or containers listed in a self-healing converged record.

#### Scenario: Crashed current replica is kept
- **WHEN** a replica of the current desired revision has exited
- **THEN** the reclaim does not remove it

#### Scenario: Restore-on-failure containers are kept
- **WHEN** a deploy with restore-on-failure has stopped the previous containers and recorded a retention, and the CLI disconnected
- **THEN** the reclaim does not remove the retained containers

### Requirement: Reclaim never touches data
A reclaim MUST NOT remove volumes, backup archives, project files, running containers, non-Tako containers or images, pulled `image:` repositories, or container log files.

#### Scenario: Data is never touched
- **WHEN** a hard reclaim runs
- **THEN** it issues no volume, backup-archive or container-log-file deletion

### Requirement: Reclaim before refusing CLI work
When a node advertising `node.disk-hygiene-v1` denies a CLI request with HTTP 507 for free-disk admission (body starting with `operation denied`), the CLI SHALL request `POST /v1/hygiene/reclaim`, wait for it (at most 5 minutes), and retry the original request exactly once. If the request body cannot be replayed, the CLI SHALL report the reclaimed space and fail with the original error.

#### Scenario: Deploy succeeds after reclaim
- **WHEN** a deploy's service-files upload is denied with 507 and the reclaim frees enough space
- **THEN** the CLI prints a single line naming the server and the bytes reclaimed, retries, and the deploy continues

#### Scenario: Reclaim skips the caller's environment
- **WHEN** the reclaim runs while the denied deploy still holds its environment's lease
- **THEN** that environment's images and containers are not removed by it

#### Scenario: Reclaim insufficient
- **WHEN** the retried request is denied again
- **THEN** the command fails with the original admission error and the exit code it had before this change

#### Scenario: Old node
- **WHEN** a node without `node.disk-hygiene-v1` returns 507
- **THEN** the CLI does not call the reclaim endpoint and fails as before

### Requirement: Scheduler denials signal a reclaim
Scheduler admission denials (backup, job, certificate) SHALL signal an asynchronous hard reclaim and SHALL NOT change scheduling.

#### Scenario: Scheduled backup denied
- **WHEN** a scheduled backup is denied for low disk
- **THEN** the hygiene loop runs a hard reclaim
- **AND** the backup runs at its next scheduled time, not immediately

### Requirement: Old-image cleanup requires the hygiene capability
The CLI SHALL send old-image removal (`CleanOldImages`) only to nodes advertising `node.disk-hygiene-v1`. `KeepImages` SHALL keep its v0.14.2 meaning. For other nodes, deploy-time cleanup SHALL omit old-image removal with a warning, and `tako cleanup` SHALL report old-image removal as not performed with a hint to run `tako upgrade servers`, while still running its other cleanup steps.

#### Scenario: Deploy to an old node
- **WHEN** a deploy succeeds on a node without the capability
- **THEN** post-deploy cleanup removes stopped containers, dangling images and build cache, but no tagged images
- **AND** the deploy prints one warning naming the server and `tako upgrade servers`

### Requirement: Hygiene runs are recorded
takod SHALL record every hygiene, reclaim and image-cleanup run, including dry runs, in a journal of the last 50 runs, with trigger, timing, free space before and after, removals and bytes freed per category, and skip reasons. `GET /v1/hygiene` SHALL return the policy, the recorded scopes, the latest runs, and containers running without log rotation with their log sizes.

#### Scenario: Scheduled run is journaled
- **WHEN** the daily maintenance tick removes two images
- **THEN** `GET /v1/hygiene` lists a run with trigger `schedule`, the two removed images and the bytes freed

### Requirement: Hygiene alerts
takod SHALL report hygiene alerts as the node-scoped kind `hygiene` of the `add-takod-health-monitoring` alert-source API, delivered to scopes that subscribe to host alerts. Successful reclaims SHALL NOT be delivered as alerts, and hygiene MUST NOT raise disk or inode threshold alerts.

#### Scenario: Successful reclaim
- **WHEN** a soft reclaim frees 8 GiB and ends above the soft threshold
- **THEN** no alert is delivered and the run is in the journal

### Requirement: Insufficient reclaim alert
takod SHALL open a critical `reclaim_insufficient` level of kind `hygiene` on subject `root` or `docker` when free space stays below the hard threshold after a full hard reclaim, and SHALL recover it once free space is above the soft threshold.

#### Scenario: Reclaim cannot free enough
- **WHEN** a hard reclaim ends with the Docker data root still below the hard threshold
- **THEN** one `disk_reclaim_insufficient` opening is delivered per webhook target for subject `docker`

### Requirement: Blocked image GC alert
takod SHALL open a warning `state_unreadable` level of kind `hygiene` on subject `image-gc` when the image GC was skipped because node state could not be read, and SHALL recover it after the next successful selection.

#### Scenario: Corrupt state blocks image GC
- **WHEN** the daily image GC is skipped because `history/shop/production/history.json` is not valid JSON
- **THEN** one `image_gc_blocked` opening is delivered per webhook target
- **AND** an `image_gc_resumed` recovery follows once the file is readable and a selection succeeds

### Requirement: Hygiene is visible in the CLI
`tako cleanup --dry-run` SHALL show what would be removed and why the rest is kept without removing anything. `tako doctor` SHALL include a `disk-hygiene` check per server. Machine output SHALL carry the same data additively.

#### Scenario: Dry run
- **WHEN** `tako cleanup --dry-run` runs
- **THEN** no image or container is removed
- **AND** the output lists each image that would be removed with its size and summarizes kept images by reason (`in use`, `current`, `other environment`, `retention`, `recent`)

#### Scenario: Doctor warns about unrotated logs
- **WHEN** a container created before the logging default has a 3 GiB log file
- **THEN** `tako doctor` reports `disk-hygiene` as `warn` and names the container and suggests redeploying its service

#### Scenario: Machine output
- **WHEN** `tako cleanup --output json` runs
- **THEN** each server outcome includes an additive `hygiene` object with removed items, kept counts by reason, bytes freed and `dryRun`
