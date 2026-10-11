# Tasks

## 1. Image selector (takod, pure core first)

- [ ] 1.1 Add `pkg/takod/hygiene_state.go`. It walks `desired/*/*/revision.json`, `desired/*/*/revisions/*.json`, `history/*/*/history.json` and `history/*/*/deployments/*.json`, and returns the referenced repositories `R`, the desired image refs, and the history refs per (project, env, service) ordered by deployment time. Any read or parse error is returned, not skipped. Verify with table tests on a temp dataDir, including previews, retained revisions, and a corrupt file that returns an error naming the path.
- [ ] 1.2 Add `selectImageRemovals(inventory, state, scope, policy, now)`. It is a pure function over an inventory of images (ID, refs, created time, labels) and containers (image ID), and implements D2 C/P/K/grace. It returns removals and kept images with reasons. Verify with table tests:
  - a staging scope with production history on the same node removes nothing of production's;
  - `K` per service;
  - an image used by an exited non-Tako container is kept;
  - an unreferenced repository is never a candidate;
  - a compose label is excluded;
  - an image younger than 24h is kept;
  - a dangling image with no refs is handled;
  - `KeepImages=0` is clamped to 1.
- [ ] 1.3 Add the Docker inventory adapter: `docker images --no-trunc` plus `docker image inspect` for created time and labels, and `docker ps -a --format '{{.ImageID}}'`. Removal is `docker rmi <id>` without `-f`, and a conflict is recorded as a skip. Verify with the existing fake-`runDocker` test harness, including a conflict response.
- [ ] 1.4 Replace `cleanupOldProjectImages` in `CleanupProject` with the selector, using the request scope. Add `DryRun` to `CleanupRequest`, and `Hygiene{Removed, Kept, BytesFreed, DryRun}` to `CleanupResponse`. Delete the old function and update `cleanup_test.go` to the new semantics.

## 2. Default log rotation

- [ ] 2.1 Add the logging-policy guard: read `docker info --format '{{.LoggingDriver}}'` and parse `/etc/docker/daemon.json` (the path is injectable for tests). Cache the result and refresh it hourly. Verify: stock daemon gives active; `max-size` set gives skipped; `journald` gives skipped; invalid JSON gives skipped (operator-managed); `--default-container-logging=off` gives disabled.
- [ ] 2.2 In `reconcile.go`, when `req.Logging == nil` and the policy is active, append the default `json-file` args. Leave the desired document and the `tako.configHash` label untouched. Verify with a reconcile test (the args are present, and the config hash is unchanged from a payload without the default) and with explicit `logging:` taking precedence.
- [ ] 2.3 Add takod flags `--default-container-logging`, `--default-container-log-max-size` and `--default-container-log-max-file`, validated with the same bounds as `validateServiceLogging`.

## 3. Hygiene loop, reclaim and journal

- [ ] 3.1 Add `pkg/takod/hygiene_journal.go`: a ring of 50 `HygieneRun` records with atomic writes and a 0600 file mode. Verify ring truncation and recovery from a corrupt journal (start a new one and keep a `.corrupt` copy).
- [ ] 3.2 Add `runHygieneLoop` (D4). It replaces `runBuildCachePruneLoop`, keeps the legacy flags, and adds `--hygiene-interval`, `--hygiene-check-interval`, `--image-retention` and `--build-cache-pressure-keep-storage`. It has the same barrier, fence and lock sequence as today. Verify with fake clock tests: maintenance tick order (image GC, then build cache); a busy lock skips and records; legacy-flag mapping; `0` disables.
- [ ] 3.3 Add the reclaim (D5) with ordered steps that stop at the threshold, plus exited-container selection (labelled `tako.project`, finished more than 1h ago, not in the current desired revision; one-offs after 24h). Verify:
  - early stop;
  - a crashed current-revision replica is kept;
  - a non-Tako exited container is kept;
  - no volume, backup or log-file operation is ever issued, by asserting on the fake docker and filesystem call log.
- [ ] 3.4 Add the reclaim signal channel and wire scheduler admission denials (`server.go:293-295`) to send to it without blocking. Verify that a burst of denials coalesces into one reclaim.
- [ ] 3.5 Add the routes `GET /v1/hygiene` and `POST /v1/hygiene/reclaim` (single-flight, 5-minute bound, `dryRun`), and the capability `node.disk-hygiene-v1`. Add both routes to the authorization and route tables. Verify the handler tests, single-flight behaviour, and the capability list test.
- [ ] 3.6 Add the unrotated-container report: `docker inspect` `HostConfig.LogConfig` plus the `LogPath` file size, capped at 50 entries. Verify it with the fake inspector.

## 4. CLI

- [ ] 4.1 `nodeclient`: on a 507 whose body starts with `operation denied`, from a node that advertises the capability, call reclaim and then retry once, only for replayable requests. Emit a `hygiene.reclaimed` event and a one-line human message. Verify:
  - retry success;
  - second denial returns the original error and exit class;
  - the old node gets no reclaim call;
  - a non-replayable body gives a no-retry message;
  - the PostgreSQL memory 507 is not retried.
- [ ] 4.2 Deploy-time cleanup (`pkg/engine/deploy.go:1255-1292`): send `CleanOldImages` only when the node advertises the capability, and keep the preview `cleanOldImagesOn` rule. Verify with an engine test against an old-capability fake node.
- [ ] 4.3 `tako cleanup`:
  - `--dry-run`;
  - `--keep` means the per-service window, and the help and output text are corrected;
  - old nodes get a per-server warning with an upgrade hint;
  - `ActionResult` per-server output gains the additive `hygiene` object.

  Verify with the cmd tests, including the JSON golden.
- [ ] 4.4 `tako doctor`: add the `disk-hygiene` check (pass, warn and fail rules from D7), reading `GET /v1/hygiene`. A node without the capability gets `warn`, "upgrade for disk hygiene". Verify with the doctor tests.
- [ ] 4.5 Rollback image preflight: when a local-only image is missing, the error mentions node retention and `--image-retention`. Verify with the existing rollback preflight test, extended.
- [ ] 4.6 Alert integration: if `add-takod-health-monitoring`'s alert-source interface exists, register `disk.reclaimed`, `disk.reclaim_insufficient` and `hygiene.skipped_unreadable_state`. Otherwise emit takod state events only, and leave a TODO referencing that change.

## 5. Setup, docs, verification

- [ ] 5.1 `pkg/provisioner/provisioner.go`: write the new takod flags with their defaults into the unit, and keep the existing build-cache flags. Verify with the unit-template golden test.
- [ ] 5.2 Docs:
  - `docs/MACHINE-INTERFACE.md`: `ActionResult.hygiene`, the `disk-hygiene` doctor check and the `hygiene.reclaimed` event.
  - `docs/CONFIGURATION.md`: the logging default and how to opt out.
  - `docs/RESOURCE-MONITORING.md`: a disk hygiene section.
  - `tako cleanup` help and man page; regenerate the man pages.
- [ ] 5.3 Run `go build ./...`, `go vet ./...` and the full `go test ./...` on Linux, compared with the master baseline, and check that `gofmt` is clean. Run `openspec validate add-disk-hygiene-defaults --strict`.
- [ ] 5.4 Manual check on a test server:
  - a staging deploy next to production removes none of production's history images;
  - `tako cleanup --dry-run` output;
  - a forced low-disk state (fallocate) triggers a reclaim and a successful retry;
  - a new container gets the default log options while existing ones are untouched.
