# Design

## Context

Observed behavior (current code):

- `main.go:8` calls `cmd.Execute()`, which calls `rootCmd.ExecuteContext(ctx)` (`cmd/root.go:121-132`). Cobra resolves the subcommand (`Find`, cobra v1.10.1 `command.go:757`) and, for an unmatched first argument on the root, returns `unknown command %q for "tako"` (`args.go:36`), which maps to exit code 1 via `exitCodeForError` (`cmd/root.go:138-159`).
- Config is NOT loaded before dispatch. `cobra.OnInitialize(initConfig)` (`cmd/root.go:242`) runs inside cobra's `preRun` after the target command and its flags are parsed (cobra `command.go:1048`), and `initConfig` (`cmd/root.go:316-345`) only feeds viper. Each command loads the typed config itself, e.g. `runExec` -> `config.LoadConfig(cfgFile)` (`cmd/exec.go:75`), `runValidate` -> `loadDeployConfig` (`cmd/validate.go:61`, `cmd/deploy.go:84-103`). So alias expansion cannot rely on existing config loading and must happen in `Execute()` before `ExecuteContext`.
- Global flags are persistent root flags: `--config`, `-v/--verbose`, `-e/--env`, `--host-key-mode`, `--output`, `--events` (`cmd/root.go:276-281`). `getEnvironmentName` uses `envFlag` or the config default (`cmd/root.go:349-360`).
- `LoadConfig` (`pkg/config/types.go:1195-1294`) loads `.env`, runs raw-content checks (`validateRawRegistryCredentials`, `validateRawACMEDNSCredentials`, `:1243-1248`), expands `${VAR}` over the whole file and fails on any missing variable (`expandEnvWithTrim`, `:1141-1169`), decodes strictly (`KnownFields(true)`, `:1274-1278`; JSON `DisallowUnknownFields`, `:1261`), then `ValidateConfig` (`pkg/config/validator.go:50`), platform inventory and cluster binding checks. A top-level `aliases:` key is rejected today as an unknown field. `Config` is at `pkg/config/types.go:23-54`.
- `config.StringOrList` (`pkg/config/string_or_list.go:21-146`) already models "string or list of strings" for YAML and JSON, preserving which form was used, and is exercised by schema tests (`schema/tako_schema_test.go:191-201`).
- Machine categories: `markHumanOnly` + `rejectMachineOutputForHumanOnly` run in root `PersistentPreRunE` against the dispatched command (`cmd/root.go:61-106`); `cmd/machine_coverage_test.go:124-176` walks `rootCmd` (without `Execute`) and requires every runnable command in exactly one category.
- Existing cobra command aliases: `certs ls` (`list`), `certs rm` (`remove`, `delete`) (`cmd/certs.go:45,54`). Hidden commands: `internal`, `platform worker ...`, `platform node upgrade-publication-guard` (`cmd/internal.go:15`, `cmd/platform.go:65-71`, `cmd/platform_upgrade_guard.go:22`). `takod run` is infrastructure (`cmd/machine_coverage_test.go:114-122`).
- Help: cobra's default usage template switches to "Additional Commands:" for ungrouped commands once any group exists (cobra `command.go:1956-1958`); `help`/`completion` are added lazily and get a group only via `SetHelpCommandGroupID`/`SetCompletionCommandGroupID` (`command.go:343-352`).
- Generated artifacts: man pages via `cmd.GenerateManPages` (`cmd/root.go:162-176`, `scripts/generate-man.go:19`, `Makefile:84-90`); completions are compared to `rootCmd.Gen*Completion` output in `cmd/completion_test.go:10-56`. Both build from `rootCmd` without going through `Execute()`.
- `schema/tako.schema.json` is hand-maintained (no generator; only `schema/tako_schema_test.go` and `cmd/init.go` reference it).

## Goals / Non-Goals

**Goals:**
- Resolve aliases before cobra dispatch with zero behavior change for built-in invocations.
- Read aliases cheaply and independently of env vars, `.env`, platform inventory, and cluster binding, so `tako console` errors only on alias problems.
- Keep static artifacts (man, completions, machine coverage) project-independent.

**Non-Goals:**
- No changes to takod, the engine request types, or any existing result document. No takod capability gate is needed because nothing node-side changes.
- No new third-party dependency (shell splitting is a small local function).

## Decisions

### D1. Invocation form: `tako <alias>`, resolved in `Execute()` by rewriting argv
`Execute()` (`cmd/root.go:121`) calls a new `resolveAliasArgs(os.Args[1:])` and, when it returns an expansion, `rootCmd.SetArgs(expanded)` before `ExecuteContext`. Because dispatch then sees an ordinary built-in command line, the alias automatically inherits the target's flags, `PersistentPreRunE` checks (human-only rejection, project-mutation attachment), result document and exit codes.
- Alternative `tako run-alias <name>` / `tako x <name>`: rejected; `tako run` is the configless image deploy (`cmd/run.go:60`) and a wrapper verb defeats the point of a shortcut.
- Alternative: register a real cobra command per alias whose `RunE` re-executes `rootCmd`: rejected; re-entrant `Execute` re-parses already-bound global flag variables and runs `PersistentPreRunE` twice.

