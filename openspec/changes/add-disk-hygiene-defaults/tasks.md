# Tasks

Prerequisite: v0.14.2's `pkg/takod/image_cleanup.go` and `CleanupProjectWithDataDir` are on master, and `add-takod-health-monitoring` has landed (alert-source API). Rebase on both before starting.

## 1. Image GC on the v0.14.2 selector (takod, pure core first)

- [x] 1.1 Extend `planOldImageRemovals` with an optional `createdAfter` cut-off (kept as `recent`) and kept-image reasons (`in use`, `current`, `other environment`, `retention`, `recent`). The default call (zero cut-off) must give exactly v0.14.2's removal set. Verify: every existing `image_cleanup_test.go` case passes unchanged; new table cases for the grace and for each kept reason.
- [x] 1.2 Add `pkg/takod/hygiene_scopes.go`: record `ImageRepositories` per (project, environment) under `<dataDir>/hygiene/scopes/<project>/<env>.json` (atomic, 0600, `recordedAt`) from every accepted scoped cleanup request; list recorded scopes that still have a desired document; delete the record in environment and preview cleanup. Verify: record on deploy-time cleanup and `tako cleanup`; no record for an empty repository list; deletion on environment cleanup; a scope without desired state is not listed.
- [x] 1.3 Add the autonomous image GC: for each recorded scope, skip (and record why) when its lease is held, the worker fence is active, or a restore-on-failure retention record exists; otherwise call `cleanupOldProjectImages` with the recorded repositories, `--image-retention` and the 24h cut-off. Verify:
  - a staging scope with production history on the same node removes nothing of production's;
  - seven owned `shop/web` images older than 24h → keep 3, remove the 4 oldest;
  - an image younger than 24h is kept;
  - a held lease and a retention record each skip only their scope;
  - an unreadable `history.json` removes nothing in that scope and records the path;
  - a pulled `image:` repository is never passed to the selector.
- [x] 1.4 Add `DryRun` to `CleanupRequest` (no removals; report what would be removed) and `Hygiene{Removed, Kept, BytesFreed, DryRun}` to `CleanupResponse`. `KeepImages` semantics are unchanged. Verify with `cleanup_test.go` additions and a request without the new fields behaving exactly as v0.14.2.

## 2. Default log rotation

- [x] 2.1 Add the logging-policy guard: read `docker info --format '{{.LoggingDriver}}'` and parse `/etc/docker/daemon.json` (the path is injectable for tests). Cache the result and refresh it hourly. Verify: stock daemon gives active; `log-opts` with only `max-file` gives skipped; `log-driver` set gives skipped; `journald` gives skipped; invalid JSON gives skipped (operator-managed); `--default-container-logging=off` gives disabled.
- [x] 2.2 In `reconcile.go`, when `req.Logging == nil` and the policy is active, append the default `json-file` args. Leave the desired document and the `tako.configHash` label untouched. Verify with a reconcile test (the args are present, and the config hash is unchanged from a payload without the default) and with explicit `logging:` taking precedence.
- [x] 2.3 Add takod flags `--default-container-logging`, `--default-container-log-max-size` and `--default-container-log-max-file`, validated with the same bounds as `validateServiceLogging`.

## 3. Hygiene loop, reclaim and journal

- [x] 3.1 Add `pkg/takod/hygiene_journal.go`: a ring of 50 `HygieneRun` records with atomic writes and a 0600 file mode. A write failure keeps the run in memory and never aborts a reclaim. Verify ring truncation and recovery from a corrupt journal (start a new one and keep a `.corrupt` copy).
- [x] 3.2 Add `runHygieneLoop` (D4). It replaces `runBuildCachePruneLoop`, keeps the legacy flags, and adds `--hygiene-interval`, `--hygiene-check-interval`, `--image-retention` and `--build-cache-pressure-keep-storage`. It has the same barrier, fence and lock sequence as today. Verify with fake clock tests: maintenance tick order (image GC, then build cache); an active worker fence skips and records; legacy-flag mapping; `0` disables.
- [x] 3.3 Add the soft and hard reclaim levels (D5): thresholds including the 50 GiB cap, ordered steps that stop above the soft threshold, the 6h proactive backoff per level (never for denials or requests), and exited-container selection (labelled `tako.project`, finished more than 1h ago; one-offs after 24h; never current-revision replicas, held-lease or retention scopes, or self-healing converged records). Verify:
  - soft and hard threshold arithmetic, including a 2 TiB disk with 60 GiB free (no run);
  - early stop;
  - backoff after an insufficient proactive run, and no backoff for a denial;
  - a crashed current-revision replica, a retained restore-on-failure container and a converged-record container are each kept;
  - a non-Tako exited container is kept;
  - no volume, backup or log-file operation is ever issued, by asserting on the fake docker and filesystem call log.
