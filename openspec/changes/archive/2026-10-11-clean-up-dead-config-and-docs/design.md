# Design

## Context

See proposal.md (Why) for motivation. The current state below was observed in the code at commit `89e1b05`.

**Dead keys**
- `DeploymentConfig.Strategy/Parallel/Cache` are declared at `pkg/config/types.go:182-201`.
- Their only readers are getters at `pkg/config/types.go:1380-1415`: `GetDeploymentStrategy`, `IsParallelDeployment`, `GetMaxConcurrentBuilds`, `GetMaxConcurrentDeploys` and `IsCacheEnabled`. A repo-wide grep finds no callers for any of them.
- `validateDeploymentConfig` (`pkg/config/validator.go:229-239`) only looks at `deployment.build`.
- `EnvironmentConfig.Labels` (`pkg/config/types.go:751`) has no readers either.

**Actual deploy behavior**
- Services are deployed one at a time in dependency order: `pkg/engine/deploy.go:840-866` runs `dependency.NewResolver` → `ResolveOrder` and then a plain `for` loop.
- Per-node work inside a single service does run concurrently (`pkg/deployer/takod_deploy.go:1257` `runTakodNodeActions`, `:1405-1415`).
- So the documentation claim "Tako deploys services in parallel by default" (`docs/CONFIGURATION.md:1033-1053`) is false, and README.md:249 and :393 overstate it.

**Strict loading**
- YAML is decoded with `KnownFields(true)` (`pkg/config/types.go:1275`, and per service at `pkg/config/service_build.go:135`). JSON uses `DisallowUnknownFields` (`types.go:1261`).
- Any top-level `services:` key therefore fails with an unknown-field error. In docs/CONFIGURATION.md, 20 of the 33 ```yaml fences have one, starting at lines 10, 274, 335, 362, 398, 496, 612 and 672. docs/ORCHESTRATION-MODEL.md has two, at 381 and 438.

**Warning pipeline**
- `config.ValidationWarnings` (`pkg/config/validator.go:188-227`) already returns structured `{Environment, Service, Field, Message}` values.
- `cmd/validate.go:66-73` maps them to `engine.ValidateFinding{Severity:"warn", Field, Message}` (`pkg/engine/validate.go:16-21`).
- `cmd/deploy.go:115-117` prints them to stderr.

**Multi-server example**
- docs/CONFIGURATION.md:301-331 has two nodes, a public service and no `environment.proxy.placement`. It therefore trips `validateEnvironmentProxyACMESafety` (`pkg/config/validator.go:562-603`).
- It also uses `constraints: node.labels.role==web` on servers that have no labels.

**Redirects**
- The proxy emits 308 everywhere (`pkg/takod/proxy_routes.go:527,598-661`, for example the `redir <target> 308` at :647).
- Docs say 301 (`docs/CONFIGURATION.md:620,628`), as do the init comment (`cmd/init.go:289`) and the field comment (`pkg/config/types.go:545`).
- 308 is the correct behavior: it is permanent and, unlike 301, preserves the request method and body. We keep the code and fix the text.

**`tako init`**
- `cmd/init.go:226-521` is a single `fmt.Sprintf` template. Its problems:
  - a stale `# registry: url:` block at :430-433 (the real key is `registries:` keyed by host, `types.go:33-38`);
  - `# port:` under `proxy` at :312 (`ProxyConfig`, `types.go:524-582`, has no port);
  - a dead `# deployment:` block at :390-397;
  - "301" at :289.
- There is no project-name check: `isValidProjectName` lives in `pkg/config/validator.go:56` and is unexported.
- Existing tests (`cmd/init_test.go:15-109`) already load the YAML and JSON outputs through `config.LoadConfig`. They do not cover commented examples, warnings, or a custom name.

**Schema**
- `schema/tako.schema.json` is hand-maintained. `schema/tako_schema_test.go` spot-checks selected paths.
- A property dump shows these keys missing: service `dockerfile` (`types.go:308`), service `monitoring` (`types.go:370`, `MonitoringConfig` at :739, read by `cmd/monitor.go:106`), environment `serverSelector` (`types.go:749`) and environment `labels` (`types.go:751`).
- The dead `deployment.strategy/parallel/cache` are declared at schema lines 301-353.