### D2. Pre-scan of argv
`resolveAliasArgs` scans tokens left to right, stopping at `--`:
- Known root persistent flags are consumed with their values: `--config X|--config=X`, `-e X|-eX|--env X|--env=X`, `--output`, `--events`, `--host-key-mode` (value forms), `-v/--verbose` (bool). `-h/--help`, `--version` end the scan (no alias resolution).
- The first non-flag token is the candidate. If none, or it is a built-in (any `rootCmd.Commands()` `Name()` or `Aliases`, plus `help`, `completion`, `__complete`, `__completeNoDesc`), return no expansion and do not read the config. `__complete*` and `help` additionally trigger best-effort placeholder registration (D7).
- Any unknown flag before the candidate stops resolution (cobra will report it as today).
Output argv = `prefixTokens + aliasArgs + suffixTokens` where prefix is everything before the alias name and suffix is everything after it.

### D3. Config shape: `Aliases map[string]StringOrList` on `Config`
Add `Aliases map[string]StringOrList \`yaml:"aliases,omitempty" json:"aliases,omitempty"\`` to `Config` (`pkg/config/types.go:23`). This makes the strict decoders accept the key, round-trips through `SaveConfig`'s `yaml.Marshal` (`types.go:1367`) and `config export`, and reuses the existing YAML/JSON string-or-list parsing. List form is the documented preferred form; string form exists for Kamal-like one-liners.

### D4. String splitting (`pkg/config/aliases.go: SplitAliasString`)
POSIX-shell-like word splitting only: whitespace separates; `'...'` literal; `"..."` with `\"` and `\\` escapes; `\x` outside quotes yields `x`. No `$`, glob, `~`, or operator semantics. Errors: unterminated quote, trailing lone backslash, zero words. Rationale: no splitter dependency exists in `go.mod`; a small tested function keeps behavior fully specified. List form bypasses splitting entirely.

### D5. Validation split between `pkg/config` and `cmd`
- `pkg/config` (`ValidateAliases`, called from `ValidateConfig` at `validator.go:50` and from `LoadAliases`): name regex `^[a-z0-9][a-z0-9_-]{0,62}$`; static reserved names (`help`, `completion`, `__complete`, `__completeNoDesc`, `aliases`); value parse/split; first token not empty, not `-`-prefixed, not `tako`, not another alias name (no recursion — single-level expansion, so cycles are impossible); forbidden `--config`/`--host-key-mode` in any spelling before `--`. Error messages use the `aliases.<name>` path like `secretSources` does (`validator.go:77-84`).
- New raw check `validateRawAliases(data, isJSON)` alongside the existing raw checks (`types.go:1243-1248`) rejects `${` inside alias values before env expansion, so a missing variable cannot mask the real problem and literal-only semantics hold.
- `cmd` (needs the command tree): target must be a non-hidden top-level command other than `help`/`completion`/`takod`/`internal`; built-in name shadowing. Target errors fail alias invocation with `engine.InvalidRequestError` (exit 2) and are `error` findings in `tako validate` / `tako aliases` (via a shared `checkAliasTargets(rootCmd, aliases)` helper). Full `LoadConfig` callers other than `validate` do not check targets, since `pkg/config` cannot see the command tree. Shadowing is never an error: built-ins win in D2, and `tako validate` / `tako aliases` add a `warn` finding. Rationale: if a future Tako adds `tako console`, an existing project's `console` alias must not turn a valid `tako.yaml` invalid (project compatibility rule); it becomes unreachable with a visible warning instead.

### D6. Lightweight loader `config.LoadAliases(path)`
Resolves the path like `resolveDeployConfigPath` (`cmd/deploy.go:92-103`), reads the file, runs `validateRawAliases`, decodes only the `aliases` key (YAML: unmarshal to `yaml.Node`, pick the top-level mapping entry; JSON: `map[string]json.RawMessage`), and runs `ValidateAliases`. It does not load `.env`, expand env, validate other sections, or touch platform state. Returns a typed not-found error so `Execute()` can produce the "no config file found for project aliases" hint while keeping cobra's unknown-command wording. Parse/validation errors are wrapped in `engine.InvalidRequestError` (exit 2). Trade-off: an alias can be invoked while another section is invalid; the expanded command then loads the full config and fails with that section's error, which is correct.