- [x] 3.4 Add the reclaim signal channel and wire scheduler admission denials (`server.go:293-295`) to send to it without blocking. Verify that a burst of denials coalesces into one reclaim.
- [x] 3.5 Add the routes `GET /v1/hygiene` and `POST /v1/hygiene/reclaim` (single-flight, 5-minute bound, `dryRun`), and the capability `node.disk-hygiene-v1` (`CapabilityNodeDiskHygieneV1`). Add both routes to the authorization, lifecycle-gate and route tables. Verify the handler tests, single-flight behaviour, and the capability list test.
- [x] 3.6 Add the unrotated-container report: `docker inspect` `HostConfig.LogConfig` plus the `LogPath` file size, capped at 50 entries. Verify it with the fake inspector.
- [x] 3.7 Register alert kind `hygiene` with the alert-source API (node-scoped; reasons `reclaim_insufficient` on `root`/`docker` and `state_unreadable` on `image-gc`, per monitoring design D13) and report levels from the reclaim and image GC. Verify: an insufficient hard reclaim opens once per target; recovery above the soft threshold; a successful reclaim delivers nothing; no `high_disk` is ever reported by hygiene.

## 4. CLI

- [x] 4.1 `nodeclient`: on a 507 whose body starts with `operation denied`, from a node that advertises the capability, call reclaim and then retry once, only for replayable requests. Emit a `hygiene.reclaimed` event and a one-line human message. Verify:
  - retry success;
  - second denial returns the original error and exit class;
  - the old node gets no reclaim call;
  - a non-replayable body gives a no-retry message;
  - the PostgreSQL memory 507 is not retried.
- [x] 4.2 Deploy-time cleanup (`pkg/engine/deploy.go:1255-1292`): send `CleanOldImages` only when the node advertises the capability, warn once per deploy for other nodes, and keep the preview `cleanOldImagesOn` rule. Verify with an engine test against an old-capability fake node.
- [x] 4.3 `tako cleanup`:
  - `--dry-run` (capable nodes only; others report "dry run not supported, upgrade");
  - `--keep` keeps its meaning (per repository, target environment only); the help text says so;
  - old nodes get a per-server "old-image removal skipped" warning with an upgrade hint, and the other steps still run;
  - `ActionResult` per-server output gains the additive `hygiene` object.

  Verify with the cmd tests, including the JSON golden.
- [x] 4.4 `tako doctor`: add the `disk-hygiene` check (pass, warn and fail rules from D7, including "no recorded scope"), reading `GET /v1/hygiene`. A node without the capability gets `warn`, "upgrade for disk hygiene". Verify with the doctor tests.
- [x] 4.5 Rollback image preflight: when a local-only image is missing, the error mentions node retention and `--image-retention`. Verify with the existing rollback preflight test, extended.

## 5. Docs and verification

- [x] 5.1 Setup writes no new flags: confirm that the provisioner unit template is unchanged, and document the systemd drop-in for overrides (`--hygiene-interval`, `--image-retention`, `--default-container-logging=off`).
- [x] 5.2 Docs:
  - `docs/MACHINE-INTERFACE.md`: `ActionResult.hygiene`, the `disk-hygiene` doctor check and the `hygiene.reclaimed` event.
  - `docs/CONFIGURATION.md`: the logging default and how to opt out.
  - `docs/RESOURCE-MONITORING.md`: a disk hygiene section (journal, reclaim, `hygiene` alerts).
  - `tako cleanup` help and man page; regenerate the man pages.
- [ ] 5.3 Run `go build ./...`, `go vet ./...` and the full `go test ./...` on Linux, compared with the master baseline, and check that `gofmt` is clean. Run `openspec validate add-disk-hygiene-defaults --strict`.
- [ ] 5.4 Manual check on a test server (not run in this change; see design Implementation notes):
  - a staging deploy next to production removes none of production's history images;
  - `tako cleanup --dry-run` output;
  - a forced low-disk state (fallocate) triggers a reclaim and a successful retry;
  - a new container gets the default log options while existing ones are untouched.