**Examples**
- `examples/{01-simple-web,09-nextjs-todos,12-hono,16-php}/tako-parallel.yaml` and `examples/test-parallel/tako.yaml` set the dead keys, and `examples/test-parallel/README.md:57-63,195-196` documents them.
- `examples/validate.sh` validates the `tako-parallel.yaml` files, and `examples/examples_test.go:119` asserts that this glob exists.

**Other docs**
- README.md:280-282 says "every command supports `--output json`", but `cmd/root.go:248-261` registers human-only commands, and docs/MACHINE-INTERFACE.md:386-389 lists them.
- docs/RESOURCE-MONITORING.md:179-190 points `file_sd_configs` (a target-discovery mechanism) at a metrics text file. That never ingests metrics.
- The `nc -l` loop at :195-201 serves no HTTP headers, so Prometheus cannot scrape it.

## Goals / Non-Goals

**Goals:**
- Use one mechanism for deprecations that is visible in human output and in the machine result document. It must not change any exit code.
- Make it a test failure for the following to drift from the loader: the init output, the enforced docs snippets, and the schema.

**Non-Goals:**
- A generic deprecation framework with version metadata on struct tags.
- Validating snippets against the JSON schema. Snippets go through the Go loader, which is the source of truth.
- Fixing every doc file. Only CONFIGURATION.md and ORCHESTRATION-MODEL.md are enforced. RESOURCE-MONITORING.md and README.md get targeted text fixes.

## Decisions

### 1. Deprecate the dead keys rather than implement them
**Choice:** keep the fields in `DeploymentConfig` and `EnvironmentConfig` so existing configs still decode. Add a `deprecatedConfigWarnings(cfg)` step at the start of `ValidationWarnings` (validator.go:197). It emits one `ValidationWarning` per present key (`Field` = key path), in this deterministic order: `deployment.strategy`, `deployment.parallel`, `deployment.cache`, then each `environments.<env>.labels` with environment names sorted.

"Present" is detected with non-nil or non-empty checks. Those are sufficient for this change:
- `Parallel` and `Cache` are pointers.
- `Strategy` is a string.
- `Labels` is a map, and an empty `labels: {}` counts as absent. That is acceptable because it is a no-op anyway.

**Message template:** `"<path> is deprecated and has no effect; <guidance>. It will be rejected starting in <RemovalRelease>."` The guidance per key:
- strategy/parallel: "services deploy sequentially in dependency order; use dependsOn for ordering and services.<name>.deploy.strategy for rollout; remove this key"
- cache: "build cache is managed by Docker; use `tako cleanup --docker-cache` or the takod --build-cache-keep-storage setting; remove this key"
- env labels: "set servers.<name>.labels and select them with serverSelector.labels or placement constraints"

**Alternatives considered:**
- Implementing bounded parallel service deploys means concurrency inside the dependency levels, interacting with leases and partial failure. That is a feature, not a cleanup, and nobody depends on it today because it never worked.
- Rejecting immediately would break configs that validate today (four shipped examples among them), which violates the compatibility rule.

The getters at types.go:1380-1415 are deleted. They have no callers, and leaving them invites someone to wire them up by accident.

### 2. Removal schedule
**Choice:** add a single constant, `config.DeprecatedKeysRemovalRelease`. It equals the next minor after the release that first ships the warnings. **Verified:** the latest tag is `v0.13.0`, so the warnings ship in `v0.14.0` and rejection starts in `v0.15.0`. The constant is set to `"v0.15.0"`.

The rejection step belongs to a follow-up change. In that change, `validateDeprecatedConfig` in `ValidateConfig` returns an `InvalidRequestError` with the same guidance text, and the fields stay in the structs so the error is ours rather than a `KnownFields` unknown-field error. Spec requirement "Deprecated keys are rejected in the following minor release" records the contract now, so the docs and warnings can promise it.

