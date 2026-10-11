# Proposal

## Why

`tako.yaml` accepts keys that do nothing. `deployment.strategy`, `deployment.parallel.*`, `deployment.cache.*` and `environments.<env>.labels` all parse and validate, but no code reads them. `tako init` and docs/CONFIGURATION.md still advertise them. The JSON schema has drifted from the Go config types, and about 20 of the 33 YAML snippets in CONFIGURATION.md use a top-level `services:` key that the strict loader rejects. Users who copy what we document get configs that silently do nothing or fail outright. There are no tests that catch either kind of drift.

## What Changes

- **Deprecate the dead deployment keys.** These are `deployment.strategy`, `deployment.parallel` (with `maxConcurrentBuilds`, `maxConcurrentDeploys` and `strategy`) and `deployment.cache` (with `enabled`, `type` and `retention`). They also include `environments.<env>.labels`, which has no readers either. Configs that set them still load. `tako validate` and `tako deploy` now emit one structured deprecation warning per key, and each warning names what to do instead. `deployment.build` is not touched.
- **BREAKING (scheduled, not in this change):** the warnings ship in `v0.14.0`. In `v0.15.0`, the next minor release, the same keys fail validation with an error that names the replacement. This change defines and documents that schedule. A follow-up change flips the switch.
- **Fix the `tako init` template.** Replace the stale `# registry:` block with a `registries:` example keyed by host, remove the nonexistent `proxy.port` line, remove the dead `deployment.parallel/cache` block, and correct "301 redirect" to 308. The commented optional examples are rebuilt from fragments that tests can validate. `tako init <name>` rejects project names that `tako validate` would reject.
- **Add an init validation test.** It generates every init variant (YAML, JSON, default name and custom name), runs each through the same load and validate path as `tako validate`, and requires zero errors and zero deprecation warnings. Each commented optional example, once merged into the base config, must also validate.
- **Fix the documentation:**
  - Rewrite the CONFIGURATION.md snippets that use a top-level `services:` key into valid shapes.
  - Make the multi-server example satisfy the single-proxy-node ACME rule.
  - Document redirects as 308, which is what the proxy emits.
  - Replace the "Parallel Deployment (Default)" section with an accurate description of dependency-ordered deploys.
  - Fix the Prometheus integration sample in RESOURCE-MONITORING.md, which misuses `file_sd_configs`.
  - Fix the README claim that every command supports `--output json`.
- **Add a docs snippet harness.** A Go test pulls fenced `yaml` blocks out of the enforced docs, reads the HTML comment marker above each one (`complete`, `partial` or `skip`), and validates them.
- **Restore schema parity.** Add the missing service `dockerfile` and `monitoring` and environment `serverSelector` to `schema/tako.schema.json`, and mark the deprecated keys as `deprecated`. A new test walks the `pkg/config` yaml tags against the schema properties, with an explicit allowlist for intentional differences.
- Update the examples that use the dead keys: the four `tako-parallel.yaml` files, and `examples/test-parallel/`, which is renamed to `examples/dependency-order/` (with its README, `project.name`, and the `examples/README.md` catalog row).

## Capabilities

### New Capabilities
- `config-validation`: how Tako treats deprecated and dead config keys (warning, then rejection, with a named replacement), and keeping the published JSON schema in step with the Go config types.
- `project-init`: what `tako init` produces, and the guarantee that every generated variant and documented optional example passes `tako validate`.
- `documentation-accuracy`: config snippets in the enforced docs are machine-checked against the loader, and documented behavior (redirect status, deploy concurrency, machine-output coverage, metrics integration) matches the code.

### Modified Capabilities
- None. There are no main specs under `openspec/specs/` yet.

## Impact

- **Code:**
  - `pkg/config/validator.go`: `ValidationWarnings` gains deprecation findings.
  - `pkg/config/types.go`: the dead getters at :1380-1415 are removed and the deprecated fields are documented. The 301 comment at :545 is corrected.
  - `cmd/init.go`: the template is rebuilt and project names are validated.
  - The test helper for deep-merging snippets moves into a shared package.
- **Tests (new):**
  - `cmd/init_test.go`: the variant matrix.
  - `docs/docs_snippets_test.go`: the snippet harness.
  - `schema/tako_schema_parity_test.go`: the struct-vs-schema walk.
  - `pkg/config` tests for the deprecation warnings.
- **Docs and artifacts:**
  - `docs/CONFIGURATION.md`, `docs/RESOURCE-MONITORING.md`, `docs/ORCHESTRATION-MODEL.md` (snippet markers) and `README.md`.
  - `schema/tako.schema.json`.
  - `examples/*/tako-parallel.yaml`, `examples/test-parallel/` (renamed to `examples/dependency-order/`) and `examples/README.md`.
- **Machine interface:** the `tako validate` result document gains `severity: "warn"` findings with `field` set to the deprecated key path. This is additive and uses the existing finding shape, with no new fields and no exit-code change. Deploy prints the same warnings to stderr.
- **takod:** no node-side change, so no capability gate is needed.
- **Compatibility:** every config that validates today still validates in this release. Only the warning is new.

### Out of scope

- `deploy.rollbackOnFailure`. The `make-rolling-default-deploys` change owns it.
- Implementing parallel service deploys or build-cache configuration. These keys are being deprecated, not built. If parallelism is wanted later, it should come back as a new, designed key.
- Flipping the deprecated keys to hard errors. That happens in the next minor release, in a follow-up change.
- Making the snippet harness mandatory for docs other than CONFIGURATION.md and ORCHESTRATION-MODEL.md. Other docs can opt in with markers.
- Generating the schema from the Go types. The parity test only detects drift.
- Changing the redirect status code. The proxy keeps emitting 308.
