# Design

## Context

Observed behavior today (v0.14.1):

### Redactor (`pkg/secrets/redactor.go`)

- `Redactor.secrets map[string]string` maps value → replacement. `Register(value)` (`:134-146`) skips values shorter than 4 characters. It stores `createRedacted(value)` (`:149-161`):
  - `[REDACTED]` for values of 4 characters or fewer;
  - `x***` for values of 5–8 characters (first character kept);
  - `ab***yz` otherwise (first and last two characters kept).
- `Redact` (`:184-204`) runs these steps in order:
  1. Protects registered public values with `\x00N\x00` placeholders, longest first (`protectPublic`, `:208-235`).
  2. Replaces registered secrets by ranging over the map. **The iteration order is random**, so when one secret contains another the output is nondeterministic.
  3. Applies 17 regex patterns (`compilePatterns`, `:85-131`) through `redactPatternMatches` (`:241-261`). That function skips hex image tags and digests and, for key/value matches, keeps the key (`redactMatch`, `:281-288`).
  4. Restores the placeholders.
- Patterns are anonymous, so there is no way to say which pattern fired.
- **Leak: a public value shields a longer secret that contains it.** Step 1 replaces public values before any secret is looked at. With project `acme` (registered public) and secret `acme-prod-token-9f8e…`, the project name inside the secret becomes a placeholder, the secret no longer matches in step 2, and the rest of the secret is printed unless a pattern happens to catch it. Public values are only 4+ character config names, but project and service names are routinely used as prefixes of generated credentials.
- `RedactMap` and `IsSensitiveKey` (`:291-325`) have no callers outside the package.

### Engine (`pkg/engine/engine.go:89-202`)

- `New` builds one redactor and passes `redactor.Redact` to `events.NewStream`. `Stream.Emit` redacts `Message` and the top-level string `Data` values (`pkg/takoapi/events/sinks.go`).
- `RedactSensitive` (`:155`) exposes the same function for result and error fields. Several paths redact twice: `deploy.go:861` redacts an error that is later emitted, and `exec.go:393` embeds `RedactSensitive` output in an event message.
- Registration helpers:
  - `RegisterSecret(value)` (`:106`);
  - `RegisterPublic` (`:115`);
  - `RegisterServiceEnvValues` (`:124-132`, uses `secrets.ShouldRedactEnvValue`);
  - `RegisterPublicConfigNames` (`:137-151`);
  - `RegisterRegistrySecrets` (`:179-186`);
  - `RegisterACMEDNSSecrets` (`:190-202`).
- `registerServiceSecretValues` (`pkg/engine/exec.go:351-381`) registers values referenced by `secrets:` lists, resolving aliased refs with `secrets.SecretRefKey`.

### Unlabeled `RegisterSecret` call sites (all must gain a label)

- **SSH passwords:** `deploy.go:246`, `destroy.go:172`, `exec.go:121`, `exec_interactive.go:76`, `jobs.go:347`, `logs.go:302`, `promote.go:117`, `remove.go:148`, `rollback.go:175`, `rollback_project.go:295`, `run.go:115`, `scale.go:131`, `state_lease.go:102`, `status.go:116`, `cmd/hooks.go:156`.
- **Notifications:** `deploy.go:251-253`, `cmd/hooks.go:165-167`.
- **Deploy secret values:** `deploy.go:240`.
- **Run request env:** `run.go:113`.
- **Exec secrets:** `exec.go:330`.
- **Config export password:** `config_export.go:129,152`.
- **Hook passEnv credentials:** `cmd/hooks.go:174`.
- **Generated TLS private key:** `cmd/certs.go:82`.
- **`proxy hash-password` input:** `cmd/proxy.go:55`.

### `secrets.Manager`

`secrets.Manager` (`pkg/secrets/manager.go:83-115`) has its own redactor. It registers every loaded secret unlabeled (`:109-112`). `GetRedactor()` is used by `safe_output.go`.

### Gap 1: `envFile` values are never registered

`envFile`/`envFiles` are read only by `Manager.CreateEnvFile` (`manager.go:424-444`) through `config.LoadEnvFile`. The callers are:
- deploy, when building the container env file (`pkg/deployer/takod_deploy.go:2210-2235`);
- exec (`pkg/engine/exec.go:333`).

`pkg/deployer` has no redaction at all. No `Register*` call ever receives an `envFile` value. Values from those files therefore reach these outputs unmasked, unless a generic pattern happens to match:
- `tako logs` lines;
- health-check failure errors (which embed the last container log lines, `pkg/takod/reconcile.go`);
- `exec` output;
- rollback, promote and scale errors;
- hook output.

