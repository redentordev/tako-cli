# environment-config Specification

## Purpose
Defines how deployment environments in tako.yaml/tako.json are declared and resolved, including inheritance between environments, abstract template environments, top-level extension keys, and how resolved values and their origin are reported.

## Requirements

### Requirement: Environment inheritance through extends
An environment MAY declare `extends: <name>` naming another environment in the same config. The CLI SHALL resolve such an environment by merging its own declared keys over the fully resolved parent environment before any validation or deployment uses it. The `extends` key itself SHALL NOT be inherited.

#### Scenario: Child inherits parent services
- **WHEN** `production` declares service `web` with `image: app:1`, `port: 3000`, and `staging` declares only `extends: production` and `servers: [stage-1]`
- **THEN** the resolved `staging` environment deploys service `web` with `image: app:1` and `port: 3000` to `stage-1`
- **AND** the resolved `production` environment is unchanged

#### Scenario: Config without extends is unaffected
- **WHEN** a config contains no `extends`, `abstract`, or top-level `x-` keys
- **THEN** it loads, validates, and resolves exactly as it did before this change, including error messages and reported line numbers

### Requirement: Mapping values merge recursively
When both the parent and the child define a mapping at the same path, the resolved value SHALL contain every key of both, with the child's value merged recursively for keys present in both. This applies to `services` (merged by service name) and to nested mappings such as `serverSelector`, service `env`, and service `proxy`.

#### Scenario: Service field override
- **WHEN** the parent defines service `web` with `replicas: 1` and `env: {LOG_LEVEL: info, REGION: eu}`, and the child defines service `web` with `replicas: 3` and `env: {LOG_LEVEL: debug}`
- **THEN** the resolved child service `web` has `replicas: 3` and `env: {LOG_LEVEL: debug, REGION: eu}`
- **AND** every other field of `web` comes from the parent

#### Scenario: Child adds a service
- **WHEN** the child defines service `debug-ui` that the parent does not define
- **THEN** the resolved child contains the parent's services plus `debug-ui`

### Requirement: Scalars and sequences replace
When the child defines a scalar or a sequence at a path, the resolved value SHALL be exactly the child's value; sequences SHALL NOT be concatenated or merged element-wise. When the parent and child values at a path are of different kinds (scalar, sequence, mapping), the child's value SHALL replace the parent's.

#### Scenario: Server list replaced
- **WHEN** the parent declares `servers: [a, b]` and the child declares `servers: [c]`
- **THEN** the resolved child has `servers: [c]`

#### Scenario: Service volumes replaced
- **WHEN** the parent service declares `volumes: [data:/var/lib/app]` and the child service declares `volumes: [scratch:/tmp/app]`
- **THEN** the resolved child service has only `scratch:/tmp/app`

#### Scenario: Build shape changes
- **WHEN** the parent service declares `build: .` and the child service declares `build: {context: ., target: prod}`
- **THEN** the resolved child service uses the structured build with context `.` and target `prod`

### Requirement: Explicit null removes an inherited key
In an environment that declares `extends`, a key whose value is an explicit `null` SHALL remove that key from the resolved environment, including an inherited service. A `null` in an environment without `extends` SHALL keep its current meaning.

#### Scenario: Remove an inherited service
- **WHEN** the parent defines services `web` and `worker`, and the child declares `services: {worker: null}`
- **THEN** the resolved child contains only service `web`

#### Scenario: Switch from servers to serverSelector
- **WHEN** the parent declares `servers: [a]` and the child declares `servers: null` and `serverSelector: {labels: {tier: edge}}`
- **THEN** the resolved child has no `servers` and selects servers by the `tier: edge` label

### Requirement: Environment secret sources follow the replace rule
An environment's `secretSources` list SHALL be inherited when the child does not declare it and SHALL be replaced entirely when the child declares it. Top-level `secretSources` SHALL continue to apply first to every resolved environment, followed by the resolved environment's own list.

#### Scenario: Inherited secret sources
- **WHEN** top-level `secretSources` contains source T, the parent environment declares sources [P], and the child declares none
- **THEN** secrets for the child are resolved from T then P, with later sources overriding earlier ones key by key

#### Scenario: Child replaces secret sources
- **WHEN** the parent declares sources [P] and the child declares sources [C]
- **THEN** secrets for the child are resolved from the top-level sources then C only

### Requirement: Extends chains, cycles, and unknown targets
Inheritance chains SHALL be resolved from the root ancestor down, so a value set closer to the child wins. A chain SHALL contain at most 8 `extends` hops. Config loading MUST fail with an error naming the offending environments when `extends` names an unknown environment, forms a cycle (including self-reference), or exceeds the maximum depth.

#### Scenario: Three-level chain
- **WHEN** `c` extends `b`, `b` extends `a`, `a` sets `replicas: 1`, `b` sets `replicas: 2`, and `c` does not set `replicas`
- **THEN** the resolved `c` has `replicas: 2`

#### Scenario: Cycle rejected
- **WHEN** `a` extends `b` and `b` extends `a`
- **THEN** loading fails with an error that names the cycle `a -> b -> a`

#### Scenario: Unknown parent rejected
- **WHEN** `staging` declares `extends: prod` and no environment named `prod` exists
- **THEN** loading fails with an error stating that environment `staging` extends unknown environment `prod`

#### Scenario: Chain too deep
- **WHEN** a chain of `extends` contains more than 8 hops
- **THEN** loading fails with an error naming the chain and the maximum depth

### Requirement: Abstract template environments
An environment MAY declare `abstract: true`. An abstract environment SHALL be usable as an `extends` target, SHALL NOT be validated as a standalone deployable environment, SHALL NOT appear among resolved environments, and SHALL NOT be chosen as the default environment. `abstract` SHALL NOT be inherited.

