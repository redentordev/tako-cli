## MODIFIED Requirements

### Requirement: Abstract template environments
An environment MAY declare `abstract: true`. An abstract environment SHALL be usable as an `extends` target, SHALL NOT be validated as a standalone deployable environment, SHALL NOT appear among resolved environments, and SHALL NOT be chosen as the default environment. `abstract` SHALL NOT be inherited. An abstract environment that declares a `preview:` block SHALL additionally be selectable with `-e` as the base of `--preview`, and SHALL NOT be reported as unused.

#### Scenario: Abstract base without servers
- **WHEN** environment `base` declares `abstract: true` and services but no `servers` or `serverSelector`, and `production` extends `base` and declares `servers: [a]`
- **THEN** the config validates and `production` deploys the services from `base` to `a`

#### Scenario: Abstract environment excluded from default
- **WHEN** the config declares abstract `base` and concrete `staging` only, and no `-e` flag is given
- **THEN** the default environment is `staging`

#### Scenario: Selecting an abstract environment
- **WHEN** a user runs any environment-scoped command with `-e base` and `base` is abstract, without `--preview`
- **THEN** the command fails with an error stating that `base` is abstract and naming the environments that extend it

#### Scenario: Abstract preview template selected for a preview
- **WHEN** abstract environment `preview` declares a `preview:` block and the user runs `tako deploy -e preview --preview pr-42`
- **THEN** the command deploys preview `pr-42` from base `preview`

#### Scenario: Only abstract environments
- **WHEN** every declared environment is abstract
- **THEN** loading fails with an error stating that at least one non-abstract environment must be configured

#### Scenario: Unused abstract environment warns
- **WHEN** environment `old-base` declares `abstract: true`, declares no `preview:` block, and no environment extends it
- **THEN** `tako validate` succeeds with a `warn` finding naming `old-base` and stating that nothing extends it
- **AND** the exit code is 0

#### Scenario: Preview template is not unused
- **WHEN** environment `preview` declares `abstract: true` and a `preview:` block, and no environment extends it
- **THEN** `tako validate` reports no unused-abstract finding for `preview`

## ADDED Requirements

### Requirement: The preview block is not inherited
An environment's `preview:` block SHALL apply only to the environment that declares it. It SHALL NOT be inherited through `extends`, and SHALL be absent from every resolved environment, so only declaring environments are preview bases.

#### Scenario: Child of a preview template
- **WHEN** abstract `preview` declares a `preview:` block and concrete `qa` declares `extends: preview`
- **THEN** `qa` is not a preview base and `tako deploy --preview pr-1 -e qa` fails with exit code 2 stating that `qa` declares no `preview:` block
