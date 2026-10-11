## ADDED Requirements

### Requirement: Preview bases are declared with a preview block
An environment SHALL be usable as the base of a preview only when it declares a `preview:` block. The block MAY set `domain` (hostname template), `ttl` (duration or `none`, default `168h`), `max` (integer ≥ 1, default 5), `maxReplicas` (integer ≥ 1, default 1), `keepJobs` (boolean, default false), `env` and `services.<name>.env` (string maps). Unknown keys SHALL be rejected like any other unknown config key.

#### Scenario: Valid preview template
- **WHEN** abstract environment `preview` declares `extends: staging` and `preview: {domain: "{name}-{service}.preview.example.com", ttl: 72h}`
- **THEN** `tako validate` succeeds
- **AND** reports no unused-abstract warning for `preview`

#### Scenario: Unknown key rejected
- **WHEN** a `preview:` block contains `maxPreviews: 3`
- **THEN** loading fails with an unknown-field error naming the declaring environment

### Requirement: Preview template placeholders
`preview.domain` MAY use the placeholders `{name}`, `{service}` and `{domain}`. Values in `preview.env` and `preview.services.<name>.env` MAY use `{name}`, `{host:<service>}` and `{url:<service>}`. Any other placeholder, or a `{host:…}`/`{url:…}` naming a service that is not proxied in the resolved base, SHALL be a validation error naming the environment, key and placeholder.

#### Scenario: Unknown placeholder rejected
- **WHEN** a `preview.env` value contains `{branch}`
- **THEN** `tako validate` fails with exit code 2 and an error naming the environment, the key and `{branch}`

#### Scenario: Placeholder names a missing service
- **WHEN** a `preview.env` value contains `{url:admin}` and the resolved base has no proxied service `admin`
- **THEN** `tako validate` fails with an error naming `admin`

### Requirement: Deploying a preview
`tako deploy --preview <name>` SHALL deploy a new or existing preview of a base environment, using the normal deploy pipeline on the preview's environment identity. Deploying an existing preview SHALL update it in place with the normal rollout strategy and refresh its expiry.

#### Scenario: First preview deploy
- **WHEN** the only preview base is `preview` and the operator runs `tako deploy --preview pr-42 --yes`
- **THEN** Tako deploys environment `preview-pr-42` resolved as `extends: preview`
- **AND** the result document has `preview.created: true` and `preview.expiresAt` 168 hours after the deploy

#### Scenario: Redeploy refreshes expiry
- **WHEN** preview `pr-42` exists and the operator deploys `--preview pr-42` again
- **THEN** the existing preview is updated, `preview.created` is false, and `expiresAt` moves forward

### Requirement: Preview base selection
The base SHALL be the `-e` environment when given (an abstract environment is allowed), otherwise the only environment declaring `preview:`. The command SHALL fail with exit code 2 before any remote call when no environment declares `preview:`, when several do and `-e` is absent, when the `-e` environment declares no `preview:` block, or when the preview already exists with a different base.

#### Scenario: Ambiguous base
- **WHEN** environments `preview` and `preview-mobile` both declare `preview:` and no `-e` is given
- **THEN** the deploy fails with exit code 2 listing both candidates and asking for `-e`

#### Scenario: Base mismatch
- **WHEN** preview `pr-42` was created from base `preview` and the operator runs `tako deploy --preview pr-42 -e preview-mobile`
- **THEN** the deploy fails with exit code 2 stating that `pr-42` was created from `preview`

#### Scenario: No preview base
- **WHEN** no environment declares `preview:` and the operator runs `tako deploy --preview pr-1`
- **THEN** the deploy fails with exit code 2 pointing to the preview environments documentation

### Requirement: Preview name normalization
The preview name SHALL be lowercased, each run of characters outside `[a-z0-9]` SHALL become `-`, and leading and trailing `-` SHALL be trimmed. A result longer than 40 characters SHALL be cut to 33 characters plus `-` and the first 6 hex characters of the SHA-256 of the original input. An empty result SHALL fail with exit code 2. When normalization changes the input, the CLI SHALL print the normalized name on stderr.

