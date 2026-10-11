# Tasks

## 1. Deprecation warnings for dead keys

- [ ] 1.1 Add `config.DeprecatedKeysRemovalRelease = "v0.15.0"` (warnings ship in `v0.14.0`; latest tag verified as `v0.13.0`) and a `deprecatedConfigWarnings(cfg)` step at the start of `ValidationWarnings` (`pkg/config/validator.go:197`). It emits warnings for `deployment.strategy`, `deployment.parallel`, `deployment.cache` and each `environments.<env>.labels` (environments sorted), using the message template from design §1. Verify with new table tests in `pkg/config/validator_deprecation_test.go`: one warning per key, exact `Field` values, the guidance text, and none for `deployment.build` only.
- [ ] 1.2 Delete the uncalled getters at `pkg/config/types.go:1380-1415`, and mark the `DeploymentConfig.Strategy/Parallel/Cache` and `EnvironmentConfig.Labels` fields with `// Deprecated:` comments. Verify that `go build ./...` and `go vet ./pkg/config` pass.
- [ ] 1.3 Add a `cmd/validate_test.go` case: a config with `deployment.cache` under `--output json` gives `valid: true`, a `warn` finding with `field: "deployment.cache"`, and exit 0. Add a deploy-side assertion that stderr contains `Warning: deployment.cache is deprecated` before any server is contacted (reuse the existing deploy preflight test harness). Verify with `go test ./cmd -run 'Validate|Deploy'`.
- [ ] 1.4 Add one sentence to docs/MACHINE-INTERFACE.md (validate section) saying that deprecation warnings appear as `warn` findings with `field` set to the key path. Verify that `go test ./cmd -run MachineCoverage` still passes.

## 2. Example configs using dead keys

- [ ] 2.1 Remove `examples/{01-simple-web,09-nextjs-todos,12-hono,16-php}/tako-parallel.yaml`. Drop the `tako-parallel.yaml` glob from `examples/validate.sh` and from the expectation at `examples/examples_test.go:119`. Verify with `go test ./examples`.
- [ ] 2.2 Remove the `deployment:` block from `examples/test-parallel/tako.yaml`, and rewrite `examples/test-parallel/README.md` (the sections at :57-63 and :195-196) to describe sequential dependency-ordered deploys with concurrent per-node rollout. Verify that `tako --config examples/test-parallel/tako.yaml validate` prints no warnings.
- [ ] 2.3 Rename `examples/test-parallel/` to `examples/dependency-order/` with `git mv`. Change its `project.name` from `test-parallel` to `dependency-order`, update any self-references in its README, and update the catalog row at `examples/README.md:52` (link `./dependency-order/`, title "Dependency order", description of sequential dependency-ordered deploys instead of "Parallel deployment" / "concurrent service deployment"). Verify with `rg -n test-parallel --glob '!openspec/**'` returning nothing, `tako --config examples/dependency-order/tako.yaml validate` printing no warnings, and `go test ./examples` passing.

## 3. Shared snippet/config test helper

- [ ] 3.1 Create `internal/configtest` with `MergeYAML(base, fragment []byte) ([]byte, error)` following the merge rules in design §3, `SetupEnv(t)` (temp HOME with `.ssh/id_ed25519` plus the placeholder env map and an auto-fill for any other `${VAR}`), and `LoadAndValidate(t, dir, name, content) (*config.Config, []config.ValidationWarning, error)`. Verify with unit tests covering map merge, sequence replacement, the server-replacement rule, and auto-filled vars.

## 4. `tako init` template and validation

- [ ] 4.1 Export `config.ValidateProjectName` (wrapping `isValidProjectName`, with the message from validator.go:57) and call it at the top of `runInit` (`cmd/init.go:32`). Verify with a test that `tako init My_App` in a temp dir errors and creates none of `tako.yaml`, `tako.json`, `.env.example` or `.gitignore`.
- [ ] 4.2 Restructure `generateYAMLConfig` (`cmd/init.go:226`): move the optional examples into an `[]initExample{Title, Body}` slice rendered by `commentBlock()`. Replace `# registry:` (:430-433) with a `registries: ghcr.io: {username: ${REGISTRY_USER}, password: ${REGISTRY_TOKEN}}` example. Delete `# port:` (:312) and the `# deployment:` block (:390-397). Change "301" to "308" (:289). Re-nest the reference service patterns (:477-519) under `environments.production.services`. Verify that `go test ./cmd -run Init` passes.
- [ ] 4.3 Add `TestInitVariantsValidate` covering {yaml, json} × {"my-app", "shop-api"}: `LoadConfig` succeeds and `ValidationWarnings` is empty. For each `initExample`, merge `Body` into the base via `internal/configtest` and validate. Also assert that `registry:`, `301` and any `deployment.parallel/cache` examples are absent. Verify with `go test ./cmd -run TestInitVariantsValidate`.
- [ ] 4.4 Regenerate man pages if the `init` help text changed (`make man`), and verify that `git diff --exit-code man/` is clean afterwards or that the diff is committed.

