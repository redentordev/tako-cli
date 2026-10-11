# Spec Delta

## Purpose

Defines how private registry credentials are declared in `registries.<host>`, how short-lived tokens are minted on the deploying machine by named providers, and how credentials reach nodes without being persisted or printed.

## ADDED Requirements

### Requirement: Registry entries use either static credentials or a provider
A `registries.<host>` entry SHALL declare either `username` and `password` (an `${ENV_VAR}` reference) or `provider`. `provider` and `password` SHALL be mutually exclusive. `username` SHALL be accepted with `provider` only for `ghcr`.

#### Scenario: Provider without password is valid
- **WHEN** `tako.yaml` declares `registries: {123456789012.dkr.ecr.us-west-1.amazonaws.com: {provider: ecr}}`
- **THEN** config validation succeeds

#### Scenario: Provider with password is rejected
- **WHEN** an entry declares both `provider: ecr` and `password: ${ECR_PASSWORD}`
- **THEN** validation fails with `provider and password are mutually exclusive` naming the host

#### Scenario: Static credentials keep working
- **WHEN** an entry declares `username: ${USER}` and `password: ${TOKEN}` and no provider
- **THEN** validation and deploys behave exactly as before this change

#### Scenario: Unknown provider
- **WHEN** an entry declares `provider: quay`
- **THEN** validation fails listing the supported providers `ecr`, `gcr`, `artifact-registry`, `ghcr`

### Requirement: Provider must match the registry host
Tako SHALL reject a provider whose host pattern does not match the entry's host. For `ecr` the region SHALL be parsed from the host, and an explicit `region` that differs from it SHALL be rejected.

#### Scenario: ECR region from host
- **WHEN** the host is `123456789012.dkr.ecr.eu-central-1.amazonaws.com` with `provider: ecr` and no `region`
- **THEN** Tako runs `aws ecr get-login-password --region eu-central-1`

#### Scenario: Mismatched host
- **WHEN** the host is `ghcr.io` with `provider: ecr`
- **THEN** validation fails stating that `ecr` requires an ECR registry host

#### Scenario: Artifact Registry host
- **WHEN** the host is `us-docker.pkg.dev` with `provider: gcr`
- **THEN** validation succeeds and the credential username is `oauth2accesstoken`

### Requirement: Tokens are minted lazily on the deploying machine
Tako SHALL mint a provider token by running the provider CLI on the deploying machine only when an operation first needs a credential for that host (image pull, `tako run`, or a takod build), and in deploy preflight when the plan contains such a step. A missing CLI or a failing command SHALL fail the operation before any mutation that needs the credential, with a message naming the CLI and how to log in.

#### Scenario: Deploy pulls from ECR
- **WHEN** a service uses an image from an ECR host configured with `provider: ecr` and `tako deploy` runs
- **THEN** Tako runs the AWS CLI once and sends `Username: AWS` and the minted password to takod as a request-scoped registry auth

#### Scenario: No pull needed
- **WHEN** a deploy only transfers already-built images and performs no authenticated pull or build
- **THEN** no provider CLI is executed

#### Scenario: CLI missing
- **WHEN** `provider: gcr` is configured and `gcloud` is not on PATH
- **THEN** the deploy fails before mutating any node with an error naming `gcloud` and its install URL

### Requirement: GHCR token and username resolution
For `provider: ghcr`, Tako SHALL use `GITHUB_TOKEN`, then `GH_TOKEN`, then the output of `gh auth token`; the username SHALL be `username` if set, else `GITHUB_ACTOR`, else the login from `gh api user`.

#### Scenario: CI token
- **WHEN** `GITHUB_TOKEN` and `GITHUB_ACTOR` are set
- **THEN** no `gh` command runs and those values are used

### Requirement: Minted tokens are cached in memory and refreshed
Within one CLI process Tako SHALL reuse a minted token for the same host and provider options, and SHALL mint again when the token is older than the provider's refresh age (ECR 6 hours, GCR 45 minutes). Concurrent requests for the same key SHALL share one mint.

#### Scenario: Multi-service deploy
- **WHEN** three services pull from the same ECR host in one deploy
- **THEN** the AWS CLI runs once

### Requirement: Minted tokens are never persisted
Tako SHALL NOT write a minted token to disk on the deploying machine (including `.tako/`, local state and history) or to takod desired state, history, or events. On nodes the token SHALL exist only in the ephemeral Docker config takod creates for one docker call and deletes afterwards.

#### Scenario: State after deploy
- **WHEN** a deploy using `provider: ecr` completes
- **THEN** no file written by Tako on the deploying machine or on any node contains the token