### D7. Help and completion via runtime-only placeholder commands
After loading aliases (only when the candidate is not a built-in, or for `help`/no-arg/`__complete*`), `Execute()` registers one placeholder `cobra.Command` per non-shadowed alias: `Use: name`, `Short: "alias for: tako <args>"`, `GroupID: "aliases"`, `DisableFlagParsing: true`, and a `RunE` that returns an internal error (never reached, because argv is already rewritten). It adds groups `commands` ("Available Commands:") and `aliases` ("Project Aliases:"), assigns `GroupID: "commands"` to every built-in child and calls `SetHelpCommandGroupID`/`SetCompletionCommandGroupID("commands")`, so built-ins keep their current heading (cobra `command.go:1956-1958`). With no aliases, nothing is registered and output is byte-identical. Cobra's dynamic `__complete` then lists aliases as subcommand candidates with their `Short` as description. Because registration happens only in `Execute()`, `GenerateManPages`, `completion_test.go`, and `machine_coverage_test.go` never see placeholders. Failures loading aliases on these paths are silent (help/completion must not break).
- Alternative: root `ValidArgsFunction` for completion plus a custom usage template: rejected as two mechanisms for one concept; placeholders give `tako help <alias>` for free.
- *Verified (resolved assumption):* cobra v1.10.1 `__complete` on the root lists available (non-hidden) children, including grouped placeholders with `DisableFlagParsing` (`completions.go:512-518`, `IsAvailableCommand`). Two constraints follow:
  - Both groups (`commands` and `aliases`) MUST be added with `rootCmd.AddGroup` before `ExecuteContext` runs, and before any child is assigned a `GroupID`. Cobra panics/errors on a `GroupID` that names an unregistered group, so registration order in `Execute()` is: `AddGroup(commands, aliases)` → assign built-in `GroupID`s and `SetHelpCommandGroupID`/`SetCompletionCommandGroupID` → add placeholders → `ExecuteContext`.
  - Completion only covers the alias *name*. Because placeholders use `DisableFlagParsing` and have no flags or `ValidArgsFunction`, `tako console --<TAB>` or `tako logsw <TAB>` offers nothing from the target command. This is documented in `docs/CONFIGURATION.md` as a known limitation; it is accepted rather than proxying completion to the target.

### D8. Environment pinning
After expansion, scan the alias tokens (before `--`) for `-e/--env` and the user prefix/suffix tokens for the same. If both present with different values, return `InvalidRequestError` ("alias stg-console pins environment staging; remove -e production or use the target command directly"). Equal values pass. Pinned-only works naturally because pflag sees one `-e`. This keeps Kamal's "alias includes a destination" without silent last-flag-wins surprises.

### D9. `tako aliases` command (full machine contract)
New `cmd/aliases.go`, read-only, local. Uses `LoadAliases` + the cmd-side checks. Result `engine.AliasListResult{APIVersion: takoapi.APIVersionCurrent, Kind: engine.KindAliasList, ConfigPath, Aliases []AliasEntry{Name, Args, Form("list"|"string"), Target, Shadowed bool, ShadowedBy string}, Findings []ValidateFinding}`, emitted via `emitResultDocument` (`cmd/engine.go:68-84`), human table via `humanOut()` (`cmd/engine.go:58-63`). Full contract chosen over human-only because it is cheap, side-effect free, and lets control planes discover project shortcuts. Not project-mutating, so not added to `markProjectMutating` (`cmd/root.go:262-267`). Aliases are literal config text with `${` rejected, so no secret values are resolved into the output; a user who hardcodes a secret literal in an alias already has it committed in `tako.yaml`.

### D10. Verbose trace
With `-v` in the prefix or suffix, `Execute()` prints `alias <name> -> tako <expanded argv>` to stderr. Nothing is written to stdout and no event is emitted, so machine consumers see exactly the target command's output.
- *Resolved:* there is no `alias.expanded` NDJSON event, now or as planned follow-up. The NDJSON stream of an alias invocation is exactly the target command's stream. Control planes that need the mapping call `tako aliases --output json`.

## Risks / Trade-offs

- [Pre-scan diverges from cobra's flag parsing (e.g. a future new persistent flag)] → Derive the value-taking flag set from `rootCmd.PersistentFlags()` at runtime (`NoOptDefVal == ""` means it takes a value) instead of hard-coding; unit-test against the real flag set.
- [Extra file read on alias/help/completion paths] → Built-in invocations never read the config (D2); alias path reads one file once.
- [Silent help/completion when aliases are invalid hides mistakes] → `tako validate` and `tako aliases` surface the error; invoking the alias returns it with exit 2.
- [Shadowed aliases become dead after a Tako upgrade] → Warning finding in `tako validate`/`tako aliases`; documented in CONFIGURATION.md.
- [Committed alias could weaken host-key verification or redirect config] → `--config`/`--host-key-mode` forbidden in aliases (D5).
- [Older Tako rejects `aliases:`] → Documented as requiring the release that ships this change; no fallback.

## Migration Plan

Additive. No migration for existing configs. Rollback = remove `aliases:` from `tako.yaml` (older CLIs reject the key). No takod rollout or capability negotiation involved.
