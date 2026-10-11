## ADDED Requirements

### Requirement: Redaction markers name their source
Every mask produced by the engine redactor SHALL be rendered as `[REDACTED:<label>]`, where `<label>` matches `[A-Za-z0-9_\-@/:.]{1,96}` and is a config-path-like name of the value's origin. A label SHALL begin with one of these kinds:
- `secrets.`
- `env.`
- `envFile.`
- `servers.`
- `registries.`
- `environments.`
- `services.`
- `notifications.`
- `hooks.`
- `run.`
- `export.`
- `certs.`
- `proxy.`
- `pattern.`

For key/value pattern matches the key SHALL be kept, followed by the marker.

#### Scenario: Aliased secret is labeled by its secret key
- **WHEN** service `web` declares `secrets: ["API_KEY:STRIPE_KEY"]` and a container log line contains the value of `STRIPE_KEY`
- **THEN** the `log.line` event shows `[REDACTED:secrets.STRIPE_KEY]` in both `message` and `data.data`

#### Scenario: Misfiring env value is self-explaining
- **WHEN** a service sets `env: {SESSION_SIGNING_TOKEN: "site"}` and deploy output contains the word `site`
- **THEN** each masked occurrence reads `[REDACTED:env.SESSION_SIGNING_TOKEN]`

#### Scenario: Pattern match is labeled
- **WHEN** output contains a JWT that was not registered
- **THEN** it is replaced with `[REDACTED:pattern.jwt]`

#### Scenario: Assignment keeps its key
- **WHEN** output contains `password=hunter2hunter2`
- **THEN** it reads `password=[REDACTED:pattern.assignment]`

### Requirement: Registered values are fully masked under one deterministic label
A registered value SHALL be fully replaced by its marker, with no characters of the value preserved. When a value is registered under several labels, the rendered label SHALL be chosen by kind precedence with `secrets.` highest, and lexicographically among labels of equal kind, independent of registration order.

#### Scenario: No partial reveal
- **WHEN** the value `livesecret` is registered as `env.API_SECRET` and appears in output
- **THEN** the output shows `[REDACTED:env.API_SECRET]` in its place and contains no `***` fragment of the value

#### Scenario: Same value under two labels
- **WHEN** one value is registered both as `env.DB_PASS` and as `secrets.DB_PASSWORD`, in either order
- **THEN** every occurrence renders `[REDACTED:secrets.DB_PASSWORD]`

### Requirement: Labels never reveal values or provider locators
A label SHALL be built only from config names (secret keys, env keys, server, registry, environment and service names) and fixed vocabulary. It SHALL NOT contain a secret value, any part of one, its length, or any `secretSources` locator (provider path, names, vault, project, profile, or map source name).

#### Scenario: Provider locator never appears
- **WHEN** a secret is resolved from a `secretSources` entry with `path: /prod/app/` and is masked in output
- **THEN** the marker is `[REDACTED:secrets.<KEY>]` and the output does not contain `/prod/app/`

### Requirement: Labels can be disabled by the operator
When the environment variable `TAKO_REDACTION_LABELS` is `off`, `0`, `false` or `no` (case-insensitive), every mask SHALL be rendered as the plain `[REDACTED]`, in human and machine output alike. Key/value pattern matches SHALL still keep their key. Masking itself SHALL NOT be disableable.

#### Scenario: Labels off
- **WHEN** `TAKO_REDACTION_LABELS=off` is set and a registered `secrets.STRIPE_KEY` value appears in a log line
- **THEN** the `log.line` `message` and `data.data` contain `[REDACTED]` and do not contain `STRIPE_KEY`

### Requirement: Redaction is deterministic and idempotent
Registered values SHALL be replaced longest first, so that overlapping secrets produce the same output on every run. Applying redaction to already-redacted text SHALL NOT change it. Markers already present, and markers inserted during redaction, SHALL NOT be altered by registered values or patterns.

#### Scenario: Overlapping secrets
- **WHEN** both `abcd1234` (as `env.A_TOKEN`) and `xxabcd1234yy` (as `env.B_TOKEN`) are registered and output contains `xxabcd1234yy`
- **THEN** the output is `[REDACTED:env.B_TOKEN]` on every run

#### Scenario: Double redaction
- **WHEN** an error message that was already passed through `RedactSensitive` is emitted as an event
- **THEN** the emitted text equals the first redaction result and every marker is intact

#### Scenario: Marker survives the key/value pattern
- **WHEN** text containing `[REDACTED:secrets.API_KEY]` is redacted again
- **THEN** the marker is unchanged

### Requirement: envFile values are registered for redaction
Every command that registers inline service `env:` values SHALL also register the values loaded from the service's `envFile`/`envFiles`, using the same credential-like rule as inline `env:` values and the label `envFile.<NAME>`. `deploy` and `exec` SHALL ignore a load error here, because building the container env file fails on it. All other commands SHALL emit one `warning` event with `data.reason: "envfile-unreadable"` stating that the file's values will not be redacted, and continue.