### 3. Docs snippet harness
**Location:** a new test-only package, `docs/docs_snippets_test.go` (`package docs_test`). This mirrors `examples/examples_test.go` and `schema/tako_schema_test.go`.

**Markers:** an HTML comment on the nearest preceding non-blank line before a ```yaml / ```yml fence:
- `<!-- tako:snippet complete -->` validates the block as-is.
- `<!-- tako:snippet partial -->` deep-merges the block onto a base config first.
- `<!-- tako:snippet skip: <reason> -->` skips the block. Use it for intentionally invalid examples, non-tako YAML (Prometheus config, GitHub workflow) and removed-key illustrations.

HTML comments render invisibly on GitHub. The enforced files are a list in the test (`CONFIGURATION.md`, `ORCHESTRATION-MODEL.md`). In other files, only marked blocks are checked.

**Base config for `partial`:** `project{name: docs-example, version: 1.0.0}`, `servers.production{host: ${TAKO_PRODUCTION_HOST}, user: root, sshKey: ${TAKO_SSH_KEY}}` and `environments.production.servers: [production]`.

**Merge rules:**
- Mappings merge recursively.
- Scalars and sequences in the snippet replace the base value.
- If the snippet defines top-level `servers`, the base `servers` is dropped. If the snippet also leaves `environments.production.servers` unset, it defaults to all snippet server names in sorted order.

**Pre-check:** before the merge, a top-level `services` key fails the test with the fix-it message from the spec.

**Execution:** the merged YAML is written to a temp dir and loaded with `config.LoadConfig`. `config.ValidationWarnings` must then return nothing. This is the same strict path `tako validate` uses (`cmd/validate.go:61-66`); the harness does not call the cobra command, so it avoids global flag state.

**Environment:**
- `HOME` is set to a temp dir containing `.ssh/id_ed25519`, which matches `examples/validate.sh`.
- A fixed placeholder map covers hosts (`203.0.113.x`), emails, `TAKO_SSH_KEY` and the like.
- Any other `${VAR}` that a snippet references is set to `example-value`, so snippets do not need registering. Snippets that need typed values extend the map.

**Shared helper:** the merge helper and the env setup live in `internal/configtest` (a new package under the existing `internal/`), so `cmd/init_test.go` can reuse them.

**Alternative:** validate snippets against the JSON schema. Rejected: the schema is the artifact that drifts, while the loader is authoritative and also enforces cross-field rules such as the ACME check.

### 4. Rewriting the docs snippets
- Each top-level `services:` snippet is re-nested as `environments: production: services:` and marked `partial`.
- The multi-server example becomes `complete`:
  - add `project:`;
  - add `labels: {role: web}` to both servers;
  - add `environments.production.proxy.placement: {strategy: pinned, servers: [node1]}`, with a short sentence explaining the single-proxy-node ACME rule.
- The "Parallel Deployment (Default)" section becomes "Deploy Order and Concurrency". It covers sequential dependency-ordered services, concurrent per-node rollout and `dependsOn`, and adds a "Deprecated keys" table listing each removed key, its guidance and the removal release.
- The redirect text becomes 308, and the `types.go:545` comment and the init comment are fixed with it.

### 5. Init template restructuring
`generateYAMLConfig` keeps the active base config and the explanatory prose. Optional examples are moved into a `[]initExample{Title, Body}` slice, where each `Body` is valid uncommented YAML using real root key paths. A `commentBlock()` helper renders each one with a `# ` prefix. This replaces the hard-coded commented text for:
- registries;
- notifications;
- runtime/state/mesh overrides;
- multi-server;
- the postgres, redis and worker services, with the `top-level volumes` declarations they need if validation requires them.

The "REFERENCE: Common Service Patterns" tail (`cmd/init.go:477-519`) becomes service-level fragments nested under `environments.production.services`, or is removed if it duplicates the examples. The `# port:` line and the `deployment:` block are deleted. The `deployment.build.strategy` example may stay as an initExample.

`runInit` (`cmd/init.go:32`) validates `projectName` with a newly exported `config.ValidateProjectName`, which wraps `isValidProjectName` with the same message as validator.go:57. It does this before the syscheck and before any write.