The project-level `.env` beside `tako.yaml` (`config/types.go:1272-1288`) only feeds `${VAR}` expansion. Expanded values land in `env:` and other fields and are covered by the existing rules, so they are not part of this gap.

### Gap 2: backup storage credentials are never registered

`backup.storage.accessKeyId` and `secretAccessKey` (`config/types.go:756-757`) are never passed to any `Register*` call.

### `tako config explain`

`tako config explain` (`cmd/config.go:19-159`) is documented as human-only (`docs/MACHINE-INTERFACE.md:624`). It prints runtime, state, mesh, servers and services with value sources, uses the global `--env/-e` flag, has no flags of its own, and already prints `auth: password (..., redacted)` without the value.

### Contract text

`docs/MACHINE-INTERFACE.md:770-809` ("Redaction Guarantees") promises *what* is masked, not the marker text. Places that assume the marker text:
- tests: `pkg/engine/logs_test.go:395` (`li***et`) and `pkg/engine/logs_secrets_test.go:135` (`va***13`, `st***07`, `lo***55`);
- `examples/test-secrets/README.md:102,120,162` (`po***ql`).

No SDK or doc promises the literal `[REDACTED]`.

## Goals / Non-Goals

**Goals:**
- Every mask says where the hidden value came from, without revealing the value or a provider locator.
- One answer, before deploying, to "what does Tako mask in this environment, and why?".
- Close the `envFile` and backup-credential registration gaps the report would otherwise show as "not masked".
- Deterministic, idempotent redaction output.

**Non-Goals:**
- Changing what counts as credential-like (`ShouldRedactEnvValue` is unchanged).
- Labeling the independent scrubbers in `pkg/unregistry`, takod ACME DNS, and `tako secrets list`.
- A JSON redaction report, or resolving `secretSources` from `config explain`.
- Disabling redaction.

## Decisions

### D1. Marker grammar

```
marker  = "[REDACTED" [ ":" label ] "]"
label   = segment *( "." segment )        ; 1..96 bytes
segment = 1*( ALPHA / DIGIT / "_" / "-" / "@" / "/" / ":" )
```

The opt-out (D6) uses the bare `[REDACTED]`. Labels are config-path-like and start with a fixed kind:

| Source | Label | Notes |
|---|---|---|
| `secrets:` ref (local `.tako/secrets*` or `secretSources`) | `secrets.<KEY>` | `<KEY>` is the Tako secret key. For an aliased ref `API_KEY:STRIPE_KEY` the label is `secrets.STRIPE_KEY`, which names what to rotate. Same label in `deploy.go:240` and `exec.go:330`. |
| Credential-like inline `env:` value | `env.<NAME>` | |
| Credential-like `envFile`/`envFiles` value | `envFile.<NAME>` | New registration (D7). |
| SSH password | `servers.<server>.password` | |
| Registry password | `registries.<host>.password` | |
| ACME DNS credential | `environments.<env>.proxy.acme.<NAME>` | |
| Backup storage credential | `services.<service>.backup.secretAccessKey` / `services.<service>.backup.accessKeyId` | `backup` exists only on services (`ServiceConfig.Backup`, `config/types.go:390`). |
| Shared build arg value | `builds.<build>.args.<ARG>` | Added during implementation: the `deploy.go:240` call site registers every `builds.<name>.args` value, not `secrets:` values, and no other kind fits. |
| Notification URL | `notifications.slack` \| `notifications.discord` \| `notifications.webhook` | |
| Hook `passEnv` credential | `hooks.passEnv.<NAME>` | |
| `run` request env (`run.go:113`) | `run.env.<NAME>` | |
| `config export` password | `export.password` | |
| Generated TLS key | `certs.privateKey` | |
| `proxy hash-password` input | `proxy.password` | |
| `secrets.Manager` loaded value | `secrets.<KEY>` | |
| Pattern match | `pattern.<id>` | |

Pattern IDs:

| ID | Pattern(s) |
|---|---|
| `assignment` | the three key/value patterns; the key is kept |
| `connection-string` | mysql, postgres, mongodb, redis |
| `stripe-key` | Stripe |
| `github-token` | GitHub |
| `npm-token` | npm |
| `digitalocean-token` | DigitalOcean |
| `aws-access-key` | AWS |
| `google-api-key` | Google |
| `hex64` | 64-character hex |
| `jwt` | JWT |
| `base64` | long base64 |
| `hex` | long hex |

