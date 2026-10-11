## ADDED Requirements

### Requirement: Preview hook context variables
When a hooked command (`deploy` or `rollback`) runs for a preview, every hook SHALL additionally receive `TAKO_PREVIEW_NAME` (the normalized preview name), `TAKO_PREVIEW_BASE` (the base environment name) and `TAKO_PREVIEW_EXPIRES_AT` (RFC 3339 UTC, empty when the preview never expires); `TAKO_ENVIRONMENT` SHALL be the preview's environment identity. These variables SHALL be absent for non-preview runs.

#### Scenario: Seeding a preview database
- **WHEN** `post-deploy` runs after `tako deploy --preview pr-42` from base `preview`
- **THEN** the hook receives `TAKO_ENVIRONMENT=preview-pr-42`, `TAKO_PREVIEW_NAME=pr-42` and `TAKO_PREVIEW_BASE=preview`

#### Scenario: Normal deploy
- **WHEN** `post-deploy` runs after `tako deploy -e staging`
- **THEN** no `TAKO_PREVIEW_*` variable is set in the hook environment
