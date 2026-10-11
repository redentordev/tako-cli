# Design

## Context

See proposal.md (Why) for motivation and specs/environment-config/spec.md for the behavior contract. Observed facts that shape the approach:

- **Load pipeline** — `LoadConfig` (`pkg/config/types.go:1195-1294`) runs: `.env` load (`:1217-1233`) → raw-byte credential checks `validateRawRegistryCredentials` (`pkg/config/registries.go:28`) and `validateRawACMEDNSCredentials` (`pkg/config/acme_dns.go:25-52`) (`types.go:1243-1248`) → textual `${VAR}` expansion `expandEnvWithTrim` (`types.go:1252`, defined `:1141`) → strict decode, JSON with `DisallowUnknownFields` (`:1259-1271`) or YAML with `KnownFields(true)` (`:1274-1278`) → `normalizeConfigRelativePaths` (`:1281`, `:1296-1337`) → `materializeDefaultPlatformInventory` (`:1282`) → `ValidateConfig` (`:1286`).
- **Environment model** — `EnvironmentConfig` (`types.go:747-754`) has `servers`, `serverSelector`, `proxy`, `labels`, `services`, `secretSources`; `Config.Environments` is a plain map (`types.go:44`). The schema has no `additionalProperties: false` at the root (checked: root keys are only listed under `properties`), so `x-` keys are already schema-legal; only the Go decoder rejects them. The environment schema (`schema/tako.schema.json:446-...`) lists `servers`, `secretSources`, `proxy`, `services` only — `serverSelector` and `labels` are already missing (pre-existing drift).
- **Typed decode is not mergeable** — `ServiceConfig` has custom `UnmarshalYAML`/`UnmarshalJSON` (`pkg/config/service_build.go:20-60`, `:87-145`) carrying unexported state (`buildStructured`), and many fields are plain `int`/`bool`/`string` where zero and "unset" are indistinguishable (e.g. `replicas: 0` vs absent). Merging typed structs would need per-field "set" tracking. Merging must therefore happen on the document tree before typed decode.
- **Line numbers are already lost inside services** — `ServiceConfig.UnmarshalYAML` materializes aliases and re-marshals each service node before strictly decoding it (`service_build.go:91-139`), so errors inside a service already do not carry original line numbers. Errors at environment/top level do (`cmd/config_error.go:13-40`, `yamlErrorLine` `:59-64`).
- **Alias tooling exists** — `materializeYAMLAliases` (`service_build.go:198-239`, bounded by `maxYAMLAliasExpansionNodes/Depth`) and merge-key lookup `yamlMappingValue` (`:150-180`).
- **Environment selection** — `getEnvironmentName` (`cmd/root.go:349-360`) uses `--env` or `Config.GetDefaultEnvironment` (`types.go:828-847`: single env, else `production`, else first map key — nondeterministic despite the "alphabetically" comment). `GetEnvironment` (`types.go:851-864`) returns `environment '%s' not found`.
- **Validation** — `ValidateConfig` loops every environment (`pkg/config/validator.go:80-84` secretSources, `:111-124` names + `validateEnvironment`); `validateEnvironment` requires `servers` or `serverSelector` (`validator.go:357-358`). Messages are prefixed `environment <name>:`.
- **Secret sources** — `Config.SecretSourcesFor` (`pkg/config/secret_sources.go:38-44`) returns top-level sources followed by the environment's own; later override earlier key by key.
- **config explain** — `runConfigExplain` (`cmd/config.go:37-139`) loads the expanded config plus a second *raw, unexpanded* copy via `loadRawConfigForExplain` (`cmd/config.go:141-170`), which strict-decodes the file itself and indexes `rawCfg.Environments[envName]` (`:61`, `:115`). It labels values `config`/`env`/`default` only. Registered human-only (`cmd/machine_coverage_test.go:83`).
- **config export** — `MaterializeConfigExport` (`pkg/engine/config_export.go:239-265`) rebuilds a config from takod desired/actual state via `configmaterialize.BuildConfig`; it never reads local `tako.yaml`.

## Goals / Non-Goals

**Goals:**
- One merge algorithm, applied to the document tree, identical for YAML and JSON, run before typed decode and before `ValidateConfig`.
- Zero behavior change (including error text and line numbers) for configs that use none of the new keys.
- Provenance (which declared environment supplied each leaf) recorded during merge and exposed to `config explain` and error formatting.
- No takod involvement.

