# Tasks

## 1. Top-level `x-` extension keys

- [x] 1.1 Add `pkg/config/environment_extends.go` with `resolveConfigDocument(data []byte, isJSON bool)` implementing only the fast-path detection and top-level `^x-` stripping (YAML: materialize aliases with `materializeYAMLAliases` first; JSON: ordered raw-message object); wire it into `LoadConfig` between expansion and decode (`pkg/config/types.go:1252-1257`); verify with `go test ./pkg/config -run XKeys` covering YAML anchor-in-`x-` used by two environments, JSON `"x-notes"`, and `extra: true` still rejected with today's message
- [x] 1.2 Add a fast-path regression test asserting that a config with no new keys returns byte-identical input and that an unknown-field error still reports the original `line N` (`go test ./pkg/config -run FastPath`)
- [x] 1.3 Add root `"patternProperties": {"^x-": {}}` to `schema/tako.schema.json` and an assertion in `schema/tako_schema_test.go`; verify `go test ./schema`
- [x] 1.4 Document top-level `x-` keys (with an anchor example) in a new "Environment inheritance and shared fragments" section of `docs/CONFIGURATION.md`; verify the example loads via a test fixture that reads the same snippet

## 2. Merge engine

- [x] 2.1 Implement the D2 merge table for the YAML adapter (`yaml.Node`), including `flattenYAMLMergeKeys` for `<<` inside environment nodes; verify with a table test covering map merge, scalar/sequence replace, kind mismatch (`build` scalar→mapping, `command` string→list), `null` removal, key-order preservation
- [x] 2.2 Implement the same table for the JSON adapter (ordered raw-message objects, scalars/arrays kept as original bytes); run the 2.1 table through both adapters from one test definition and add a round-trip case with large integers and floats; verify `go test ./pkg/config -run EnvironmentMerge`
- [x] 2.3 Record provenance (`env -> dotted path -> declaring env`) during merge, including for removed keys (absent) and whole replaced subtrees; verify with table assertions on provenance in the 2.1/2.2 suite

## 3. Extends resolution, abstract environments, and errors

- [x] 3.1 Add `Extends string` and `Abstract bool` (`omitempty`) to `EnvironmentConfig` (`pkg/config/types.go:747-754`); verify `go build ./...` and that `SaveConfig` of a resolved config emits neither key (unit test)
- [x] 3.2 Implement chain resolution (memoized DFS, cycle detection incl. self-reference, unknown target, non-string `extends`, max 8 hops) and strip `extends`/`abstract` from resolved nodes; verify tests for `a->b->c` precedence, `a<->b` cycle message `a -> b -> a`, unknown `prod`, depth 9 rejected, depth 8 accepted
- [x] 3.3 Pre-decode each declared environment strictly against its original node (D5) so unknown fields name the declaring environment; verify a test where a typo in abstract `base` fails naming `base` and not `production`
- [x] 3.4 Drop abstract environments from the re-encoded document, attach `environmentResolution` to `Config`, and add accessors `EnvironmentExtendsChain`, `EnvironmentValueSource`, `IsAbstractEnvironment`; verify unit tests for each accessor
- [x] 3.5 Make `GetEnvironment` (`types.go:851-864`) return the abstract-environment error listing children, and change the empty-environments message in `ValidateConfig` (`pkg/config/validator.go:112-114`) only when abstract environments exist; verify tests for `-e base` error text, default env skipping abstract `base`, and all-abstract config rejected
- [x] 3.6 Wrap `ValidateConfig` errors whose `environment <name>` prefix names an inheriting environment with `(environment <name> extends <chain>)` in `LoadConfig`; verify a test with `staging` extends `production` and `servers: [missing]`, and that non-inheriting error text is unchanged
- [x] 3.7 Add tests that `SecretSourcesFor` on a resolved child returns top-level + inherited parent sources, and top-level + child sources when the child declares its own list (`pkg/config/secret_sources_test.go`); verify `go test ./pkg/config -run SecretSources`
- [x] 3.8 Add `extends` (string) and `abstract` (boolean) to the environment schema (rebase on `clean-up-dead-config-and-docs`, which adds `serverSelector` and deprecates environment `labels`); extend `schema/tako_schema_test.go` to assert `extends` and `abstract` exist; verify `go test ./schema`
- [x] 3.9 Document `extends`, merge rules table, `null` removal, list replacement (with `secretSources` example), chains/limits, and `abstract` in the `docs/CONFIGURATION.md` section from 1.4; convert `examples/deployment-patterns/10-stages-shared-node/tako.yaml` to `abstract` base + `extends`, and verify it with an example-loading test (or `tako validate` against it with placeholder env vars)
- [x] 3.10 Add an unused-abstract warning to `config.ValidationWarnings` (`pkg/config/validator.go:188-227`): one `ValidationWarning{Environment, Field: "abstract"}` per abstract environment with no children; verify a `pkg/config` test and a `cmd/validate_test.go` case asserting a `warn` finding and exit 0, and no warning when the abstract environment is extended
- [x] 3.11 Add a test that an `x-` key inside a service or environment is still rejected as an unknown field; verify `go test ./pkg/config -run XKeys`
- [x] 3.12 Let an inheriting or abstract environment declare a partial structured `build` (for example only `target`): relax the "context is required" check in the per-environment pre-decode (D5) for those environments only, then require a non-empty `context` on every resolved deployable inheriting environment after the merge, with the error ending in `(environment <name> extends <chain>)`; verify tests for an inherited context, a chain with no context naming the chain, and an unchanged error for a non-extending `build: {target: x}`

