# Tasks

## 1. PR1: Lease, builder and image-source scoping (CLI only; fixes #114's error; lands after #120)

- [x] 1.1 Add a regression test for the #114 config: two environments, one server each, no roles, a proxied service and a `build:` service under the default `auto` strategy. Assert:
  - the preview deploy lease set is `{preview}`;
  - no takod call targets `production`;
  - the deploy succeeds when the `production` dial fails.

  Write the test first and confirm it fails on master.
- [x] 1.2 Add `config.EnvironmentBuilderCandidates(cfg, env)` (D1). Use it, plus `ResolveEnvironmentProxyTargets` for edges, in `DeployLeaseTargets`/`DeployMutationTargets` (`pkg/engine/takod.go:59-125`) and `placementApplyTargets` (`pkg/engine/placement_apply.go:214-239`). Enrolled configs keep inventory roles. Verify with table tests covering:
  - non-enrolled, proxied;
  - non-enrolled with a remote build;
  - local build strategy;
  - enrolled (unchanged);
  - placement-apply with a proxied service.
- [x] 1.3 Use the same helper in `Deployer.remoteBuilderServers` (`pkg/deployer/takod_deploy.go:720-732`), which also scopes `locateRetainedImage` (`:939-960`). Verify with deployer tests:
  - a build for `staging` never selects `app-prod` as its source;
  - retained-image lookup contacts no server outside the environment.