**Non-Goals:**
- Changing the typed `Config` API seen by deploy code: downstream code keeps reading `cfg.Environments[name]` and sees flattened data.
- Fixing the existing schema drift beyond the fields this change touches (the `clean-up-dead-config-and-docs` change owns schema parity: it adds `serverSelector` and marks environment `labels` deprecated, so this change only adds `extends` and `abstract`).

## Decisions

### D1. Resolve on the parsed document tree, after `${VAR}` expansion, before strict typed decode
New step in `LoadConfig` between expansion (`types.go:1252`) and decode (`:1257-1279`): `resolveConfigDocument(expanded []byte, isJSON bool) (resolved []byte, meta *environmentResolution, err error)` in a new file `pkg/config/environment_extends.go`.
- Fast path: parse once; if the document has no top-level `x-` key and no environment with `extends` or `abstract`, return the original bytes untouched, so the existing decoders run on the original text (preserves line numbers and messages — spec "Config without extends is unaffected").
- Slow path: strip top-level `x-` keys, pre-validate each declared environment (D5), merge (D2), drop abstract environments, re-encode, then run the existing strict decoder on the re-encoded bytes.
- Expansion stays before resolution because it is textual over the whole file (`expandEnvWithTrim`); ordering does not change values, since expansion never introduces keys. The raw-byte credential checks keep running on the original bytes; they iterate declared environments, so an inherited ACME credential was already checked where it was declared and a child override is checked in the child (`acme_dns.go:35-50`). Their lenient decoders ignore `extends`/`abstract`/`x-` keys.
- `normalizeConfigRelativePaths` (`types.go:1296-1337`) keeps running after decode; inherited relative `build`/`envFile`/`files` paths come from the same file, so resolving them against the same config directory is correct.
- Alternative rejected: merge typed `EnvironmentConfig` values after decode — cannot distinguish "unset" from zero values and fights the custom `ServiceConfig` unmarshalers (Context).
- Alternative rejected: always re-encode — would shift line numbers for every existing config.

### D2. Merge rules (single table, two tree adapters)
For child node C over resolved parent node P at the same path:

| P \ C | absent | `null` | scalar | sequence | mapping |
|---|---|---|---|---|---|
| absent | absent | absent | C | C | C |
| scalar or sequence | P | key removed | C | C | C |
| mapping | P | key removed | C | C | recursive merge; key order: P's keys, then C's new keys |

- Lists replace. Rationale: list elements in tako.yaml have no stable identity (`servers`, `volumes`, `ports`, `command`, `secretSources`); append would make removal impossible without a directive syntax, and element-wise merge is surprising. This matches Kamal's destination merge (ActiveSupport `deep_merge` replaces arrays). `!reset`/`!override` tags were considered and rejected as unnecessary once `null` deletes.
- `null` deletes only in an environment that declares `extends` (and only at paths inside that environment). In non-extending environments the tree is not touched, so `null` keeps today's decode-to-zero meaning.
- `services` needs no special case: it is a mapping keyed by service name, so per-service merge and `services: {worker: null}` removal fall out of the generic rule.
- Union fields (`build` scalar vs mapping, `command`/`entrypoint` string vs list) follow the kind-mismatch row: child replaces.
- `secretSources` follows the sequence rule; `SecretSourcesFor` (`secret_sources.go:38-44`) is unchanged and still prepends top-level sources to the resolved environment list.
- YAML adapter: operates on `yaml.Node`. Before merging, each environment node is passed through `materializeYAMLAliases` (reuse, same limits) and a new `flattenYAMLMergeKeys` that rewrites `<<` into explicit keys with YAML precedence (explicit keys win, then earlier merge sources), reusing the lookup order of `yamlMappingValue` (`service_build.go:150-180`). This is required because a child using `<<: *anchor` must merge by effective keys.
- JSON adapter: lossless — objects decoded as ordered `[]struct{key string; value json.RawMessage}` recursively; scalars/arrays stay as original `RawMessage` bytes. Re-encoded JSON is syntactically valid, so `jsonErrorLocation` (`cmd/config_error.go:86-96`, `SyntaxError` only) never maps a re-encoded offset onto the original file.
- One table-driven test suite runs every merge case through both adapters.

### D3. Chain resolution
Resolve with memoized DFS from each environment: state `visiting`/`done`; encountering a `visiting` node returns `environment extends cycle: a -> b -> a`. Unknown target: `environment staging extends unknown environment "prod"`. Max 8 hops (`maxEnvironmentExtendsDepth = 8`) — deep enough for base → region → stage → variant patterns, small enough to keep provenance readable; exceeding it errors with the chain. `extends` must be a non-empty string naming a valid identifier; any other node kind is an error at the declaring line. `extends` and `abstract` are stripped from every resolved node and not inherited.