## 4. config explain provenance

- [x] 4.1 Export `config.ParseConfigDocument(data []byte, isJSON bool)` sharing the resolver with `LoadConfig`, and replace `loadRawConfigForExplain` (`cmd/config.go:141-170`) with it; verify `go test ./cmd -run ConfigExplain` passes for an existing config and for one using `extends` + `x-` keys
- [x] 4.2 Print `Environment: <env> (extends a -> b)` and append `, from <env>` to source labels of environment-scoped lines (`cmd/config.go:88-136`) when a chain exists; verify golden-output tests for an inherited `replicas` and a child-overridden `replicas`, and that password redaction is unchanged
- [x] 4.3 Update `configExplainCmd.Long` (`cmd/config.go:26-28`) to mention inheritance, regenerate man pages with `make man`, and verify `man/tako-config-explain.1` diff contains only the new text; confirm `cmd/machine_coverage_test.go` still lists `tako config explain` as human-only (no change expected) with `go test ./cmd -run Machine`

## 5. Deterministic default environment (design D9)

- [x] 5.1 Add `(c *Config) ResolveDefaultEnvironment() (name string, implicit bool)` in `pkg/config/types.go` (only environment → production → first after `sort.Strings`, over non-abstract environments) and make `GetDefaultEnvironment` (`types.go:828-847`) wrap it; verify table tests for one env, `production` present, `{staging, dev}` → `dev` with `implicit=true`, and an abstract env excluded; add a determinism test that resolves `{staging, dev}` 100 times and always gets `dev`
- [x] 5.2 Update `getEnvironmentName` (`cmd/root.go:349-360`) to use `ResolveDefaultEnvironment` and print `no -e and no 'production'; using '<name>'; pass -e` once to stderr when `implicit` is true; verify a cmd test that the line goes to stderr only (stdout under `--output json` is unchanged) and is absent when `production` exists or `-e` is given
- [x] 5.3 Update the `-e/--env` help at `cmd/root.go:278` to describe the selection order, regenerate man pages with `make man`, and verify the man diff touches only the `-e` description and that `go test ./cmd -run Completion` still passes

## 6. Integration checks

- [x] 6.1 Load every `examples/**/tako.yaml` through `LoadConfig` with placeholder env vars before and after the change and verify identical resolved configs (fast path untouched)
- [x] 6.2 Add an engine-level test that the desired state built for an inheriting environment contains no `extends`/`abstract` and equals the state built from the equivalent hand-flattened config; verify `go test ./pkg/engine -run Extends`
- [x] 6.3 Run `go build ./...` and `go test ./...`; verify no new failures beyond the known Windows-only ones
