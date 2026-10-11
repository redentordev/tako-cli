# Proposal

## Why

Redaction in Tako is silent. A mask says that *something* was hidden, but not what or why. When it goes wrong, users cannot tell the difference between a leak guard working and a guard misfiring.

The jardin-cms incident fixed in v0.14.1 is the example. Deploy output showed `[REDACTED]-renderer` and masked image tags. Finding the cause, `CMS_RUNTIME_MODE: site` registered as a secret, took a full debugging session. A label such as `[REDACTED:env.CMS_RUNTIME_MODE]-renderer` would have explained it in one glance.

There is also no way to ask Tako, before deploying, "what will you mask, and is my secret actually covered?". While researching this change we found two real gaps that such a report would have exposed:

- **`envFile` / `envFiles` values are never registered with the redactor.** `.env.production` style files are loaded only inside `secrets.Manager.CreateEnvFile` (`pkg/secrets/manager.go:424-444`), when the container env file is built (`pkg/deployer/takod_deploy.go:2220`, `pkg/engine/exec.go:333`). No engine registration path ever sees those values. As a result, an `SMTP_PASSWORD` that lives only in `.env.production` is not masked in `tako logs`, deploy health-failure errors, rollback, promote, scale, exec or hooks output. Pattern redaction catches only some of these (JWTs, long base64, known token prefixes).
- **Service backup storage credentials (`services.<name>.backup.storage.accessKeyId` / `secretAccessKey`, `pkg/config/types.go:756-757`) are never registered.** Only the generic patterns protect them.

## What Changes

- **Labeled masks.** Every mask produced by the engine redactor becomes `[REDACTED:<source>]`. `<source>` is a config-path-like label naming *where* the value came from, never the value and never a provider locator. Examples:
  - `[REDACTED:secrets.STRIPE_KEY]`
  - `[REDACTED:env.DATABASE_URL]`
  - `[REDACTED:envFile.SMTP_PASSWORD]`
  - `[REDACTED:servers.app.password]`
  - `[REDACTED:registries.ghcr.io.password]`
  - `[REDACTED:pattern.jwt]`
- **BREAKING (human and machine output text):**
  - Registered secrets are now fully masked. The old partial reveal (`li***et`, first and last two characters) is removed. It leaked up to four characters and the rough length of every secret, and the label replaces its only use, which was guessing which secret matched.
  - Key/value pattern matches keep their key, so `password=[REDACTED]` becomes `password=[REDACTED:pattern.assignment]`.
  - The marker text was never a documented contract. The new stable grammar is documented in `docs/MACHINE-INTERFACE.md`.
- **Opt-out.** Setting `TAKO_REDACTION_LABELS=off` (also `0`, `false`, `no`) emits a plain `[REDACTED]` for every mask, for environments where even secret *names* must not appear in output. Masking itself cannot be turned off.
- **Deterministic, idempotent redaction.**
  - Registered secrets are replaced longest first. Today map iteration order decides, so a secret that is a substring of another can produce different output on different runs.
  - Existing markers this redactor could have produced are protected, so redacting already-redacted text (the engine does this when `RedactSensitive` output is re-emitted) never rewrites or nests markers. Other marker-shaped text is redacted normally, so it cannot shield a secret.
- **Leak fix: public names no longer shield secrets.** Today public values (project, service, environment names) are protected before secrets are replaced, so a secret that contains the project name, such as `acme-prod-token-…` in project `acme`, is no longer matched and is printed minus the name. Secrets and public values are now resolved in one pass; a secret stays visible only when it lies entirely inside a longer public value (the jardin-cms case).
- **Registration gaps closed:**
  - `envFile`/`envFiles` values are registered with the same `secrets.ShouldRedactEnvValue` rule as inline `env:` values, everywhere inline env values are registered today.
  - Backup storage credentials are registered in every command that registers config secrets.
- **`tako config explain --redaction`.** A new flag on the existing human-only `config explain` command. For the selected environment it lists every value that will be masked and why, every service env key that stays visible and why, and the names kept public. It never prints a value, never contacts servers and never invokes secret provider CLIs.
- Registration API: `Redactor.Register(value)` becomes `Redactor.RegisterLabeled(value, label)`. `Register(value)` remains as an unlabeled shim. The engine gains `RegisterSecretAs(value, label)`, and every existing call site passes a label.

Out of scope:
- Redactors outside the engine/`secrets.Redactor` path keep their plain `[REDACTED]`. These are `pkg/unregistry` (`unregistry.go:99-111`), takod's ACME DNS error scrubbing (`pkg/takod/acme_dns.go:1045`), and `tako secrets list` display (`cmd/secrets.go:153,296,355`).
- Changing *which* values count as credential-like (the v0.14.1 `ShouldRedactEnvValue` rule is unchanged).
- A machine-readable (`--output json`) form of the redaction report; `config explain` stays human-only.
- Resolving `secretSources` from `config explain`.

## Capabilities

### New Capabilities
- None.

### Modified Capabilities
- `secret-redaction`: adds labeled masks, the label opt-out, deterministic and idempotent redaction, the rule that public values never shield a secret, registration of `envFile` values and backup storage credentials, and the `config explain --redaction` report. It also modifies "Logs redact every resolved secret value" to include `envFile` values.

## Impact

- Redactor: `pkg/secrets/redactor.go` (labels, pattern IDs, marker protection, one longest-first pass over secrets and public values replacing `protectPublic`, removal of `createRedacted` partial reveal), `pkg/secrets/redactor_test.go`.
- Labels and reasons: a new `pkg/secrets/labels.go` with label constructors, the label grammar validator, and a `RedactionReason` enum shared by registration and the explain report.
- Engine registration: `pkg/engine/engine.go` (`RegisterSecretAs`, labeled `RegisterServiceEnvValues`, new `RegisterServiceEnvFileValues`, `RegisterBackupSecrets`, labeled registry/ACME helpers). Every `RegisterSecret` caller is updated with a label:
  - in `pkg/engine`: `deploy.go`, `destroy.go`, `exec.go`, `exec_interactive.go`, `jobs.go`, `logs.go`, `promote.go`, `remove.go`, `rollback.go`, `rollback_project.go`, `run.go`, `scale.go`, `state_lease.go`, `status.go` and `config_export.go`;
  - in `cmd`: `certs.go`, `hooks.go` and `proxy.go`.
- `secrets.Manager`: per-key labeling of loaded secrets (`manager.go:109-112`), and an exported helper that loads `envFile` values without building the env file.
- CLI: `cmd/config.go` (`--redaction` flag and report), `cmd/config_test.go`.
- Tests asserting the partial-reveal format must be updated: `pkg/engine/logs_test.go:395` and `pkg/engine/logs_secrets_test.go:135`.
- Docs:
  - `docs/MACHINE-INTERFACE.md` (Redaction Guarantees: marker grammar, opt-out, envFile and backup credentials);
  - `docs/API-SDK.md` (redactor note, `RegisterSecretAs`);
  - `docs/CONFIGURATION.md` (envFile redaction, explain flag);
  - `examples/test-secrets/README.md` (it references `po***ql` partial values);
  - `man/tako-config-explain.1` and `completions/*` (regenerated).
- No takod change, no capability, no `tako.yaml` schema change.