**Test:** `TestInitVariantsValidate` loops over {yaml, json} × {"my-app", "shop-api"}:
- it requires `LoadConfig` to succeed and `ValidationWarnings` to be empty;
- for each `initExample`, it merges `Body` into the base YAML with `internal/configtest` and validates the result.

It also asserts the absence of the strings `registry:`, `proxy.port`, `# deployment:\n#   strategy` and `301`.

### 6. Schema parity test
`schema/tako_schema_parity_test.go` walks `reflect.TypeOf(config.Config{})`:
- Pointers are dereferenced.
- Structs are walked by yaml tag, skipping `-` and unexported fields.
- `map[string]T` maps to `additionalProperties`, and `[]T` maps to `items`.
- `StringOrList`, the scalar-or-structured service `build`, and other types with custom unmarshalers are treated as leaves.

Schema nodes are resolved through `$ref` into `definitions`, and properties are unioned across `oneOf`/`anyOf`/`allOf` branches.

**Two directions:**
- (a) every struct key path has a schema property;
- (b) every schema property maps to a struct key.

Both directions consult an `allowlist map[string]string` (path → reason). For example, structured `build.context|args|target` comes from the unexported `structuredServiceBuild`, `service_build.go:12-16`.

The test also asserts that yaml and json tag names match for every field, because `tako.json` goes through the same types.

Deprecated keys are kept in the schema with `"deprecated": true` (draft-07 tolerates the keyword as an annotation) and a description naming the replacement.

**Alternative:** generate the schema from the Go types. Rejected for now: the schema carries hand-written patterns, enums and conditionals (see the backup and ACME branches checked in `tako_schema_test.go`) that a reflector cannot express.

### 7. Machine interface and takod
- No new commands, flags or result fields. Warnings reuse the `ValidateFinding` shape, so `cmd/machine_coverage_test.go` and docs/MACHINE-INTERFACE.md need no change beyond one sentence noting that deprecation warnings appear as `warn` findings.
- There is no node-side behavior change, so no takod capability gate is needed.

### 8. Examples
- Delete the dead blocks from the four `tako-parallel.yaml` files. Because they would then duplicate `tako.yaml`, remove those files and drop `tako-parallel.yaml` from `examples/validate.sh` and from the assertion at `examples/examples_test.go:119`.
- Rename `examples/test-parallel/` to `examples/dependency-order/` in this change (resolved; the old name advertises behavior that does not exist). Set its `project.name` to `dependency-order`, rewrite its README to describe dependency ordering, and update the catalog row at `examples/README.md:52` (link, title "Dependency order", and a description of sequential dependency-ordered deploys). A repo grep shows no other references to `test-parallel` outside `openspec/`.

## Risks / Trade-offs

- **Users see new warnings on every deploy** → each key warns only once, the text says exactly what to delete, and the warnings go to stderr, so JSON stdout is unaffected.
- **Marker discipline adds friction for doc authors** → the failure message prints the three allowed markers and the line number, and `skip` is always available with a reason.
- **The schema walker may surface many pre-existing gaps at once** → the first implementation fills the real gaps (dockerfile, monitoring, serverSelector, plus whatever the walk finds) and allowlists only intentional differences, each with a reason. Reviewers check the allowlist.
- **Environment `labels` might be relied on by external tooling that reads tako.yaml** → the key is only deprecated with a warning for a full minor release. The decision here is to deprecate it rather than implement it. If maintainers prefer to implement it (for example, as node or container labels), that is a separate feature change that removes it from the deprecation list.
- **The placeholder env auto-fill could mask a snippet that needs a typed value** → the validation error points at the snippet line, and the author adds the variable to the fixed map.

## Migration Plan

1. Ship the warnings, the docs, the schema and the tests in a single release (the warning phase). Configs keep working.
2. The release notes list the deprecated keys and the removal release.
3. A follow-up change flips `validateDeprecatedConfig` to an error in the removal release. Rolling back this change is a plain revert, since no data or state formats are touched.