#### Scenario: Branch name normalized
- **WHEN** the operator runs `tako deploy --preview "Feature/Login_V2"`
- **THEN** the preview name is `feature-login-v2`
- **AND** stderr notes the normalization while stdout in `--output json` mode contains only the result document

#### Scenario: Empty name
- **WHEN** the operator runs `tako deploy --preview "///"`
- **THEN** the command fails with exit code 2

### Requirement: Preview environment identity
A preview's environment identity SHALL be `preview-<normalized name>` and SHALL NOT depend on the base. Loading SHALL fail with exit code 2 when a declared environment has the same name.

#### Scenario: Identity collision
- **WHEN** the config declares an environment named `preview-pr-42` and the operator deploys `--preview pr-42`
- **THEN** the command fails with exit code 2 naming the conflicting environment

### Requirement: Preview data isolation
Every preview SHALL use volumes scoped to its own environment identity, so persistent services start with empty data and never mount another environment's volumes. Instantiating a preview SHALL fail with exit code 2, before any remote call, when a remaining service mounts a volume whose top-level definition sets `name` or `external: true`, naming the service and volume.

#### Scenario: Database starts empty
- **WHEN** the base defines persistent service `mongo` with volume `mongo_data`
- **THEN** the preview's `mongo` mounts the preview-scoped volume for `mongo_data`, distinct from the base's volume

#### Scenario: Shared volume rejected
- **WHEN** a preview service mounts top-level volume `uploads` declared with `name: shared_uploads`
- **THEN** the deploy fails with exit code 2 naming the service and `uploads`

### Requirement: Preview host-level isolation
Instantiating a preview SHALL fail with exit code 2, before any remote call, when a remaining service publishes host `ports` or declares `imports`. The error SHALL name the service and tell the operator to remove or override it in the preview template.

#### Scenario: Host ports rejected
- **WHEN** a service in the resolved preview declares `ports: ["5432:5432"]`
- **THEN** the deploy fails with exit code 2 naming the service and suggesting `ports: null` or removing the service in the template

### Requirement: Preview schedules and alerts are disabled
Tako SHALL remove `backup` and `monitoring` from every preview service and SHALL drop `kind: job` services unless the preview block sets `keepJobs: true`. `kind: run` services SHALL be kept.

#### Scenario: Backups and jobs stripped
- **WHEN** the base declares a service with a `backup` schedule and a `kind: job` service, and `keepJobs` is not set
- **THEN** the preview deploys no backup schedule and no job

### Requirement: Preview replica cap
Every long-running service of a preview SHALL run at most `maxReplicas` replicas; services with placement strategy `global` SHALL keep their placement and are not capped.

#### Scenario: Replicas capped
- **WHEN** the base runs `web` with `replicas: 3` and the preview block does not set `maxReplicas`
- **THEN** the preview runs `web` with 1 replica

### Requirement: Preview hosts
For each proxied service, a preview SHALL serve exactly one host rendered from `preview.domain` (default `{name}.{domain}`), where `{domain}` is the service's resolved primary host. Additional `domains`, `redirectFrom`, `dynamicDomains` and `cdn` SHALL be cleared; all other route settings SHALL be kept. Post-deploy domain checks SHALL behave as for any environment.

#### Scenario: Default host template
- **WHEN** the base serves `web` at `app.example.com` and the preview block sets no `domain`
- **THEN** preview `pr-42` serves `web` only at `pr-42.app.example.com` with its own certificate

#### Scenario: Shared template domain
- **WHEN** `preview.domain` is `{name}-{service}.preview.example.com` and services `web` and `admin` are proxied
- **THEN** preview `pr-42` serves `pr-42-web.preview.example.com` and `pr-42-admin.preview.example.com`

#### Scenario: Missing wildcard DNS
- **WHEN** no DNS record resolves the preview host and `--strict-domains` is not given
- **THEN** the deploy succeeds and the domain check reports the host as pending

### Requirement: Preview host validation
Instantiation SHALL fail with exit code 2 when a rendered host is not a valid hostname, is used twice in the preview, or equals a host served by any declared environment of the project.

#### Scenario: Host collides with production
- **WHEN** `preview.domain` is `{domain}` so the preview would serve `app.example.com`, which production serves
- **THEN** the deploy fails with exit code 2 naming the host and the environment that serves it

