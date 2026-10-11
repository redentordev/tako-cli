# Proposal

## Why

Every environment in `tako.yaml` is a fully independent map (`EnvironmentConfig`, `pkg/config/types.go:747-754`), so a project with `preview`, `staging`, and `production` repeats the same service definitions and changes only a domain, a replica count, or a server list (see `examples/deployment-patterns/10-stages-shared-node/tako.yaml`, where two environments differ in two values out of ~18 lines each). The only reuse tool today is YAML anchors, and because the loader decodes strictly (`pkg/config/types.go:1274-1276`, JSON at `:1259-1261`), anchors must be parked inside a known key (`examples/sentry/tako.yaml:51` anchors a whole `services:` block inside `production`) and there is no scratch area for shared fragments. Kamal users expect destination overlays (`deploy.<dest>.yml` deep-merged over `deploy.yml`) and Compose users expect `x-` extension keys; both are missing.

## What Changes

- New optional environment key `extends: <envName>`: the environment is resolved by deep-merging its own keys over the fully resolved parent environment. Merge rules: mappings merge recursively by key (services merge by service name), scalars and sequences in the child replace the parent value, a node whose kind differs from the parent's replaces it, and an explicit `null` in the child removes the inherited key (for example `services: { worker: null }` drops an inherited service; `servers: null` lets a child switch to `serverSelector`).
- Environment `secretSources` follow the sequence rule (child list replaces the inherited list); top-level `secretSources` are still prepended for every environment exactly as today.
- Chains are allowed (`a -> b -> c`) up to a maximum depth of 8 `extends` hops; cycles, self-references, and references to unknown environments are validation errors that name the full chain.
- New optional environment key `abstract: true`: an abstract environment is a template only. It is not validated as a deployable environment, is never chosen as the default environment, cannot be selected with `-e`, and is absent from the resolved config. `abstract` is not inherited.
- Top-level keys that start with `x-` are accepted and ignored in both YAML and JSON configs, so shared fragments and YAML anchors can live in an obvious scratch area.
- `tako validate` (and `tako deploy` on stderr) warns about an abstract environment that no environment extends.
- **Bug fix:** default-environment selection becomes deterministic. Without `-e`, Tako uses the only non-abstract environment, else `production`, else the alphabetically first environment name. In that last case it prints one stderr line, for example `no -e and no 'production'; using 'dev'; pass -e`. Today the last case picks a random map key. The `-e` help text and man pages are updated to match.
- `tako config explain` shows the extends chain for the selected environment and, for each environment-scoped value it prints, which declared environment supplied it.
- `tako validate` / config load errors for an inheriting environment name the environment that declared the offending key (strict-decode errors) or the extends chain (semantic validation errors).
- `schema/tako.schema.json` gains `extends`, `abstract`, and a root `patternProperties` entry for `^x-`; docs/CONFIGURATION.md gains an "Environment inheritance" section; man pages regenerate.

No breaking change: every config that validates today has no `extends`/`abstract` keys and no top-level `x-` keys (both are rejected by the strict decoder today), and such configs keep taking the existing decode path unchanged. No CLI flag or JSON result document changes (only the `-e` help text changes); `config explain` is registered human-only (`cmd/machine_coverage_test.go:83`).

### Out of scope

- Multi-file overlays (`tako.<env>.yaml` merged over `tako.yaml`, Kamal's file-per-destination layout) and `include:` of other files.
- Per-key merge directives such as `!reset`, `!override`, or list-append syntax; `null` deletion plus list replacement is the whole model.
- `x-` keys anywhere other than the document root (inside environments, services, servers).
- Inheritance for anything other than environments (servers, services across environments, builds).
- Writing `extends` back out: `tako config export`/`config pull` keep emitting a single flattened environment.
- Requiring `-e` when the default environment is ambiguous. That would break read-only commands for existing multi-environment configs.

## Capabilities

### New Capabilities
- `environment-config`: how environments in `tako.yaml`/`tako.json` are declared and resolved — inheritance through `extends`, abstract template environments, merge semantics, top-level `x-` extension keys, and how resolved environments and their provenance are reported by validation and `config explain`.

### Modified Capabilities
- None (no main specs exist yet).

## Impact

- `pkg/config`: `LoadConfig` (`types.go:1195-1294`) gains a pre-decode resolution step; `EnvironmentConfig` (`types.go:747-754`) gains `Extends`/`Abstract`; `GetEnvironment` (`types.go:851-864`) rejects abstract names; `ValidateConfig` (`validator.go:111-124`) wording for "no environments" accounts for abstract ones. New file for the resolver plus tests.
- `cmd/config.go`: `loadRawConfigForExplain` (`:141-170`) must use the same resolver (it strict-decodes today and would reject the new keys); explain output adds provenance.
- `cmd/validate.go` and `cmd/config_error.go`: error wording only; `config.ValidationWarnings` gains the unused-abstract warning.
- `pkg/config/types.go` `GetDefaultEnvironment` (`:828-847`) and new `ResolveDefaultEnvironment`; `cmd/root.go` `getEnvironmentName` (`:349-360`) stderr notice and `-e` help text (`:278`); all man pages that list the inherited `-e` option are regenerated.
- `schema/tako.schema.json`, `schema/tako_schema_test.go`, `docs/CONFIGURATION.md`, `man/` (regenerated), one example update.
- No takod change, no capability gate: resolution happens entirely in the CLI before any desired state is built, so takod only ever receives flattened environments.
- Sibling changes: `add-local-deploy-hooks` and any other change that adds environment-level keys inherit through the same generic merge with no extra work; `make-rolling-default-deploys` affects defaults that `config explain` prints but not provenance.
