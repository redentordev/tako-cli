# Tasks

## 1. Redactor core (`pkg/secrets`)

- [ ] 1.1 Add `pkg/secrets/labels.go`:
  - label constructors for every kind in design D1;
  - `SanitizeLabel` (segment charset, `_` replacement, 96-byte cap that keeps the kind prefix);
  - the kind precedence table (D3);
  - a `RedactionReason` enum with display strings (D9).

  Verify with table tests for each constructor, sanitization, and truncation.
- [ ] 1.2 Replace `createRedacted` with marker rendering:
  - `[REDACTED:<label>]`;
  - `[REDACTED]` when the label is empty or labels are off.

  Add `RegisterLabeled(value, label)`, with precedence and lexicographic tie-break when a value is registered twice, and make `Register` call it with `""`. Verify tests:
  - no partial reveal for 5-, 8- and 30-character secrets;
  - the same value registered as `env.X` and `secrets.Y` renders `secrets.Y` regardless of order.
- [ ] 1.3 Give each compiled pattern an ID (D1 table) and make `redactPatternMatches` render `pattern.<id>`, keeping the key for `assignment` matches. Verify tests for `password=hunter2…` → `password=[REDACTED:pattern.assignment]`, a JWT → `[REDACTED:pattern.jwt]`, and that the existing hex image-tag and digest exemptions still hold.
- [ ] 1.4 Replace registered secrets longest first, using a lazily rebuilt sorted slice (D4). Verify that two overlapping secrets (`abcd1234` inside `xxabcd1234yy`) give identical output across 100 runs.
- [ ] 1.5 Protect existing markers and the inserted markers with placeholders (D5). Verify:
  - `Redact(Redact(x)) == Redact(x)` over a corpus;
  - `[REDACTED:secrets.API_KEY]` survives the key/value pattern;
  - a registered secret equal to `REDACTED` does not corrupt markers.
- [ ] 1.6 Read `TAKO_REDACTION_LABELS` in `NewRedactor` (`off|0|false|no`, case-insensitive). Verify a test that sets the env var and asserts plain `[REDACTED]` for registered values and patterns, with keys still kept for assignment matches.
- [ ] 1.7 Register `secrets.Manager` loaded values as `secrets.<KEY>` (`manager.go:109-112`). Verify that `sources_test.go` still passes and that the label appears.
- [ ] 1.8 Extract `LoadServiceEnvFiles(service)` from `CreateEnvFile` (`manager.go:431-444`) and have `CreateEnvFile` use it. Verify that existing `CreateEnvFile` tests still pass and add a precedence test (envFile vs envFiles, later file wins).

## 2. Engine registration labels

- [ ] 2.1 Add `Engine.RegisterSecretAs(value, label)`. Label inside the existing helpers:
  - `RegisterServiceEnvValues` → `env.<NAME>`;
  - `RegisterRegistrySecrets` → `registries.<host>.password`;
  - `RegisterACMEDNSSecrets` → `environments.<env>.proxy.acme.<NAME>`.

  Verify unit tests per helper.
- [ ] 2.2 Label `registerServiceSecretValues` and `buildExecEnvFileContent` as `secrets.<SecretRefKey>`. Do the same for `deploy.go:240`. Verify an aliased ref `API_KEY:STRIPE_KEY` renders `[REDACTED:secrets.STRIPE_KEY]` in a `log.line`.
- [ ] 2.3 Migrate every SSH-password registration to `servers.<name>.password`. The call sites are listed in design.md Context. Verify with `go build ./...`.
- [ ] 2.4 Migrate the remaining call sites to the D1 labels:
  - notifications;
  - `run.go:113` (`run.env.<NAME>`);
  - `config_export.go:129,152`;
  - `cmd/hooks.go:174`;
  - `cmd/certs.go:82`;
  - `cmd/proxy.go:55`.

  Verify with `go build ./...`.
- [ ] 2.5 Update the partial-reveal assertions in `pkg/engine/logs_test.go:395` and `pkg/engine/logs_secrets_test.go:135` to the labeled markers. Verify with `go test ./pkg/engine`.
- [ ] 2.6 Add a guard test that scans non-test `.go` files under `pkg/engine` and `cmd` and fails on any remaining unlabeled `RegisterSecret(` call. Verify that it fails when one call is reverted.