### Requirement: Preview environment variables
Every preview container SHALL receive `TAKO_PREVIEW=true` and `TAKO_PREVIEW_NAME`, and every proxied preview container SHALL receive `TAKO_PREVIEW_URL` with its own URL. `preview.env` SHALL be merged into every service's env and `preview.services.<name>.env` into that service's env, with placeholders expanded, overriding values from the template.

#### Scenario: Absolute URL override
- **WHEN** `preview.services.admin.env` sets `BETTER_AUTH_URL: "{url:admin}"` and the preview host of `admin` is `pr-42.admin.example.com`
- **THEN** the preview `admin` container has `BETTER_AUTH_URL=https://pr-42.admin.example.com`

### Requirement: Preview secrets
A preview SHALL load local secret files for its base environment name (`.tako/secrets`, then `.tako/secrets.<base>`) and SHALL resolve the `secretSources` of its resolved environment. Preview names, identities and hosts SHALL be registered as public values with the redactor.

#### Scenario: Template secrets file
- **WHEN** the base is abstract environment `preview` and `.tako/secrets.preview` exists
- **THEN** the preview's services receive secrets from `.tako/secrets` overridden by `.tako/secrets.preview`, and never read `.tako/secrets.staging`

### Requirement: Preview records
After a successful preview deploy, Tako SHALL write a `PreviewRecord` (project, environment, name, base, servers, hosts, createdAt, updatedAt, expiresAt, ttl, git source, performer) to the replicated state of every target server, under the same lease as the deploy. A failed rollout SHALL NOT create or refresh a record.

#### Scenario: Record written
- **WHEN** `tako deploy --preview pr-42` succeeds on servers `app-1` and `app-2`
- **THEN** both servers hold a `PreviewRecord` for `preview-pr-42` with the same `expiresAt`

### Requirement: Listing previews
`tako preview list` SHALL read records from every server named in the config, merge them by environment keeping the newest `updatedAt`, mark records past `expiresAt` as `expired`, and exit 6 when any server could not be read.

#### Scenario: List previews
- **WHEN** previews `pr-41` (expired) and `pr-42` exist
- **THEN** `tako preview list --output json` returns a `PreviewList` with both items, `pr-41` having `status: expired`

### Requirement: Destroying a preview
`tako preview destroy <name>` SHALL remove the preview's containers, proxy routes, networks, Docker volumes, replicated state and record on every server named in its record or in the resolved template. It SHALL decommission every service found in either the current template or the preview's desired state, and SHALL require `--yes` in non-interactive and machine modes.

#### Scenario: PR closed
- **WHEN** CI runs `tako preview destroy pr-42 --yes --output json` after a pull request closes
- **THEN** the result has `status: destroyed` and no container, network, volume or proxy route of `preview-pr-42` remains

#### Scenario: Service removed from template
- **WHEN** preview `pr-42` was deployed with service `worker` and the template no longer defines `worker`
- **THEN** destroy still removes the `worker` containers

### Requirement: Preview destroy is idempotent and retryable
Destroying a preview that has no record SHALL succeed with status `not_found`. A partially failed destroy SHALL leave the record in place so the same command can be retried.

#### Scenario: Already gone
- **WHEN** `tako preview destroy pr-42 --yes` runs after the preview was destroyed
- **THEN** it exits 0 with `status: not_found`

### Requirement: Pruning expired previews
`tako preview prune` SHALL destroy every preview whose `expiresAt` has passed, each under its own lease, skipping and reporting previews whose lease is held. It SHALL exit 6 when any expired preview could not be destroyed. `--dry-run` SHALL report without mutating. A `ttl` of `none` SHALL never expire.

#### Scenario: Scheduled prune
- **WHEN** preview `pr-41` expired yesterday and `pr-42` expires tomorrow
- **THEN** `tako preview prune --yes` destroys `pr-41` only and reports it in `destroyed`

### Requirement: Opportunistic prune and TTL override on deploy
`tako deploy --preview` SHALL run the prune for the project before checking limits unless `--no-prune` is given. Prune failures during deploy SHALL be warnings that never change the deploy outcome. `--ttl <duration|none>` SHALL override the template's `ttl` for that preview.

