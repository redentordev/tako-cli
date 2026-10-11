# Tasks

## 1. Lease scoping (CLI only; fixes #114's blocking error)

- [ ] 1.1 Add a regression test for the #114 config (two environments, one server each, no roles, proxied service). Assert that the preview deploy lease set is `{preview}`, and that the deploy succeeds when the production SSH dial fails. Write it first and confirm it fails on master.
- [ ] 1.2 Rewrite `DeployMutationTargets`/`DeployLeaseTargets` (`pkg/engine/takod.go:59-125`) per D1: environment proxy targets instead of all edges, one selected builder, and role-less servers counted as builder candidates only when they are environment members. Update `pkg/engine/deploy.go:390-421`. Verify with table tests covering explicit proxy placement outside the environment, an explicit `roles: [builder]` server outside the environment, local build strategy, and enrolled mode (unchanged).
- [ ] 1.3 Add an in-scope reason to lease errors for nodes outside the environment (D2). Verify with a test on the error text.
- [ ] 1.4 Add a "mutated implies leased" test harness: record every mutating takod call made through the deployer in deploy, rollback, project rollback, scale and promote, and assert that each target node is in the lease set.
- [ ] 1.5 Audit the target lists in `cmd/backup.go:135`, `cmd/cleanup.go:114`, `cmd/env.go:161`, `cmd/maintenance.go:112`, `cmd/live.go:79` and `cmd/certs.go:155` for the same over-reach, and fix any found. Verify with per-command tests of the target list.

## 2. Node maintenance lease

- [ ] 2.1 takod: add the `node` lease kind and the mutual exclusion with environment leases (`pkg/takod/lease.go`), plus capability `lease.node-maintenance-v1`. Verify with takod unit tests: node lease blocks environment leases, a live environment lease blocks the node lease, expiry releases both, and different projects are covered.
- [ ] 2.2 CLI: add `AcquireNodeMaintenanceLeases` (`pkg/engine/leases.go`) with the D3 fallback and warning. Use it in `cmd/upgrade.go`, setup/provision commands, and the node-global `cmd/state.go` operations. Verify with engine tests using fake agents with and without the capability.

## 3. Mesh peer sets and addressing

- [ ] 3.1 Add a regression test for mesh state with two environments sharing one server. Deploy env A, then env B. Assert that the shared node's peers include both environments' members, and that A-only and B-only nodes never list each other. Confirm it fails on master.
- [ ] 3.2 takod: store per-`(project, env)` peer sets, render WireGuard from their union, run the conflict check, migrate legacy `peers.json` (D4-D6), and add capability `mesh.peer-sets-v1` (`pkg/takod/metadata.go`, `server.go:2671`, `pkg/mesh`). Verify with takod tests: legacy file with tags, legacy file without tags, union deduplication, a duplicate address with different keys rejected and nothing applied, and destroy removing a set.
- [ ] 3.3 Deployer: replace positional `meshAddress(i)` with the D5 resolution (pinned, then recorded, then hash-probe). The recorded address is returned by `ensureTakodMeshKeys`. Verify with tests: an existing positional address is preserved, preview and production compute identical addresses for a shared server, adding a server never changes a recorded address, and a narrower operation (subset targets) does not renumber.
- [ ] 3.4 Config: add optional `servers.<name>.meshIP`, validated to be inside `mesh.networkCIDR` and unique; update the schema. Verify with config and schema tests.
- [ ] 3.5 Fall back to the legacy single-file path when an agent lacks `mesh.peer-sets-v1`, with the shared-server warning. Verify with a deployer test.
- [ ] 3.6 `tako doctor`: read-only mesh conflict and stale-peer-set report. Verify with doctor tests.

## 4. Docs and verification

- [ ] 4.1 Update `docs/ORCHESTRATION-MODEL.md` (lease scope, node lease, mesh peer sets) and `docs/CONFIGURATION.md` (environments sharing servers, `meshIP`). Add a troubleshooting entry for the #114 error. Remove the "separate config per environment" workaround from the docs if present.
- [ ] 4.2 Run `go build ./...` and `go test ./...`. Do a manual check on two test VPSes: deploy preview with production powered off, then deploy both environments while they share a third server.
