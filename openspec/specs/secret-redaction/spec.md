# secret-redaction Specification

## Purpose
Defines which resolved secret values Tako registers with its output redactor before streaming container output or errors that embed container output, so that secret values never appear in plaintext in human output, events, or result documents.

## Requirements

### Requirement: Logs redact every resolved secret value
Before emitting any `log.line` event, `tako logs` SHALL register for redaction the values of all secrets referenced by every selected service, including named jobs. This SHALL cover values resolved from `secretSources`, from local `.tako/secrets*` files, and from literal `env` entries.

#### Scenario: secretSources value is masked in logs
- **WHEN** service `web` references a secret whose value is resolved from a `secretSources` provider and a container log line contains that value
- **THEN** the `log.line` event shows the value redacted in both `data.data` and `message`

#### Scenario: Local secrets file value is masked in logs
- **WHEN** service `web` references a secret stored in `.tako/secrets` and a container log line contains that value
- **THEN** the value is redacted in human output and in the `log.line` event

#### Scenario: Job secrets are registered
- **WHEN** the user runs `tako logs nightly` for a job that references a secret and the recorded run output contains the secret value
- **THEN** the value is redacted

### Requirement: Aliased secret refs are redacted
A secret ref of the form `CONTAINER_VAR:SECRET_KEY` SHALL be resolved for redaction by `SECRET_KEY`, the same key used to inject it into the container. This SHALL apply in every place that registers secret values, including `tako logs`, `tako exec`, `tako rollback`, `tako promote` and `tako scale`.

#### Scenario: Aliased secret masked in logs
- **WHEN** service `web` declares `secrets: ["API_KEY:STRIPE_KEY"]` and a container log line contains the value of `STRIPE_KEY`
- **THEN** the `log.line` event shows that value redacted in both `data.data` and `message`

#### Scenario: Aliased secret masked in exec output
- **WHEN** `tako exec web -- env` runs for a service with `secrets: ["API_KEY:STRIPE_KEY"]`
- **THEN** the value of `STRIPE_KEY` is redacted in the streamed output

### Requirement: Unresolvable secrets warn without blocking logs
If the secret values for the selected services cannot be resolved (for example, a provider CLI is missing or a vault is unreachable), `tako logs` SHALL emit one `warning` event stating that sourced secret values will not be redacted, and SHALL continue streaming. The warning text SHALL itself be redacted and SHALL NOT contain any secret value.

#### Scenario: Provider unavailable
- **WHEN** a selected service references a `secretSources` secret and the provider CLI exits with an error
- **THEN** a `warning` event is emitted saying sourced values will not be redacted
- **AND** log lines are still streamed and the exit code is determined only by node streaming results

### Requirement: Commands that embed container output register secrets
`tako rollback`, `tako promote` and `tako scale` SHALL register the same secret values as `tako logs` before emitting any event, because health-check failure errors include recent container log lines. When resolution fails they SHALL emit the same `warning` and continue.

#### Scenario: Rollback health failure error is redacted
- **WHEN** `tako rollback` fails because the target container fails its health check and the embedded container log lines contain a `secretSources` value
- **THEN** the error in the result document and in events shows that value redacted

#### Scenario: Scale with aliased secret
- **WHEN** `tako scale web=3` fails a health check and the embedded log lines contain the value of an aliased secret ref
- **THEN** the value is redacted in the error output
