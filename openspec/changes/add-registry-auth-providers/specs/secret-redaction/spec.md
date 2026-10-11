# Spec Delta

## ADDED Requirements

### Requirement: Minted registry tokens are redacted
Tako SHALL register every registry token minted by a provider with the output redactor before the token is used or returned to any caller, so that it is masked in human output, NDJSON events, result documents, and errors that embed provider CLI stderr or docker output. Tokens shorter than 16 characters SHALL be rejected as malformed.

#### Scenario: Docker error echoes the token
- **WHEN** a pull fails and the node's error output contains the minted ECR token
- **THEN** the `image.pull.auth_failed` event and the final error show the token masked

#### Scenario: Provider CLI error
- **WHEN** `aws ecr get-login-password` fails and its stderr is included in the error
- **THEN** the error is passed through the redactor before it is printed
