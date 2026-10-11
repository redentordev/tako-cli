# Spec Delta

## Purpose

Tako nodes keep their disks healthy by themselves: container logs are bounded by default, unused images are removed without ever touching a rollback target, and low disk triggers a safe reclaim before work is refused. Every automatic deletion is recorded and visible.

## ADDED Requirements

### Requirement: Default container log rotation
When a service has no `logging:` block, takod SHALL start its containers with the `json-file` driver, `max-size=20m` and `max-file=5`, unless the Docker daemon's default logging driver is not `json-file`, `/etc/docker/daemon.json` sets `log-opts.max-size`, `daemon.json` is unreadable or invalid, or takod runs with `--default-container-logging=off`. An explicit `logging:` block SHALL always take precedence. The default MUST NOT be part of the desired state document or the container config hash.

#### Scenario: Service without logging block on a stock daemon
- **WHEN** a service without `logging:` is created on a node whose daemon default driver is `json-file` with no `max-size`
- **THEN** the container is started with `--log-driver json-file --log-opt max-size=20m --log-opt max-file=5`

#### Scenario: Operator-managed daemon logging
- **WHEN** `/etc/docker/daemon.json` sets `log-opts.max-size`, or the daemon default driver is `journald`
- **THEN** takod adds no log options for services without `logging:`
- **AND** `GET /v1/hygiene` reports the default as skipped with the reason

#### Scenario: Explicit logging wins
- **WHEN** a service sets `logging: {driver: local, maxSize: 50m, maxFiles: 3}`
- **THEN** the container uses exactly those options

#### Scenario: Upgrade does not restart services
- **WHEN** takod is upgraded to a version with the logging default and the next deploy leaves a service's image and config unchanged
- **THEN** that service's containers are not recreated
- **AND** they keep their previous log options until they are next recreated

### Requirement: Only Tako-referenced images are removal candidates
Every image removal performed by takod (deploy-time cleanup, `tako cleanup`, scheduled hygiene and reclaim) SHALL use one selector. The selector SHALL consider only images whose repository is referenced by a desired, retained-revision or history document on the node, and that carry no `com.docker.compose.project` label. Removal SHALL be by image ID without force.

#### Scenario: Non-Tako image
- **WHEN** an image's repository is not referenced by any Tako state document on the node
- **THEN** no hygiene, cleanup or reclaim operation removes it

#### Scenario: Docker refuses a removal
- **WHEN** `docker rmi` reports a conflict for a selected image
- **THEN** the image is kept and the run records a skip reason
- **AND** no forced removal is attempted

### Requirement: Image removal never deletes protected images
The selector MUST NOT remove an image that is used by any container on the node in any state, referenced by any current or retained desired revision of any project or environment on the node, among the newest `K` distinct images per project, environment and service in that environment's deployment history (default `K = 5`, minimum 1), or created less than 24 hours ago.

#### Scenario: Staging cleanup on a server shared with production
- **WHEN** production and staging of project `shop` run on the same server, production's history references `shop/web:a1` and `shop/web:b2`, and `tako cleanup -e staging --keep 1` runs
- **THEN** `shop/web:a1` and `shop/web:b2` are not removed

#### Scenario: Rollback window per service
- **WHEN** production history references seven distinct images of `shop/web` that are older than 24 hours and not used by any container
- **THEN** scheduled hygiene removes the two oldest and keeps the newest five

#### Scenario: Image used by a stopped non-Tako container
- **WHEN** an image in a Tako-referenced repository is used only by an exited container not created by Tako
- **THEN** it is not removed

#### Scenario: Recently built image not yet in desired state
- **WHEN** an image was built 10 minutes ago and no desired document references it yet
- **THEN** it is not removed

### Requirement: Image selection fails closed
If any desired, retained-revision or history document on the node cannot be read or parsed, or the container or image inventory cannot be listed, the selector SHALL remove no images in that run and SHALL record the reason.

#### Scenario: Corrupt history document
- **WHEN** `history/shop/production/history.json` is not valid JSON during scheduled hygiene
- **THEN** no image is removed
- **AND** the hygiene journal records `skipped: unreadable state` with the path

### Requirement: Autonomous hygiene loop
takod SHALL run a hygiene loop that replaces the build-cache prune loop. Every 24 hours by default it SHALL run the image GC followed by the build-cache prune. Runs SHALL hold the maintenance barrier and mutation lock and SHALL be skipped while a worker operation is active. `--hygiene-interval=0` SHALL disable the autonomous runs. The existing `--build-cache-prune-interval` and `--build-cache-keep-storage` flags SHALL keep working.

#### Scenario: Busy node
- **WHEN** a deploy holds the mutation lock at a scheduled tick
- **THEN** the run is skipped and recorded, and retried at the next tick