### D4. Abstract environments
`abstract: true` environments are resolved (so children can extend them) but removed from the re-encoded `environments` map. Consequences, all without touching downstream code:
- `ValidateConfig` never sees them, so they need not have `servers` (`validator.go:357`).
- `GetDefaultEnvironment` (`types.go:828-847`) never returns them.
- Deploy/history/state code never sees them.
The resolution metadata is attached to `Config` as an unexported field (`resolution *environmentResolution`, `yaml:"-" json:"-"` not needed since unexported) holding: declared names, abstract names with their children, extends chain per resolved env, and provenance map `env -> dotted path -> declaring env`. Accessors: `(*Config).EnvironmentExtendsChain(env) []string`, `(*Config).EnvironmentValueSource(env, path) string`, `(*Config).IsAbstractEnvironment(name) bool`.
`GetEnvironment` (`types.go:859-861`) checks `IsAbstractEnvironment` before "not found" and returns `environment 'base' is abstract and cannot be deployed; use one of: production, staging`. `ValidateConfig`'s empty check (`validator.go:112-114`) becomes `at least one non-abstract environment must be configured` when abstract environments exist (unchanged text otherwise). Abstract environment names still go through `isValidRuntimeIdentifier` during pre-validation. An abstract environment with no children is allowed (useful while editing) and only strict-decoded (D5). It is not an error, but `config.ValidationWarnings` (`pkg/config/validator.go:188-227`) adds one warning per such environment (resolved): `Environment: <name>`, `Field: "abstract"`, message `environment <name> is abstract and no environment extends it; it has no effect`. `tako validate` reports it as a `warn` finding and `tako deploy` prints it to stderr, through the existing warning pipeline. The warning reads the resolution metadata (`IsAbstractEnvironment` plus the children index).
Alternative rejected: keep abstract envs in `Environments` with a flag — every consumer that iterates environments (validation, `tako status` style loops, completion) would need a filter.

### D5. Error attribution
- Strict-decode errors in the slow path would otherwise refer to re-encoded text. To keep them useful, each *declared* environment node (minus `extends`/`abstract`) is first strictly decoded into `EnvironmentConfig` on its own, using a decoder over the original node, so unknown-field/type errors name the declaring environment and, for top-level environment fields, the original line, e.g. `environment base: line 14: field servrs not found in type config.EnvironmentConfig`. For service-internal typos the environment is named and the field is reported as today; no original line is promised, because the nested `ServiceConfig` decoder already drops inner line numbers (Context). The final decode of the merged document then cannot fail on unknown fields.
- Semantic errors from `ValidateConfig` for a resolved environment that has a chain are wrapped in `LoadConfig`: the message gains `(environment staging extends production)`. `ValidateConfig` itself stays chain-unaware; wrapping uses the `environment <name>` prefix already present in its messages (`validator.go:358,365,...`) to find the env. Messages without that prefix are left alone.
- `formatDeployConfigError` (`cmd/config_error.go:13-40`) needs no new branches; the existing `invalid config:` prefix is preserved.

### D6. `x-` keys
Only top-level keys matching `^x-` are stripped (after alias materialization in YAML, so anchors defined inside them still resolve). Values are not decoded or validated. Keys elsewhere, including inside `services` and `environments`, are not stripped and remain strict errors. This is final for this change (resolved): there is no plan to allow nested `x-` keys; top-level is sufficient for anchors. Env names like `x-foo` are unaffected because stripping is top-level only. Schema: add root `"patternProperties": {"^x-": {}}` for documentation/editor completion.

### D7. config explain
`loadRawConfigForExplain` (`cmd/config.go:141-170`) is replaced by an exported `config.ParseConfigDocument(data []byte, isJSON bool) (*Config, error)` that runs D1–D6 on raw (unexpanded) bytes without validation, so explain keeps its `env` vs `config` source detection and gains provenance. Output changes (human-only, `cmd/machine_coverage_test.go:83`, no machine contract):
- Header: `Environment: staging (extends production -> base)`.
- Each environment-scoped line (servers, service `source`, `replicas`, `deploy.strategy`, `proxy.*`) appends `, from <env>` to the existing source label when the selected environment has a chain, e.g. `replicas: 2 (config, from production)`. Paths not set anywhere stay `(default)`. Password redaction is unchanged (`cmd/config.go:101-102`).
`LoadConfig` is refactored to call the same `ParseConfigDocument` core so explain and deploy cannot diverge.

