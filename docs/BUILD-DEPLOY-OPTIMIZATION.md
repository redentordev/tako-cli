# Build And Deploy Optimization Notes

This document separates the build/deploy behavior currently proved on a live
node from the optimization work that is still planned.

## Tested Baseline

Environment:

- Date: 2026-06-21.
- Target: one x86_64 playground node running `takod`.
- App: one build-backed web service deployed through the takod runtime path.
- Command shape: `tako deploy -e production --force --yes --verbose`.

Results:

| Case | Result | Notes |
| --- | ---: | --- |
| Pre-change same-app forced deploy | 37.23s | Build path was invoked even though Docker reused cache for the commit image. The old CLI did not expose archive/upload/build stage timings. |
| Post-change same-app forced deploy | 33.20s | The node-local image-exists check took 807ms, skipped archive creation, skipped context upload, and skipped remote Docker build. |
| Temporary first build timing sample | 42.72s | A small test app produced an 11.9 KiB build-context archive in 4ms, spent 7.3s in the remote build request, and `takod` reported 6ms extract plus 6.4s Docker build. |

The same-app post-change result is the clean comparison point. The temporary
first-build sample is only stage-timing evidence because it used a different
app and included first-time create work.

## Implemented

- `takod` returns image-build timings in `/v1/images/build` responses:
  extract time, Docker build time, and total server-side build time.
- Verbose deploy output prints build-context archive size, local archive
  creation time, remote build request duration, and server-side build timings.
- Build-backed takod deploys check `/v1/images/exists` before building on a
  node. If the exact commit image exists, deploy skips archive creation,
  context upload, and Docker build.
- Automatic post-deploy dangling image cleanup uses `docker image prune -f`
  instead of passing BuildKit-generated dangling image IDs directly to
  `docker rmi`.
- Docker can still list a dangling image when a running container references an
  untagged image. Tako leaves those alone because Docker does not consider them
  reclaimable.

## Not Yet Tested

- Two-node build distribution where one node builds once and another node
  receives the image.
- Real unregistry layer-delta transfer. `pkg/unregistry` exists, but the live
  deploy path does not call it yet.
- Multi-architecture builds and manifests.
- Peer image transfer over WireGuard-only paths.
- Peer image transfer through an SSH tunnel when mesh reachability is degraded.
- Large build contexts where context compression/upload time dominates.
- Local Docker build handoff from Docker Desktop, Colima, or rootless Docker.
- CI runner timing against a remote node.
- Cache behavior after a remote node is destroyed and replaced.

## Next Optimization Work

1. Build once per architecture and make peer image availability a separate
   reconciliation step before container rollout.
2. Replace full `docker save`/`docker load` transfer with a short-lived
   unregistry pull path so peers fetch only missing layers.
3. Add build context diagnostics that report total archive size and largest
   included files before upload.
4. Keep BuildKit cache hot on builder nodes and expose explicit config knobs
   later, such as build strategy, cache policy, and distribution policy.
5. Add a local-build fast path only when local Docker exists and the target
   platform matches; otherwise keep the current remote-builder path.
