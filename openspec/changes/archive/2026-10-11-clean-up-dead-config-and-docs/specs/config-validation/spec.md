# Spec Delta

## Purpose

Defines how Tako treats configuration keys that are accepted but have no effect: warn for a release, then reject with a named replacement. Also defines how the published JSON schema is kept consistent with the keys the loader actually accepts.

## ADDED Requirements

### Requirement: Deprecated no-op keys produce a named-replacement warning
The system SHALL emit exactly one warning per deprecated key that is present in a config. The keys are `deployment.strategy`, `deployment.parallel`, `deployment.cache` and `environments.<env>.labels`. Each warning MUST name the key path, state that the key has no effect, name the replacement or the action to take, and name the release in which the key becomes an error.

#### Scenario: Parallel and cache keys present
- **WHEN** a config sets `deployment.strategy: parallel`, `deployment.parallel.maxConcurrentBuilds: 4` and `deployment.cache.enabled: true`
- **THEN** `tako validate` exits 0 and reports three warnings, with fields `deployment.strategy`, `deployment.parallel` and `deployment.cache`
- **AND** each message says the key has no effect and that it should be removed
- **AND** the `deployment.strategy` and `deployment.parallel` messages point to service `dependsOn` for ordering and to `services.<name>.deploy.strategy` for rollout behavior

#### Scenario: Warning names the removal release
- **WHEN** the `v0.14.0` release validates a config with `deployment.cache`
- **THEN** the warning message states that the key will be rejected starting in `v0.15.0`

#### Scenario: Cache key warning names the actual cache controls
- **WHEN** a config sets `deployment.cache`
- **THEN** the warning names `tako cleanup --docker-cache` and the takod `--build-cache-keep-storage` setting as the supported build-cache controls

#### Scenario: Environment labels present
- **WHEN** `environments.production.labels` is set
- **THEN** the warning field is `environments.production.labels`, and the message points to `servers.<name>.labels` combined with `serverSelector` or placement constraints

#### Scenario: Build strategy is not deprecated
- **WHEN** a config sets only `deployment.build.strategy: auto`
- **THEN** no deprecation warning is emitted

### Requirement: Deprecation warnings use the existing finding contract
Deprecation warnings SHALL appear in the `tako validate` result document as findings with `severity` `warn` and `field` set to the deprecated key path. They SHALL also be printed to stderr by `tako validate` (unless `--quiet` is set) and by `tako deploy`. They MUST NOT change the exit code, and the result document MUST NOT gain new fields.

#### Scenario: JSON output
- **WHEN** `tako validate --output json` runs on a config with `deployment.cache`
- **THEN** the result has `valid: true` and contains a finding `{severity: "warn", field: "deployment.cache", ...}`, and the process exits 0

#### Scenario: Deploy surfaces the warning
- **WHEN** `tako deploy` runs with a deprecated key present
- **THEN** stderr contains `Warning:` followed by the deprecation message before any server is contacted

### Requirement: Deprecated keys are rejected in the following minor release
Starting with the first minor release after the one that introduced the warnings (warnings in `v0.14.0`, rejection from `v0.15.0`), a config containing any deprecated key SHALL fail validation with exit code 2. The error MUST name the key path and the same replacement guidance as the warning. It MUST NOT be a generic unknown-field parse error.

#### Scenario: Rejection names the replacement
- **WHEN** a config with `deployment.parallel` is validated by a release in the rejection phase
- **THEN** validation fails, and the message contains `deployment.parallel` and the removal guidance

### Requirement: JSON schema covers every accepted config key
The published JSON schema SHALL declare a property for every key the strict loader accepts at the top level, server, environment, service and nested object levels, except for keys listed in a reviewed allowlist. Each allowlist entry MUST record a reason. Deprecated keys MUST stay in the schema, marked `deprecated: true`, with a description naming the replacement.

#### Scenario: Previously missing keys
- **WHEN** the schema is inspected
- **THEN** service `dockerfile` and `monitoring` and environment `serverSelector` are declared

#### Scenario: Drift is detected
- **WHEN** a new yaml-tagged field is added to a config type without a matching schema property or allowlist entry
- **THEN** the repository test suite fails and names the config path that is missing

#### Scenario: Stale schema property is detected
- **WHEN** the schema declares a property that no config type accepts and that has no allowlist entry
- **THEN** the repository test suite fails and names the schema path