#### Scenario: Opportunistic prune
- **WHEN** preview `pr-41` has expired and the operator deploys `--preview pr-50`
- **THEN** `pr-41` is destroyed before `pr-50` is deployed and a `preview.pruned` event is emitted

### Requirement: Preview limit per base
Deploying a new preview SHALL fail with exit code 2 when the base already has `max` previews after pruning, listing the existing previews and their expiry. Redeploying an existing preview SHALL NOT count against the limit.

#### Scenario: Limit reached
- **WHEN** `max` is 2, previews `pr-1` and `pr-2` of the same base are active, and the operator deploys `--preview pr-3`
- **THEN** the deploy fails with exit code 2 naming `pr-1` and `pr-2`

### Requirement: Commands that accept --preview
`--preview <name>` SHALL be accepted by `deploy`, `ps`, `logs`, `exec`, `history`, `rollback` and `config explain`, which SHALL operate on the preview's environment. Except for `deploy`, these commands SHALL fail with exit code 2 when no record exists for the preview. Every other command SHALL reject `--preview` with exit code 2.

#### Scenario: Logs of a preview
- **WHEN** the operator runs `tako logs --preview pr-42 web`
- **THEN** logs stream from the `web` containers of `preview-pr-42`

#### Scenario: Unsupported command
- **WHEN** the operator runs `tako backup --preview pr-42`
- **THEN** the command fails with exit code 2 stating that `--preview` is not supported by `tako backup`

### Requirement: Preview deploy result
`DeployResult` SHALL carry an additive `preview` object (`name`, `environment`, `base`, `created`, `expiresAt`, `ttl`, `hosts`) for preview deploys and omit it otherwise.

#### Scenario: CI reads the preview URL
- **WHEN** CI runs `tako deploy --preview pr-42 --yes --output json`
- **THEN** stdout is one `DeployResult` whose `preview.hosts.web` is the preview host of `web`

### Requirement: Preview command machine contract
`preview list`, `preview destroy` and `preview prune` SHALL honor the full machine contract with result documents `PreviewList`, `PreviewDestroyResult` and `PreviewPruneResult`, and SHALL emit `preview.destroyed` and `preview.pruned` events.

#### Scenario: Prune events
- **WHEN** `tako preview prune --yes --events ndjson` destroys two previews
- **THEN** stdout contains two `preview.pruned` events followed by the terminal result event carrying a `PreviewPruneResult`

### Requirement: Preview in the deploy summary
When the deploy summary defined by `add-deploy-summary` is present, a preview deploy SHALL report `environment.kind: preview` with the preview name, base and expiry, and `kind: preview` on its URLs. The undo command of a preview's first deploy SHALL be `tako preview destroy <name>`.

#### Scenario: Summary of a new preview
- **WHEN** the first deploy of preview `pr-42` succeeds
- **THEN** the summary shows the preview name and expiry, its URLs with `kind: preview`, and the undo command `tako preview destroy pr-42`

### Requirement: Preview capability gate
Preview deploy, destroy and prune SHALL require the takod capability `preview.lifecycle-v1` on every target server, and `preview list` and the record reads of other `--preview` commands SHALL require it on every queried server. When a server lacks it, the command SHALL fail before any mutation with the standard missing-capability message.

#### Scenario: Old node
- **WHEN** a target server runs a takod without `preview.lifecycle-v1`
- **THEN** `tako deploy --preview pr-42` fails before building or deploying, naming the capability and telling the operator to upgrade the node agent

### Requirement: Preview volume removal is guarded on the node
takod SHALL refuse a preview volume removal request for an environment that has no preview record or whose name does not start with `preview-`, and SHALL remove only volumes labeled with exactly that project and environment.

#### Scenario: Volume removal guarded
- **WHEN** a cleanup request asks to remove preview volumes for environment `staging`
- **THEN** takod rejects the request and removes nothing

### Requirement: Environment network cleanup matches exact names
takod cleanup scoped to an environment SHALL remove only that environment's own runtime networks (its project network and its service export networks) and SHALL NOT remove networks of another environment whose name shares a prefix.

#### Scenario: Prefix-sharing environments
- **WHEN** environments `prod` and `prod-eu` of the same project both have networks on a node and `prod` is destroyed
- **THEN** every `prod-eu` network remains