Characters outside `segment` in a name, which config validation should already prevent, are replaced with `_`. A label longer than 96 bytes is truncated, keeping the kind prefix.

- **Why config paths, not bare names:** a bare `[REDACTED:API_KEY]` cannot say whether the value came from `env:`, `envFile` or `secrets:`, and that is exactly the question users need answered. The kind prefix makes the label unambiguous for any user-chosen name.
- **Why `pattern.<id>`:** it distinguishes "a registered value matched" (a real secret Tako knew about) from "a heuristic matched" (possibly a false positive such as a long base64 build ID). That distinction is the most useful debugging signal.
- **Leakage analysis.** Labels only carry names that already appear in the committed `tako.yaml` (env keys, secret keys, server, registry host and service names) or fixed vocabulary.
  - They never carry a value, a value length or a partial value.
  - They never carry a `secretSources` locator: provider path, ARN, vault or item URI, project, profile or `map` source name.
  - A label does reveal *which* secret appears at that position of a log line. The same operator could learn this today by comparing against `tako secrets list`, and it is the purpose of the feature.
  - Where even that is unacceptable, use D6.

### D2. Full masking replaces partial reveal

`createRedacted` is removed. A registered value is always replaced by its marker.
- The partial reveal leaked 2–4 characters of every secret over 4 characters, and its shape leaked approximate length.
- Its only practical use, guessing which secret matched, is served by the label.
- With labels off (D6), the replacement is plain `[REDACTED]`.

### D3. Labeled registration API

- **In `pkg/secrets`:**
  - `func (r *Redactor) RegisterLabeled(value, label string)`.
  - `Register(value)` becomes `RegisterLabeled(value, "")`, which renders as `[REDACTED]`. It is kept so external `pkg/secrets` users compile.
- **When the same value is registered under several labels**, the label with the highest precedence wins, with ties broken lexicographically. The precedence, highest first:
  1. `secrets.`
  2. `envFile.`
  3. `env.`
  4. `registries.`
  5. `servers.`
  6. `environments.`
  7. `services.` (backup credentials)
  8. `builds.` (shared build args; added during implementation)
  9. `notifications.`
  10. `hooks.`
  11. `run.`
  12. `export.`
  13. `certs.`
  14. `proxy.`
  15. unlabeled

  This makes the output independent of registration order. A value that is both in `secrets:` and copied into an env var reports `secrets.<KEY>`, which is the source to rotate.
- **In the engine:**
  - `func (e *Engine) RegisterSecretAs(value, label string)`.
  - `RegisterSecret(value)` stays as an unlabeled shim, but every in-tree caller is migrated, and a test (task 2.6) fails if any non-test call to `RegisterSecret(` remains under `pkg/engine` or `cmd`.
  - Helpers that build labels take names from the config, never from values.
- **In `secrets.Manager`,** loaded secrets are registered as `secrets.<KEY>` (`manager.go:109-112`).

### D4. One deterministic pass over secrets and public values

`protectPublic` and the map-ordered secret loop are replaced by one pass that treats public values and secrets together:

1. Find every occurrence of every registered secret and every registered public value in the input.
2. A secret occurrence is left alone only when it lies entirely inside an occurrence of a **longer** public value. This keeps today's intended behavior: service `jardin-cms` stays readable even when a short secret such as `cms1` is a substring of it.
3. Every other secret occurrence is masked. Longer secrets are matched first, ties broken by value order, so a secret that contains another is masked as one marker with the longer secret's label.
4. A public occurrence is protected (placeholder) only when it does not overlap a masked secret occurrence. A public value inside or overlapping a secret therefore never shields it, which closes the leak described in Context.
5. Patterns run on the result; markers and protected public values are placeholders and are never matched. All placeholders are restored at the end.

A value registered as both public and secret is a secret (as today). The sorted value lists are cached and rebuilt lazily after `Register*` calls, which happen before streaming, so steady-state cost is unchanged. The output no longer depends on map iteration order.

### D5. Marker protection and idempotence

- Before anything else, `Redact` replaces existing markers with a placeholder, using the same NUL-delimited scheme as public values in a separate index space. Only markers this redactor could have produced are protected:
  - the bare `[REDACTED]`;
  - `[REDACTED:<label>]` where `<label>` is a label registered with this redactor;
  - `[REDACTED:pattern.<id>]` for a known pattern ID.