## 5. Schema parity

- [ ] 5.1 Add service `dockerfile` and `monitoring` (`enabled`, `interval`, `webhook`, `checkType` enum `http|container`) and environment `serverSelector` (`labels` map, `any` bool) to `schema/tako.schema.json`. Mark `deployment.strategy`, `deployment.parallel`, `deployment.cache` and environment `labels` with `"deprecated": true` and replacement descriptions. Verify with `go test ./schema`.
- [ ] 5.2 Add `schema/tako_schema_parity_test.go` implementing the bidirectional reflect walk, `$ref`/`oneOf`/`anyOf`/`allOf` resolution, and the yaml/json tag-equality check from design §6, with a reasoned allowlist. Fix or allowlist each gap it reports. Verify with `go test ./schema`, and confirm that temporarily removing `dockerfile` from the schema makes the test fail with the path `environments.*.services.*.dockerfile`.

## 6. Docs snippet harness and CONFIGURATION.md rewrite

- [ ] 6.1 Add `docs/docs_snippets_test.go` (`package docs_test`). It parses ```yaml/```yml fences and their markers (`complete` / `partial` / `skip: <reason>`), enforces markers in CONFIGURATION.md and ORCHESTRATION-MODEL.md, rejects a top-level `services:` key with the fix-it message, and validates through `internal/configtest`. Verify that it initially fails, listing the unmarked blocks.
- [ ] 6.2 Re-nest each of the 20 top-level `services:` snippets in docs/CONFIGURATION.md (starting at lines 10, 274, 335, 362, 398, 496, 612, 672, ...) under `environments.production.services`, and add markers to all 33 blocks. Verify that the CONFIGURATION.md subtests of `go test ./docs` pass.
- [ ] 6.3 Fix the multi-server example (docs/CONFIGURATION.md:301-331): add `project`, add server `labels: {role: web}`, add `environments.production.proxy.placement` pinned to `node1`, and explain the single-proxy-node ACME rule in one sentence. Mark it `complete`. Verify that it passes in `go test ./docs`.
- [ ] 6.4 Change the redirect text at docs/CONFIGURATION.md:620,628 to 308 (permanent, preserves method and path), and fix the comment at `pkg/config/types.go:545`. Verify with `rg -n "301" docs/CONFIGURATION.md cmd/init.go pkg/config/types.go`, which should return no redirect references.
- [ ] 6.5 Replace "Parallel Deployment (Default)" (docs/CONFIGURATION.md:1033-1053) with "Deploy Order and Concurrency" plus a "Deprecated keys" table (key, guidance, removal release). Keep the `deployment.build` example. Verify that `go test ./docs` passes and that the section no longer contains `maxConcurrent`.
- [ ] 6.6 Add markers to the two `services:` snippets in docs/ORCHESTRATION-MODEL.md (:381, :438) and the rest of its yaml blocks, re-nesting where needed. Verify that its subtests in `go test ./docs` pass.

## 7. Targeted doc text fixes

- [ ] 7.1 README.md:280-282: say that commands with the machine contract support `--output json` and `--events ndjson`, and link the human-only list in docs/MACHINE-INTERFACE.md. At README.md:249 and :393, replace "parallel deployment" with "concurrent per-node rollout in dependency order". Verify by reading the diff and by `rg -n "every command supports" README.md` returning nothing.
- [ ] 7.2 docs/RESOURCE-MONITORING.md:169-201: replace the `file_sd_configs` sample with a node_exporter textfile-collector recipe (write to a temp file, then `mv` to `/var/lib/node_exporter/textfile/tako.prom`; run node_exporter with `--collector.textfile.directory`; scrape node_exporter). Remove the `nc -l` loop or replace it with a note that a real HTTP server is required. Mark the Prometheus YAML block `<!-- tako:snippet skip: prometheus config -->`. Verify with `rg -n file_sd_configs docs/` returning nothing.

## 8. Integration check

- [ ] 8.1 Run `go build ./...`, `go test ./pkg/config ./cmd ./schema ./docs ./examples ./internal/...` and `bash examples/validate.sh` (on Linux/CI). Verify that all pass, apart from the known Windows-only failures noted in project context.

## Workflow follow-up

- Release notes for the warning release must list the deprecated keys, the guidance and `DeprecatedKeysRemovalRelease`.
- Open a follow-up change before the removal release that turns the warnings into validation errors (spec requirement "Deprecated keys are rejected in the following minor release").
- Archive this change after review.
