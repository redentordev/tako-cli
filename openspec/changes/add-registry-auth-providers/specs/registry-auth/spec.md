# Spec Delta

## Purpose

Defines:
- how private registry credentials are declared in `registries.<host>`;
- how named providers mint short-lived tokens on the deploying machine;
- which requests carry which credentials;
- how credentials reach nodes without being persisted or printed.

## ADDED Requirements

### Requirement: Registry entries use either static credentials or a provider
A `registries.<host>` entry SHALL declare either `username` and `password` (an `${ENV_VAR}` reference) or `provider`. `provider` and `password` SHALL be mutually exclusive. `username` SHALL be accepted with `provider` only for `ghcr`.

#### Scenario: Provider without password is valid
- **WHEN** `tako.yaml` declares `registries: {123456789012.dkr.ecr.us-west-1.amazonaws.com: {provider: ecr}}`
- **THEN** config validation succeeds

#### Scenario: Provider with password is rejected
- **WHEN** an entry declares both `provider: ecr` and `password: ${ECR_PASSWORD}`
- **THEN** validation fails with `provider and password are mutually exclusive`, naming the host

#### Scenario: Static credentials keep working
- **WHEN** an entry declares `username: ${USER}` and `password: ${TOKEN}` and no provider
- **THEN** validation and deploys behave exactly as before this change, including which requests carry the credential

#### Scenario: Unknown provider
- **WHEN** an entry declares `provider: quay`
- **THEN** validation fails, listing the supported providers `ecr`, `gcr`, `artifact-registry` and `ghcr`

### Requirement: Provider must match the registry host
Tako SHALL reject a provider whose host pattern does not match the entry's host. For `ecr`, the region SHALL be parsed from the host, and an explicit `region` that differs from it SHALL be rejected.

#### Scenario: ECR region from host
- **WHEN** the host is `123456789012.dkr.ecr.eu-central-1.amazonaws.com` with `provider: ecr` and no `region`
- **THEN** Tako runs `aws ecr get-login-password --region eu-central-1`

#### Scenario: Mismatched host
- **WHEN** the host is `ghcr.io` with `provider: ecr`
- **THEN** validation fails, stating that `ecr` requires an ECR registry host

#### Scenario: Artifact Registry host
- **WHEN** the host is `us-docker.pkg.dev` with `provider: gcr`
- **THEN** validation succeeds, and the credential username is `oauth2accesstoken`

### Requirement: Provider credentials are attached only where they can be used
An image pull SHALL carry a provider-minted credential only for the provider entry whose host equals the image's registry host. A takod image build SHALL carry a credential for every provider entry. Static entries SHALL be attached as before this change.

#### Scenario: Pull from an unrelated registry
- **WHEN** a config declares an ECR provider entry and a service pulls `nginx:1.27`
- **THEN** no provider CLI runs for that pull, and no ECR credential is attached

#### Scenario: Build with a private base image
- **WHEN** a service builds on takod and the config declares `ecr` and `ghcr` provider entries
- **THEN** the build request carries credentials for both hosts

### Requirement: Tokens are minted before mutation and only when needed
Before its first mutation, every operation that can attach registry credentials SHALL mint each provider credential it will attach. These operations are deploy, rollback, project rollback, scale, promote, `run` and placement apply. An operation that will attach no provider credential SHALL NOT run a provider CLI.

#### Scenario: Deploy pulls from ECR
- **WHEN** a service uses an image from an ECR host configured with `provider: ecr` and `tako deploy` runs
- **THEN** Tako runs the AWS CLI once, before any node is changed
- **AND** it sends `Username: AWS` and the minted password to takod as a request-scoped registry auth

#### Scenario: No credential needed
- **WHEN** a deploy only transfers already-built images and performs no pull or takod build
- **THEN** no provider CLI is executed

### Requirement: Mint failures stop the operation before mutation
When a provider CLI is missing, its command fails, or its output is rejected, the operation SHALL fail with exit code 1 before mutating any node. The error SHALL name the CLI and how to install it or log in.

#### Scenario: CLI missing during rollback
- **WHEN** `provider: gcr` is configured for the image host of a rollback target, and `gcloud` is not on PATH
- **THEN** `tako rollback` exits 1 before mutating any node, with an error naming `gcloud` and its install URL

#### Scenario: Token outside the size limits
- **WHEN** a provider CLI prints fewer than 16 characters, or more than 4096 bytes
- **THEN** the operation fails before mutation with an error naming the provider and host, without echoing the output

### Requirement: GHCR token and username resolution
For `provider: ghcr`, Tako SHALL take the token from `GH_TOKEN`, then `GITHUB_TOKEN`, then the output of `gh auth token`. The username SHALL be `username` if set, else `GITHUB_ACTOR`, else the login from `gh api user`.

#### Scenario: CI token
- **WHEN** `GITHUB_TOKEN` and `GITHUB_ACTOR` are set and `GH_TOKEN` is not
- **THEN** no `gh` command runs, and those values are used

#### Scenario: Explicit override in CI
- **WHEN** both `GH_TOKEN` and `GITHUB_TOKEN` are set
- **THEN** the `GH_TOKEN` value is used

### Requirement: Minted tokens are cached in memory and refreshed
Within one CLI process, Tako SHALL reuse a minted token for the same host and provider options. It SHALL mint again when the token is older than the provider's refresh age: 6 hours for ECR, 45 minutes for GCR. Concurrent requests for the same key SHALL share one mint.

#### Scenario: Multi-service deploy
- **WHEN** three services pull from the same ECR host in one deploy
- **THEN** the AWS CLI runs once

#### Scenario: Long deploy
- **WHEN** a deploy is still pulling from a GCR host 50 minutes after the first mint
- **THEN** Tako mints a new token before the next pull

### Requirement: Minted tokens are never persisted
Tako SHALL NOT write a minted token to disk on the deploying machine, including `.tako/`, local state and history. It SHALL NOT write one to takod desired state, history or events. On nodes, the token SHALL exist only in the ephemeral Docker config that takod creates for one docker call and deletes afterwards.

#### Scenario: State after deploy
- **WHEN** a deploy using `provider: ecr` completes
- **THEN** no file written by Tako on the deploying machine or on any node contains the token