- Any other text shaped like a marker is ordinary input. Protecting every match of the grammar would let marker-shaped text shield a secret: an application that logs `[REDACTED:ghp_…]`, or a secret made only of label characters, would pass through unmasked because the whole bracket was treated as a finished marker. A foreign marker that is not protected can at worst be rewritten by a pattern, which is cosmetic, never a leak.
- The markers it inserts also go in as placeholders, so later patterns never see them. Without this, the key/value pattern `(?i)(secret|token|key|...)[\s=:]+([^\s]+)` would rewrite `[REDACTED:secrets.API_KEY]`, because `secrets.` contains `secret`, and break the label.
- All placeholders are restored at the end.
- Placeholders cannot be forged by input. Each protected span (marker, public value) becomes one NUL byte in the working text, with its text kept in an ordered per-call list, and every NUL byte already in the input is protected as itself before anything else. Every NUL in the working text is therefore a placeholder created by this call, restoration is positional, and input such as `shop \x000\x00-9f8e7d` can never splice protected text into a registered secret. A registered value containing a NUL byte is registered as its NUL-free parts.
- Guarantee: `Redact(Redact(x)) == Redact(x)` for the same redactor, which is what the deploy summary relies on when it re-redacts already-redacted strings. A registered value that is itself a substring of a marker (for example a secret equal to `REDACTED`) cannot corrupt the marker, because markers are protected before secret replacement.

### D6. Opt-out: `TAKO_REDACTION_LABELS`

- Read once in `secrets.NewRedactor`.
- Values `off`, `0`, `false` and `no` (case-insensitive) make every marker the plain `[REDACTED]`. Any other value, or unset, means labels are on.
- Key/value pattern matches still keep the key (`password=[REDACTED]`, as today).
- This is an environment variable rather than a CLI flag or a `tako.yaml` field:
  - It is an operator/CI policy, not a project property.
  - It must apply to every command, including hooks and SDK embedders.
  - It must not be committable by a contributor to quietly re-enable names.
- It applies identically to human and machine output. There is no separate JSON setting: event `message` and `data` strings are redacted by the same function, and splitting them would make the human `message` and the machine `data` disagree.

### D7. Register `envFile` values

- New `secrets.LoadServiceEnvFiles(service *config.ServiceConfig) (map[string]string, error)`. It is extracted from `CreateEnvFile`'s loop (`manager.go:431-444`), which then calls it, applying the same precedence: `envFile` wins over `envFiles`, and later files override earlier ones.
- New `Engine.RegisterServiceEnvFileValues(services)` registers each value with `ShouldRedactEnvValue(name, value)` as `envFile.<NAME>`. When a file cannot be read, it emits nothing and returns the error.
- Callers wrap the helper so behavior matches today's file handling:
  - **`deploy` and `exec`:** an unreadable file already fails `CreateEnvFile`, so they ignore the helper's error. The later failure is authoritative.
  - **`logs`, `rollback`, `rollback_project`, `promote`, `scale`, `remove`, `destroy` and hooks:** these call it next to `RegisterServiceEnvValues`. A failure emits one `warning` event with `data.reason: "envfile-unreadable"` ("could not read envFile for <service>; its values will not be redacted: <redacted error>") and continues, mirroring the existing `secrets-unresolved` warning. These commands do not otherwise need the file.
- An `env:` key that overrides an `envFile` key wins in the container. Both values are still registered if each is credential-like, because the file value may still appear in old logs.

### D8. Register backup storage credentials

New `Engine.RegisterBackupSecrets(services)` walks each selected service's `backup.storage` and registers `secretAccessKey`, `accessKeyId` and (added during implementation) the optional `sessionToken` under `services.<service>.backup.<field>`. With YAML anchors, the same value is registered for several services; D3's tie-break picks the lexicographically first label. It is called wherever `RegisterRegistrySecrets` is called, plus in deploy, destroy, logs, rollback (both forms), promote, scale, remove, hooks, and the `backup` command paths that emit events.

### D9. `tako config explain --redaction`

- **Invocation.** `tako config explain --redaction [-e ENV]`. With `--redaction`, only the redaction report is printed; the normal explain sections are skipped, to keep output focused. The command:
  - loads config exactly as `runConfigExplain` does, with expansion, so `${VAR}`-sourced values are evaluated;
  - reads local `envFile`s and local `.tako/secrets*` key names;
  - never prints a value;
  - never connects to servers;
  - never invokes provider CLIs.