#### Scenario: envFile secret masked in logs
- **WHEN** service `admin` has `envFile: .env.production` containing `SMTP_PASSWORD=pa55w0rd-smtp` and a container log line contains `pa55w0rd-smtp`
- **THEN** the `log.line` event shows `[REDACTED:envFile.SMTP_PASSWORD]` in `message` and `data.data`

#### Scenario: Plain envFile setting stays visible
- **WHEN** the envFile contains `LOG_LEVEL=debug` and output contains `debug`
- **THEN** `debug` is not masked

#### Scenario: Unreadable envFile in logs
- **WHEN** `tako logs admin` runs and `.env.production` does not exist on the operator machine
- **THEN** one `warning` event with `data.reason: "envfile-unreadable"` is emitted and log lines are still streamed

### Requirement: Backup storage credentials are registered for redaction
Every command that registers registry credentials SHALL also register each selected service's `backup.storage.accessKeyId` and `backup.storage.secretAccessKey`, labeled `services.<service>.backup.<field>`.

#### Scenario: Backup secret in a health-failure error
- **WHEN** `tako rollback` fails a health check and the embedded container log lines contain the value of `backup.storage.secretAccessKey` of service `mongo`
- **THEN** the error shows `[REDACTED:services.mongo.backup.secretAccessKey]`

### Requirement: config explain reports what will be redacted
`tako config explain --redaction` SHALL print, for the selected environment:
- every config value that will be masked, with its label and reason;
- for every service, each `secrets:` ref, `env` key and `envFile` key marked `masked` or `visible`, with the reason;
- `secrets:` refs that cannot be resolved locally or by any `secretSources`;
- the names kept public;
- the pattern IDs.

It SHALL exit 0 when entries are unresolvable.

#### Scenario: Masked and visible env keys
- **WHEN** service `site-renderer` has `env: {CMS_RUNTIME_MODE: site, API_TOKEN: abc}` and the user runs `tako config explain --redaction`
- **THEN** the output lists `env.CMS_RUNTIME_MODE` as `visible` (plain setting) and `env.API_TOKEN` as `masked` (credential-like name)
- **AND** the output contains neither `site` as a value nor `abc`

#### Scenario: Unresolvable secret
- **WHEN** service `admin` references secret `LEGACY_KEY` that is in no local secrets file and no `secretSources` apply
- **THEN** `secrets.LEGACY_KEY` is listed under "Not resolvable" and the command exits 0

#### Scenario: Labels state reported
- **WHEN** `TAKO_REDACTION_LABELS=off` is set and the user runs `tako config explain --redaction`
- **THEN** the report states that labels are off

### Requirement: config explain redaction report is offline and value-free
`tako config explain --redaction` SHALL NOT:
- print any secret or env value;
- print `secretSources` locators beyond the source index and provider name;
- contact servers;
- invoke secret provider CLIs.

It SHALL remain human-only.

#### Scenario: Sourced secret not resolved
- **WHEN** service `admin` references secret `SENDGRID_KEY` that is not in `.tako/secrets*` and the environment has `secretSources: [{provider: aws-ssm, path: /prod/}]`
- **THEN** `secrets.SENDGRID_KEY` is listed as `masked` with origin `secretSources[0] aws-ssm; not resolved here`
- **AND** `/prod/` does not appear and no provider CLI is executed

#### Scenario: Machine output refused
- **WHEN** the user runs `tako config explain --redaction --output json`
- **THEN** the command fails with the existing human-only error

## MODIFIED Requirements

### Requirement: Logs redact every resolved secret value
Before emitting any `log.line` event, `tako logs` SHALL register for redaction the values of all secrets referenced by every selected service, including named jobs. This SHALL cover:
- values resolved from `secretSources`;
- values from local `.tako/secrets*` files;
- credential-like literal `env` entries;
- credential-like values from the service's `envFile`/`envFiles`.

#### Scenario: secretSources value is masked in logs
- **WHEN** service `web` references a secret whose value is resolved from a `secretSources` provider and a container log line contains that value
- **THEN** the `log.line` event shows the value redacted in both `data.data` and `message`

#### Scenario: Local secrets file value is masked in logs
- **WHEN** service `web` references a secret stored in `.tako/secrets` and a container log line contains that value
- **THEN** the value is redacted in human output and in the `log.line` event

#### Scenario: Job secrets are registered
- **WHEN** the user runs `tako logs nightly` for a job that references a secret and the recorded run output contains the secret value
- **THEN** the value is redacted

#### Scenario: envFile credential is masked in logs
- **WHEN** service `web` loads `envFile: .env.production` containing `DB_PASSWORD=s3cr3t-pass` and a container log line contains `s3cr3t-pass`
- **THEN** the `log.line` event shows `[REDACTED:envFile.DB_PASSWORD]` in both `data.data` and `message`