### D8. State, history, export, takod
Resolution completes before any engine code runs, so desired state, deploy history, and lease documents contain only flattened environments. `EnvironmentConfig.Extends/Abstract` fields exist for strict pre-decode but are always zero on resolved environments; they are tagged `omitempty` so `SaveConfig` (`types.go:1348-1378`) and `configmaterialize` output never emit them. `config export`/`pull` (`pkg/engine/config_export.go:239-265`) read takod state, not local files, and need no change. No takod behavior changes, so no capability gate is required.

### D9. Deterministic default environment (bug fix, in scope)
**Observed bug.** `GetDefaultEnvironment` (`pkg/config/types.go:828-847`) falls back to the first key of a Go map iteration when there is more than one environment and none is named `production`. Despite the "alphabetically" comment, the result changes from run to run, so `tako deploy` with `{staging, dev}` may target either. Abstract environments make multi-environment configs without `production` more likely, so this change fixes it.

**Fix.**
- Add `(c *Config) ResolveDefaultEnvironment() (name string, implicit bool)`. Selection over the resolved (non-abstract) environments:
  1. exactly one environment → that one, `implicit = false`;
  2. otherwise `production` if present → `production`, `implicit = false`;
  3. otherwise the first name after `sort.Strings` → that name, `implicit = true`;
  4. no environments → `""`, `false`.
- `GetDefaultEnvironment` becomes a thin wrapper returning only the name, so every existing caller becomes deterministic with no signature change.
- `getEnvironmentName` (`cmd/root.go:349-360`) calls `ResolveDefaultEnvironment` when `-e` is not given. When `implicit` is true it prints exactly one line to stderr, for example `no -e and no 'production'; using 'dev'; pass -e`. It is written to stderr in every output mode, so machine stdout is unchanged; it is not an event.
- The `-e/--env` help (`cmd/root.go:278`) becomes `environment to use (default: the only environment, else production, else the first alphabetically)`. Man pages are regenerated (`make man`); the flag is a root persistent flag, so every page that lists inherited options changes.
- *Why not fail closed:* requiring `-e` whenever the default is ambiguous would break read-only commands (`tako status`, `tako logs`, `tako config explain`) for existing multi-environment configs. A deterministic choice plus a visible notice fixes the bug without breaking anyone.

## Risks / Trade-offs

- [Slow-path errors inside merged services lose original line numbers] → Already true today for service internals (`service_build.go:130-137`); D5 pre-decodes declared environments against original nodes so typos are still attributed to the declaring environment.
- [Deep merge can combine mutually exclusive fields, e.g. parent `servers` + child `serverSelector`] → Validation still runs on the resolved environment; error is wrapped with the chain (D5); docs show `servers: null` to clear.
- [List replacement surprises users expecting append (e.g. `secretSources`)] → Documented explicitly with an example; `x-` anchors give a concise way to repeat shared lists.
- [JSON via YAML-style merge could alter number formatting] → JSON adapter keeps scalars as original `RawMessage` bytes (D2); round-trip test with large ints and floats.
- [YAML alias materialization blow-up on huge anchor graphs] → Reuses existing bounded materializer (`maxYAMLAliasExpansionNodes = 100000`, depth 100).
- [Sibling changes adding new environment keys (e.g. hooks in `add-local-deploy-hooks`)] → Generic merge covers them automatically; if a new key needs non-default semantics (e.g. append), that change must update the D2 table and spec.

## Migration Plan

Additive; no migration. Existing configs take the fast path. Users may optionally convert duplicated environments to `extends`; `examples/deployment-patterns/10-stages-shared-node/tako.yaml` is updated as the reference. Rollback: older CLIs reject `extends`/`abstract`/`x-` with today's unknown-field error, which is the correct fail-closed behavior.

- [Configs with several environments and no `production` that relied on whichever environment Go's map order happened to pick] → The choice is now always the alphabetically first one, with a stderr notice telling the user to pass `-e`.

## Accepted Assumptions

- No current config relies on `null` inside an environment to mean anything other than "unset"; `null` deletion applies only in environments that declare `extends`, so existing files are unaffected regardless.
- 8 hops is sufficient; it is a constant and can be raised without a spec change other than the stated number.