#### Scenario: Abstract base without servers
- **WHEN** environment `base` declares `abstract: true` and services but no `servers` or `serverSelector`, and `production` extends `base` and declares `servers: [a]`
- **THEN** the config validates and `production` deploys the services from `base` to `a`

#### Scenario: Abstract environment excluded from default
- **WHEN** the config declares abstract `base` and concrete `staging` only, and no `-e` flag is given
- **THEN** the default environment is `staging`

#### Scenario: Selecting an abstract environment
- **WHEN** a user runs any environment-scoped command with `-e base` and `base` is abstract
- **THEN** the command fails with an error stating that `base` is abstract and naming the environments that extend it

#### Scenario: Only abstract environments
- **WHEN** every declared environment is abstract
- **THEN** loading fails with an error stating that at least one non-abstract environment must be configured

#### Scenario: Unused abstract environment warns
- **WHEN** environment `old-base` declares `abstract: true` and no environment extends it
- **THEN** `tako validate` succeeds with a `warn` finding naming `old-base` and stating that nothing extends it
- **AND** the exit code is 0

### Requirement: Default environment selection is deterministic
When no `-e/--env` is given, Tako SHALL select from the non-abstract environments: the only one if there is exactly one; otherwise `production` if it exists; otherwise the first name in ascending lexical order. The same config SHALL always yield the same default, and Tako SHALL NOT fail because the default is ambiguous.

#### Scenario: Alphabetical fallback is stable
- **WHEN** a config declares environments `staging` and `dev`, no `production`, and the user runs `tako validate` without `-e` repeatedly
- **THEN** every run selects `dev`

#### Scenario: Production preferred
- **WHEN** a config declares `production`, `staging` and `dev`, and no `-e` is given
- **THEN** the default environment is `production` and no notice is printed

### Requirement: Implicit alphabetical default is announced on stderr
When the default environment was chosen by lexical order, Tako SHALL print one notice to stderr naming the chosen environment and recommending `-e`. The notice SHALL NOT be written to stdout.

#### Scenario: Notice names the chosen environment
- **WHEN** a config declares `staging` and `dev` only and the user runs `tako validate` without `-e`
- **THEN** stderr contains one line stating that `dev` was used because there is no `-e` and no `production`

#### Scenario: Notice does not affect machine output
- **WHEN** a config declares `staging` and `dev` only and the user runs `tako status --output json` without `-e`
- **THEN** stdout contains only the result document for `dev`, and the notice appears on stderr

### Requirement: Top-level extension keys
Top-level keys whose name starts with `x-` SHALL be accepted and ignored in both YAML and JSON configs. Their content SHALL NOT be validated against the config schema, and YAML anchors defined inside them SHALL be usable anywhere in the document. Unknown keys without the `x-` prefix SHALL remain errors, and `x-` keys inside `environments`, services, or servers SHALL remain errors.

#### Scenario: Shared fragment via anchor
- **WHEN** a YAML config declares `x-web: &web {image: app:1, port: 3000}` at the top level and two environments use `web: *web` under `services`
- **THEN** the config validates and both environments deploy `web` with `image: app:1` and `port: 3000`

#### Scenario: JSON extension key
- **WHEN** a JSON config contains a top-level key `"x-notes": {"owner": "ops"}`
- **THEN** the config validates and the key has no effect

#### Scenario: Non-prefixed unknown key still rejected
- **WHEN** a config contains a top-level key `extra: true`
- **THEN** loading fails with the same unknown-field error as today

#### Scenario: Nested extension key rejected
- **WHEN** a service inside an environment declares a key `x-notes: hi`
- **THEN** loading fails with an unknown-field error for `x-notes`

### Requirement: Errors identify the declaring environment
When an inheriting environment fails to load or validate, the error SHALL identify the resolved environment and the extends chain. When the failure is an unknown or mistyped field, the error SHALL name the environment that declared that field rather than an environment that inherited it.

#### Scenario: Typo in a parent environment
- **WHEN** `base` declares field `replcas: 2` in service `web` and `production` extends `base`
- **THEN** loading fails with an error that names environment `base` and the unknown field `replcas`
- **AND** the error does not attribute the field to `production`

#### Scenario: Semantic error in a resolved child
- **WHEN** `staging` extends `production`, inherits a service whose `proxy.domain` is fine, but declares `servers: [missing]` where `missing` is not a configured server
- **THEN** the error names environment `staging`, the unknown server, and the chain `staging -> production`

### Requirement: config explain reports inheritance provenance
`tako config explain` SHALL print the extends chain of the selected environment and, for each environment-scoped value it prints, the declared environment that supplied the value. It SHALL remain a human-only command and SHALL NOT print secret values.

#### Scenario: Value from a parent
- **WHEN** `staging` extends `production`, `production` sets service `web` `replicas: 2`, and the user runs `tako config explain -e staging`
- **THEN** the output shows `Environment: staging (extends production)` and the `replicas: 2` line indicates it came from `production`

#### Scenario: Value set by the selected environment
- **WHEN** `staging` sets `replicas: 1` for `web` and the user runs `tako config explain -e staging`
- **THEN** the `replicas: 1` line indicates it came from `staging`

### Requirement: Deployed state uses the resolved environment
Deployments, desired state, deployment history, and `tako config export`/`config pull` output SHALL contain only the resolved, flattened environment. `extends` and `abstract` SHALL NOT be sent to nodes or written to exported configs.

#### Scenario: Export after deploying an inheriting environment
- **WHEN** `staging` extends `production`, `staging` is deployed, and the user runs `tako config export` for `staging`
- **THEN** the exported config contains a single `staging` environment with the resolved services and no `extends` key