- **Shared classification.** It reuses the registration code: a new `secrets.Classify...` layer returns `[]RedactionEntry{Label, Reason, Masked bool, Note}` for config and services. The engine registration helpers consume the same function, so the report cannot drift from what is actually registered (task 4.1 adds a parity test).
- **Reasons:**

  | Reason | Applies to |
  |---|---|
  | `secrets list` | `secrets:` refs |
  | `credential-like name` | `env` / `envFile` |
  | `URL with credentials` | `env` / `envFile` |
  | `token-like URL part` | `env` / `envFile` |
  | `token-shaped value` | `env` / `envFile` |
  | `ssh password` | servers |
  | `registry credential` | registries |
  | `DNS provider credential` | ACME |
  | `backup storage credential` | backup |
  | `notification URL` | notifications |
  | `passEnv credential name` | hook passEnv |
  | `plain setting` | visible `env` / `envFile` values |
  | `empty` | visible `env` / `envFile` values |

- **Example output:**

  ```
  Redaction: production (config ./tako.yaml)
  Labels: on (TAKO_REDACTION_LABELS)

  Masked
    servers.app.password                      ssh password
    registries.ghcr.io.password               registry credential
    services.mongo.backup.secretAccessKey     backup storage credential
    notifications.slack                       notification URL
    hooks.passEnv.DEPLOY_TOKEN                passEnv credential name (when set on this machine)

  Services
    admin:
      masked   secrets.STRIPE_KEY        secrets list (from .tako/secrets.production)
      masked   secrets.SENDGRID_KEY      secrets list (from secretSources[0] aws-ssm; not resolved here)
      masked   env.BETTER_AUTH_SECRET    credential-like name
      masked   env.WEBHOOK_URL           token-like URL part
      masked   envFile.SMTP_PASSWORD     credential-like name (.env.production)
      visible  env.NODE_ENV              plain setting
      visible  env.MONGODB_URI           plain setting (URL without credentials)
    ...
  Not resolvable
    secrets.LEGACY_KEY (admin)  not in .tako/secrets* and no secretSources; will not be redacted

  Always visible: project jardin-cms; environments production; servers app; service names; service images
  Patterns: assignment, connection-string, jwt, ... (heuristic, labeled pattern.<id>)
  ```

- **Secret origin wording:**
  - Found in local files: `from .tako/secrets[.<env>]`. Reading local files is the same work `tako secrets list` does.
  - Otherwise, when `secretSources` apply: `from secretSources[i] <provider>; not resolved here`. Only the index and provider name are shown, never the path or names.
  - Otherwise: listed under "Not resolvable".
  - A secretSources listing mode cannot be expanded statically, so the report states that sourced keys are verified at deploy/logs time.
- **Exit codes.** The command exits 0 even when entries are unresolvable; this is a report. An unreadable `envFile` is reported inline and does not fail the command.
- **Machine interface.** `config explain` stays in the human-only row of `docs/MACHINE-INTERFACE.md:624`, and `--output json` with `--redaction` returns the existing human-only error.
- **Previews.** If `add-preview-environments` lands, `--redaction --preview <name>` reports the preview instance, with local secrets read from the base's file (`.tako/secrets.<base>`), still offline.

### D10. Compatibility statement

`docs/MACHINE-INTERFACE.md` gains the marker grammar from D1 and a stability promise: consumers detect masks with `\[REDACTED(:[^\]]*)?\]`. Label *kinds* are stable; new kinds and pattern IDs may be added.

## Risks / Trade-offs

- **Output text changes for anyone who greps for the partial-reveal format or the bare `[REDACTED]`.** Marked BREAKING in the proposal. The marker was never documented, and the new grammar is.
- **Names of secrets now appear in logs.** Names come from committed config. `TAKO_REDACTION_LABELS=off` covers strict environments, and provider locators are never used.
- **Registering `envFile` values masks more output than today.** For example, a 16+ character mixed token in `.env.production` that is not actually secret will now be masked. The same rule already applies to inline `env:`, the label makes any such mask self-explaining, and `config explain --redaction` lists it as `token-shaped value`.
- **Explain and runtime registration drifting.** Both use one classification function, and a parity test checks it.
- **Public names inside secrets are now masked with the secret.** A line that contains a secret prefixed with the project name loses that name too. That is the intended fix: safety outranks readability, and the label says what was hidden.
- **Extra placeholder pass per `Redact` call.** One regex scan for existing markers. `Redact` is per event or line. The D4 pass resolves shielding with a worklist over occurrences sorted by start (each secret is masked and each public invalidated at most once, and lookups scan only occurrences within one value length), and protected publics are chosen with a byte-coverage map, so the work stays near-linear in line length and occurrence count. A first implementation iterated a secrets × publics fixpoint and took seconds on a 700 KB line with tens of thousands of occurrences; a benchmark and a time-bounded test guard against that regression.
- **An `envFile` read in read-only commands (`logs`, `rollback`) where the file is missing on the operator's machine.** This emits one warning and does not fail.

