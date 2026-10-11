# project-init Specification

## Purpose
Defines what `tako init` writes. Every generated config and every optional example it shows must be a valid, current Tako configuration, so a new user's first file teaches only real keys.

## Requirements

### Requirement: Generated configs pass validation
Every config variant that `tako init` can generate SHALL pass `tako validate` with zero errors and zero deprecation warnings, once its placeholder environment variables are set. The variants are YAML and `--json`, each with the default project name and with a user-supplied valid name.

#### Scenario: YAML default
- **WHEN** `tako init` runs in an empty directory and `TAKO_PRODUCTION_HOST`, `TAKO_SSH_KEY` and `LETSENCRYPT_EMAIL` are set
- **THEN** `tako validate` on the generated `tako.yaml` exits 0 with no `warn` findings

#### Scenario: JSON custom name
- **WHEN** `tako init shop-api --json` runs and the same variables are set
- **THEN** `tako validate` on the generated `tako.json` exits 0 with no `warn` findings, and the project name is `shop-api`

### Requirement: Init rejects project names validation would reject
`tako init <name>` SHALL refuse to write any file when `<name>` fails the project-name rule enforced by config validation. The error MUST state the rule.

#### Scenario: Invalid name
- **WHEN** `tako init My_App` runs
- **THEN** the command fails, states the lowercase-letter/digit/hyphen rule, and creates no `tako.yaml`, `tako.json`, `.env.example` or `.gitignore`

### Requirement: Commented examples show only real, valid keys
Every commented optional example in the generated YAML SHALL use only keys the loader accepts. Uncommenting an example and merging it into the generated base config MUST produce a config that passes validation with no deprecation warnings.

#### Scenario: Registry example
- **WHEN** the generated YAML is inspected
- **THEN** the private-registry example is a `registries:` map keyed by registry host, with `username` and `password` given as `${ENV_VAR}` references
- **AND** no `registry:` or `url:` key appears

#### Scenario: No dead or nonexistent keys
- **WHEN** the generated YAML is inspected
- **THEN** it contains no `deployment.strategy`, `deployment.parallel` or `deployment.cache` example, and no `proxy.port` line

#### Scenario: Each optional example validates
- **WHEN** each commented optional example is uncommented and merged into the generated base config
- **THEN** the merged config passes validation

### Requirement: Init describes redirects accurately
Generated comments describing `proxy.redirectFrom` SHALL describe the redirect as a 308 permanent redirect that preserves path and method.

#### Scenario: Redirect comment
- **WHEN** the generated YAML is inspected
- **THEN** the `redirectFrom` comment says 308 and does not say 301
