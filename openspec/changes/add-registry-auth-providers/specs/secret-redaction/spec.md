# Spec Delta

## ADDED Requirements

### Requirement: Minted registry tokens are redacted
Tako SHALL register every registry token minted by a provider with the output redactor, under the label `registries.<host>.token`, before the token is used or returned to any caller. The token SHALL then be masked (as `[REDACTED:registries.<host>.token]` when labels are on) in human output, NDJSON events, result documents, and errors that embed docker output. Tokens shorter than 16 characters SHALL be rejected as malformed.

#### Scenario: Docker error echoes the token
- **WHEN** a pull fails and the node's error output contains the minted ECR token
- **THEN** the `image.pull.auth_failed` event and the final error show the token masked

#### Scenario: Redaction report lists a provider registry without minting
- **WHEN** `tako config explain --redaction` runs for a config with `registries.gcr.io.provider: gcr`
- **THEN** the Masked section lists `registries.gcr.io.token` with reason `registry token (provider)`, and no provider CLI runs

### Requirement: Provider CLI output never leaks a token
When a registry provider command fails, Tako SHALL NOT include the command's standard output in any error, event or log line. It MAY include standard error, and only after passing it through the operation's labeled redactor.

#### Scenario: Provider CLI fails with empty stderr
- **WHEN** `gh auth token` exits non-zero with empty stderr and a token on stdout
- **THEN** the error names the command and its exit status, and does not contain the stdout value

#### Scenario: Provider CLI error with stderr
- **WHEN** `aws ecr get-login-password` fails and writes a message to stderr
- **THEN** the error shows that message after redaction