## Migration Plan

1. Ship the redactor and registration changes, docs, and the explain flag in one CLI release. takod is not involved.
2. Release notes call out the BREAKING marker change and `TAKO_REDACTION_LABELS=off`.
3. Rollback: downgrading the CLI restores the old masks. No state is involved.

## Resolved questions

1. **Should JSON (machine) output keep a plain `[REDACTED]` while human output is labeled?** Decision: no, label both. Event `message` and `data` strings pass through one function; splitting them needs a second pass and lets them disagree, and consumers detect masks with the documented regex (D10).
2. **Keep the partial reveal behind a flag?** Decision: no, and its removal is required, not cosmetic. It leaks 2–4 characters and the approximate length of every secret, and the label replaces its only use.
3. **Should `--redaction` also print the normal explain sections?** Decision: no, report only. `config explain` without the flag is unchanged, and the report is usually wanted on its own.
4. **Should `config explain --redaction --resolve` invoke providers to confirm sourced keys exist?** Decision: not in this change. An explain command must not run vault CLIs; `tako secrets fetch` already previews sourced keys.
5. **Opt-out spelling: an env var only, or also a `--no-redaction-labels` global flag?** Decision: env var only (D6). It is operator policy, must reach hooks and SDK embedders, and a flag can be added later without breaking anything.
6. **Should an unreadable `envFile` in read-only commands warn or stay silent?** Decision: warn once, matching `secrets-unresolved`. A silent gap is what this change exists to remove.

Review additions (flaws found and fixed in this design):

- **Public values no longer shield secrets that contain them (D4).** One length-ordered pass over secrets and public values replaces protect-then-replace, which leaked the rest of any secret that starts with or contains a project or service name. A secret stays visible only when it lies entirely inside a longer public value.
- **Only markers this redactor could have produced are protected (D5).** Protecting every grammar match let marker-shaped text shield a secret inside the brackets.

## Implementation notes

Deviations and details settled while implementing, each the safest equivalent of the plan:

- **`builds.` label kind.** The `deploy.go:240` call site registers every shared `builds.<name>.args` value, not `secrets:` values. No D1 kind fits, so it is labeled `builds.<build>.args.<ARG>` and ranked after `services.`.
- **`backup.storage.sessionToken`** is registered alongside `accessKeyId` and `secretAccessKey`; it is the same kind of credential.
- **Pattern rendering.** Each pattern names the submatch that holds the secret. Everything before it is kept, so `password: x` keeps its space (`password: [REDACTED:pattern.assignment]`) and connection strings mask only the password (`postgres://app:[REDACTED:pattern.connection-string]@db`). A pattern value that consists only of protected markers (plus closing punctuation) is left alone, which keeps `KEY=[REDACTED:secrets.KEY]` intact and redaction idempotent. A protected public value is not a marker, so `POSTGRES_PASSWORD=postgres` is still masked as `pattern.assignment` when a service is named `postgres`.
- **Invalid `envFile` lines.** `LoadEnvFile` warns about a line without `=` with the line number and path only, never the line content: the warning goes straight to stderr, bypassing the redactor, and is reached from read-only commands and `config explain --redaction`.
- **Overlapping secrets** that only partly overlap are merged into one marker covering their union, labeled by the longer secret, so no fragment of either leaks.
- **Labels() registry.** Marker protection uses every label ever registered with the redactor, not only the winning label of each value.
- **`exec`** registers the selected service's envFile values in both attach and one-off modes; `logs` registers envFile values for the selected services (the warning names the service).
- **`backup` commands** do not route output through the engine redactor today, so there is no backup-command call site to add; deploy, destroy, logs, rollback (both forms), promote, scale, remove, run and hooks register backup credentials.
- **Unreadable envFile in the report** is shown inline as `envFile unreadable: <path>; its values will not be redacted` without the underlying error text, which could quote file content. Config validation already rejects a missing envFile, so this only occurs for files that become unreadable.
- **Local secret keys** are read with a new `secrets.LocalSecretKeys`, which never creates `.tako/`, never writes `.gitignore` and never runs `$(command)` substitutions (unlike `NewManager`).