#### Scenario: Legacy flag
- **WHEN** takod runs with `--build-cache-prune-interval=12h` and no `--hygiene-interval`
- **THEN** the maintenance tick runs every 12 hours

### Requirement: Proactive disk-pressure check
takod SHALL check free space every hour by default and run a reclaim when free space on the takod data directory or the Docker data root is below `max(1.5 × minimumFreeDiskBytes, 10% of the filesystem)`.

#### Scenario: Proactive reclaim
- **WHEN** the hourly check finds 12 GiB free with a 10 GiB floor on a 200 GiB disk
- **THEN** a reclaim runs, because 12 GiB is below both 15 GiB and 20 GiB

### Requirement: Reclaim is ordered
A reclaim SHALL proceed in order through dangling images, build cache down to the configured budget, build cache down to the pressure budget, retention-eligible images, and eligible exited Tako containers. It SHALL stop once free space reaches the proactive threshold.

#### Scenario: Reclaim stops early
- **WHEN** pruning dangling images and build cache raises free space above the threshold
- **THEN** no images or containers are removed by that reclaim

### Requirement: Reclaim is limited to safe resources
A reclaim SHALL remove only exited Tako-labelled containers that finished more than one hour ago and are not replicas of their environment's current desired revision, and one-off exec, job and backup containers after 24 hours. A reclaim MUST NOT remove volumes, backup archives, project files, running containers, non-Tako containers or images, or container log files.

#### Scenario: Crashed current replica is kept
- **WHEN** a replica of the current desired revision has exited
- **THEN** the reclaim does not remove it

#### Scenario: Data is never touched
- **WHEN** a reclaim runs under critical pressure
- **THEN** it issues no volume, backup-archive or container-log-file deletion

### Requirement: Reclaim before refusing CLI work
When a takod request from the CLI is denied with HTTP 507 for free-disk admission by a node advertising `node.disk-hygiene-v1`, the CLI SHALL request `POST /v1/hygiene/reclaim`, wait for it (at most 5 minutes), and retry the original request exactly once. If the request body cannot be replayed, the CLI SHALL report the reclaimed space and fail with the original error. Scheduler admission denials SHALL signal an asynchronous reclaim and SHALL NOT change scheduling.

#### Scenario: Deploy succeeds after reclaim
- **WHEN** a deploy's service-files upload is denied with 507 and the reclaim frees enough space
- **THEN** the CLI prints a single line naming the server and the bytes reclaimed, retries, and the deploy continues

#### Scenario: Reclaim insufficient
- **WHEN** the retried request is denied again
- **THEN** the command fails with the original admission error and the exit code it had before this change

#### Scenario: Old node
- **WHEN** a node without `node.disk-hygiene-v1` returns 507
- **THEN** the CLI does not call the reclaim endpoint and fails as before

### Requirement: Old-image cleanup requires the hygiene capability
The CLI SHALL send old-image removal (`CleanOldImages`) only to nodes advertising `node.disk-hygiene-v1`, where `KeepImages` means the per project, environment and service rollback window. For other nodes, deploy-time cleanup SHALL omit old-image removal, and `tako cleanup` SHALL report old-image removal as not performed with a hint to run `tako upgrade servers`, while still running its other cleanup steps.

#### Scenario: Deploy to an old node
- **WHEN** a deploy succeeds on a node without the capability
- **THEN** post-deploy cleanup removes stopped containers, dangling images and build cache, but no tagged images

### Requirement: Hygiene runs are recorded
takod SHALL record every hygiene, reclaim and image-cleanup run, including dry runs, in a journal of the last 50 runs, with trigger, timing, free space before and after, removals and bytes freed per category, and skip reasons. `GET /v1/hygiene` SHALL return the policy, the latest runs, and containers running without log rotation with their log sizes.

#### Scenario: Scheduled run is journaled
- **WHEN** the daily maintenance tick removes two images
- **THEN** `GET /v1/hygiene` lists a run with trigger `schedule`, the two removed images and the bytes freed

### Requirement: Hygiene is visible in the CLI
`tako cleanup --dry-run` SHALL show what would be removed and why the rest is kept without removing anything. `tako doctor` SHALL include a `disk-hygiene` check per server. Machine output SHALL carry the same data additively.

#### Scenario: Dry run
- **WHEN** `tako cleanup --dry-run` runs
- **THEN** no image or container is removed
- **AND** the output lists each image that would be removed with its size and summarizes kept images by reason (`in use`, `desired`, `rollback window`, `recent`)

#### Scenario: Doctor warns about unrotated logs
- **WHEN** a container created before the logging default has a 3 GiB log file
- **THEN** `tako doctor` reports `disk-hygiene` as `warn` and names the container and suggests redeploying its service

#### Scenario: Machine output
- **WHEN** `tako cleanup --output json` runs
- **THEN** each server outcome includes an additive `hygiene` object with removed items, kept counts by reason, bytes freed and `dryRun`