## 3. Close registration gaps

- [ ] 3.1 Add `Engine.RegisterServiceEnvFileValues(services) error`, which uses `LoadServiceEnvFiles` and `ShouldRedactEnvValue` and labels values `envFile.<NAME>`. Verify unit tests:
  - a credential-like name is masked;
  - a plain setting is not;
  - a token-shaped value is masked;
  - an unreadable file returns an error and registers nothing.
- [ ] 3.2 Call it in `deploy` and `exec`, ignoring the error because `CreateEnvFile` fails authoritatively later.
- [ ] 3.3 Call it in `logs`, `rollback`, `rollback_project`, `promote`, `scale`, `remove`, `destroy` and `cmd/hooks.go registerSecrets`. Wrap it so one `warning` event with `data.reason: "envfile-unreadable"` is emitted on error and the command continues. Verify:
  - an engine test where a `log.line` containing an `.env.production` `SMTP_PASSWORD` value renders `[REDACTED:envFile.SMTP_PASSWORD]` (this is the regression test for the gap);
  - a test with a missing file asserting one warning and that streaming continues.
- [ ] 3.4 Add `Engine.RegisterBackupSecrets(services)` (`services.<svc>.backup.secretAccessKey|accessKeyId`) and call it at each call site of `RegisterRegistrySecrets` and in the commands listed in design D8. Verify a test where a health-failure error containing the backup secret key is masked.

## 4. `tako config explain --redaction`

- [ ] 4.1 Add a shared classifier in `pkg/secrets` (or `pkg/engine`) that returns `[]RedactionEntry` for the config, the selected environment and its services, and make the engine registration helpers derive from it. Verify a parity test: for a fixture config, the set of labels registered by the engine equals the set of `Masked` entries from the classifier. `secrets:` and passEnv values, which the test supplies, are included.
- [ ] 4.2 Add the `--redaction` flag to `configExplainCmd` and a `runConfigRedactionReport` renderer following the D9 layout:
  - sections in order: Masked, Services, Not resolvable, Always visible, Patterns;
  - deterministic sorting;
  - secret origin wording as in D9;
  - no values, no provider locators, no network or provider calls.

  Verify golden tests in `cmd/config_test.go`. One golden test uses a jardin-cms-shaped config (with `CMS_RUNTIME_MODE: site` shown as visible). A second asserts that no fixture value string appears anywhere in the output.
- [ ] 4.3 Make `--output json` with `--redaction` return the existing human-only error. Verify a test, and verify `go test ./cmd -run Machine`.
- [ ] 4.4 Regenerate `man/tako-config-explain.1` and `completions/*`. Verify that `git diff --stat` touches only config-explain-related generated files.

## 5. Docs

- [ ] 5.1 Update `docs/MACHINE-INTERFACE.md` "Redaction Guarantees":
  - the marker grammar and detection regex;
  - the label kinds and stability promise;
  - removal of the partial reveal;
  - `TAKO_REDACTION_LABELS`;
  - `envFile` and backup-credential registration;
  - the `envfile-unreadable` warning.

  Verify by review against D1, D6, D7 and D10.
- [ ] 5.2 Update the redactor notes in `docs/API-SDK.md` (`RegisterSecretAs`, labels), `docs/CONFIGURATION.md` (envFile redaction, the `config explain --redaction` example) and `examples/test-secrets/README.md` (replace the `po***ql` references). Verify with the docs snippet harness (`go test ./... -run Docs`).

## 6. Integration verification

- [ ] 6.1 Run `go build ./...`, `go vet ./...`, and `go test ./...` on Linux (WSL), compare against master, and confirm gofmt is clean. Verify that there are no new failures.
- [ ] 6.2 Manual check:
  - Run `tako config explain --redaction -e production` on a project with an `envFile`, a `secretSources` key and a backup block, and confirm that no value appears.
  - Run `tako logs` on a test service that echoes an `envFile` password, and confirm `[REDACTED:envFile.<NAME>]`.
  - Rerun with `TAKO_REDACTION_LABELS=off` and confirm a plain `[REDACTED]`.
