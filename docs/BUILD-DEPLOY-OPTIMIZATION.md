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
- Automatic post-deploy cleanup uses `docker image prune -f` for dangling images
  with a negative `com.docker.compose.project` label filter, and `docker builder
  prune -f --keep-storage 20GB` for BuildKit cache pressure. Explicit project
  image cleanup also inventories and excludes Compose-labeled images before
  removing anything. Project image deletion requires the engine's exact
  repository allowlist; takod does not infer ownership from repository names.
  The builder cache threshold avoids clearing the whole shared cache after every
  deploy while still preventing unbounded cache growth.
- The installed takod service also schedules Docker builder cache pruning every
  24 hours with the same `20GB` keep-storage budget. `takod run
  --build-cache-prune-interval 0` disables that loop for custom installs.
- Build-backed services can opt into `deployment.build.strategy: local` to build
  on the developer or CI machine with `docker buildx build --platform
  linux/$arch --load` and push directly to each assigned server with
  psviderski/unregistry's `docker pussh` plugin. `auto` tries the same local
  path and falls back to the remote takod builder if local Docker, buildx,
  docker-pussh, SSH auth, or remote Docker prerequisites are not available.
- Docker can still list a dangling image when a running container references an
  untagged image. Tako leaves those alone because Docker does not consider them
  reclaimable.

## Not Yet Tested

- Two-node local build distribution with the same architecture where one local
  build is pushed to multiple assigned nodes.
- Mixed AMD64/ARM64 local builds from the same client. The code builds once per
  target architecture, but this still needs live timing and compatibility data.
- Registry-backed multi-architecture manifests.
- Large build contexts where context compression/upload time dominates.
- Local Docker build handoff from Docker Desktop, Colima, or rootless Docker.
- CI runner timing against a remote node.
- Cache behavior after a remote node is destroyed and replaced.
- Containerd image-store cleanup behavior on remote hosts that use Docker's
  classic image store. Upstream unregistry can leave an extra containerd copy
  when Docker must pull back from the temporary registry.

## Next Optimization Work

1. Collect live timings for `deployment.build.strategy: local` and `auto`
   against one-node, same-arch multi-node, and mixed-arch environments.
2. Add build context diagnostics that report total archive size and largest
   included files before remote upload.
3. Expose cache policy knobs for remote builder cache and local buildx cache.
4. Add an optional registry-backed path for GHCR/ECR/private registries when a
   team wants durable image distribution instead of direct SSH transfer.