- [x] 1.4 Expose the servers pruned by environment scoping as `Config.ScopedOutServers()` (#120 code). Make `AddPriorDesiredMutationTargets` fail with exit 2 when a prior owner is scoped out (D3). Verify:
  - with a config test that the pruned names and variables are reported;
  - with an engine test that the deploy fails before any lease, naming the server and the variable;
  - that a prior owner absent from `servers:` still follows today's behavior.
- [x] 1.5 Add the in-scope reason to lease and connectivity errors for prior owners outside the environment (D2). Verify with a test on the error text.
- [x] 1.6 Add a "mutated implies leased" harness. It records every mutating takod request made through the runtime factory during deploy, run, rollback, project rollback, scale and promote, and asserts that each target node was leased. Verify that it fails if `remoteBuilderServers` is reverted to project-wide.
- [x] 1.7 Audit the target lists in `cmd/backup.go`, `cmd/cleanup.go`, `cmd/env.go`, `cmd/maintenance.go`, `cmd/live.go` and `cmd/certs.go` for servers outside the environment, and fix any found. Verify with per-command target-list tests.
- [x] 1.8 Docs: add a troubleshooting entry for `failed to acquire remote deploy leases: failed to connect to lease node` to `docs/CONFIGURATION.md`. Update the lease scope in `docs/ORCHESTRATION-MODEL.md`, and remove any "separate config per environment" workaround. Verify with `go test ./...`, including the docs snippet tests.

## 2. PR2: Stable mesh addresses and environment-wide peer lists (CLI only)

- [ ] 2.1 Write the regression tests first and confirm they fail on master:
  - `tako run` on the second server of a three-server environment keeps that node's address and peers, because no mesh request is sent;
  - scale with `servers: [b, a]` resolves the same addresses as deploy;
  - a deploy whose workloads touch only `c` still sends the full peer list `[a, b, c]` with unchanged addresses.
- [ ] 2.2 Implement `resolveMeshAddresses` (D5) as a pure function over the CIDR, the sorted members and the `GET /v1/status` records. Verify with table tests:
  - a fresh environment yields `.1, .2, .3`;
  - recorded addresses are kept;
  - a duplicate recorded address is kept by the first name and reassigned for the other, with a warning;
  - a recorded address outside a changed CIDR is reassigned;
  - addresses used by another project's peers on a shared member are avoided;
  - results are identical whether or not unrelated servers were pruned.
- [ ] 2.3 Wire the resolver into the deployer:
  - `SetupTakodRuntime` sends the full schedulable membership as peers and applies to every member for deploy, rollback, project rollback, scale and promote (D4);
  - delete `meshAddress(i)`;
  - route `meshHostIPForServer` (`takod_proxy.go:1140-1166`) through the operation's resolved map.

  Verify with deployer tests on the request payloads.
- [ ] 2.4 Make `tako run` verify-only (`pkg/engine/run.go:182-186`). Verify with an engine test that no `PUT /v1/metadata` or `POST /v1/mesh/apply` is sent.
- [ ] 2.5 Make `tako domains hosts --address mesh|auto` read recorded addresses from the proxy nodes and fail for unreachable ones (`cmd/domain_hosts.go:125-200`). Verify with command tests using fake status responses.
- [ ] 2.6 Warn when a member's recorded `node.project`/`node.environment` differs from the deploy's and the agent lacks `mesh.peer-sets-v1`. Verify with a deployer test.
- [ ] 2.7 Docs: in `docs/ORCHESTRATION-MODEL.md`, describe mesh addressing (recorded first, lowest free otherwise) and say that `run` does not touch the mesh. In `docs/CONFIGURATION.md`, add the "Shared Nodes" mesh caveat until takod supports peer sets, and the connectivity requirement of `domains hosts --address mesh`. Regenerate `man/` if command help text changes.

## 3. PR3: Node maintenance lease (takod and CLI)

- [ ] 3.1 takod: add the maintenance lease at `<dataDir>/node-leases/maintenance.json` with a frozen v1 format. Enforce mutual exclusion with environment leases under one mutex (`pkg/takod/lease.go`), and advertise `lease.node-maintenance-v1`. Verify with takod unit tests:
  - the maintenance lease blocks environment leases;
  - a live environment lease of any project blocks the maintenance lease, and the error lists the holders;
  - expiry releases both;
  - a lease written before a restart is honored after it.
- [ ] 3.2 CLI: add `AcquireNodeMaintenanceLease(node)` (`pkg/engine/leases.go`) with a 15-minute TTL, no renewal, and the D6 fallback that takes the selected environment's lease and warns. Use it per node in `cmd/upgrade.go` (non-enrolled stage only) and in `cmd/setup.go` converge. Verify with:
  - engine tests using fake agents with and without the capability;
  - an upgrade test proving that the lease is released on the restarted agent and that an environment lease held elsewhere makes the upgrade exit 3.
- [ ] 3.3 Docs: add the node lease and the fallback warning to `docs/ORCHESTRATION-MODEL.md`. Add a `tako upgrade servers` note to `docs/MACHINE-INTERFACE.md` (exit 3 when an environment lease is live).

## 4. PR4: Shared-node mesh peer sets (takod and CLI)

- [ ] 4.1 Write a regression test first: `preview: [a, s]` and `production: [b, s]` are deployed in both orders. Assert:
  - `s` peers with `a` and `b`;
  - `a` and `b` never peer;
  - redeploying `preview` keeps `b` on `s`.

  Confirm the test fails against today's takod.
- [ ] 4.2 takod: add `PUT /v1/mesh/peer-sets` and `DELETE /v1/mesh/peer-sets/<project>/<env>`. Store the sets, render the union under the D7 rules, add `meshPeerSets` to `GET /v1/status`, and advertise `mesh.peer-sets-v1` (`pkg/takod/metadata.go`, `mesh.go`, `server.go`, `pkg/mesh`). Verify with takod tests:
  - union deduplication;
  - an address held by two keys is rejected and nothing is stored or applied;
  - a key holding two addresses is rejected;
  - a CIDR or port mismatch between multi-node sets is rejected;
  - single-node sets with a different `mesh:` block are accepted without changing the interface;
  - delete re-renders the union.
- [ ] 4.3 takod: migrate a legacy `peers.json` (tagged becomes that set; untagged becomes `_legacy`, dropped on the next write). Key a legacy `POST /v1/mesh/apply` by the tags of the just-written `peers.json`. Verify with tests: an old-CLI request sequence against a new takod preserves the other environment's peers.
- [ ] 4.4 CLI:
  - use the peer-set endpoint when every member advertises `mesh.peer-sets-v1`, and use the legacy path per member otherwise;
  - have the D5 used set read `meshPeerSets` when present;
  - have `tako destroy` delete the environment's set on capable members (`pkg/engine/destroy.go`).

  Verify with deployer and engine tests.
- [ ] 4.5 Docs: in `docs/ORCHESTRATION-MODEL.md`, describe peer sets, union rules and conflict errors. In `docs/CONFIGURATION.md` "Shared Nodes", replace the PR2 caveat with the union behavior and the `mesh:` compatibility rule for multi-node environments on a shared server.

## 5. Verification

- [ ] 5.1 For each PR, run `go build ./...` and `go test ./...`. Compare failures against master, because the Windows/WSL baseline has known failures.
- [ ] 5.2 Manual check after PR2, on two test VPSes:
  - deploy `preview` while `production` is powered off;
  - run `tako run` on one node of a two-node environment and confirm both nodes keep their mesh addresses and peers.
- [ ] 5.3 Manual check after PR4: deploy two environments sharing a third server and confirm cross-node traffic for both.
