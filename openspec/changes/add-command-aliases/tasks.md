# Tasks

## 1. Config model and validation (pkg/config)

- [ ] 1.1 Add `Aliases map[string]StringOrList` (`yaml:"aliases,omitempty" json:"aliases,omitempty"`) to `Config` in `pkg/config/types.go`; verify with a `types_test.go` case that YAML and JSON configs with `aliases` load under strict decoding and that a config without `aliases` produces an identical `Config`
- [ ] 1.2 Implement `SplitAliasString` in new `pkg/config/aliases.go` (whitespace, single quotes, double quotes with `\"`/`\\`, backslash escapes; errors for unterminated quote, trailing backslash, empty); verify with table tests in `pkg/config/aliases_test.go` including literal `;`, `|`, `&&`, `$(x)`, `*`
- [ ] 1.3 Implement `ValidateAliases` (name regex, static reserved names, non-empty first token, no leading `-`, no leading `tako`, no alias-to-alias, forbidden `--config`/`--host-key-mode` in all spellings before `--`) with `aliases.<name>` error paths, and call it from `ValidateConfig`; verify with table tests for every rule and the "flag after `--` is allowed" case
- [ ] 1.4 Add `validateRawAliases(data, isJSON)` next to the raw registry/ACME checks in `LoadConfig` rejecting `${` in alias values; verify a test where `${UNSET}` in an alias yields the alias error, not "missing environment variable(s)"
- [ ] 1.5 Implement `LoadAliases(path)` that reads only the `aliases` key (YAML node / JSON raw map), skips `.env`, env expansion and other-section validation, returns a typed not-found error; verify tests: config with an unrelated invalid section still returns aliases, missing file returns the not-found error, invalid alias returns its validation error
- [ ] 1.6 Verify `SaveConfig` / `config export` round-trip preserves list vs string form for aliases with a round-trip test

## 2. Schema and configuration docs

- [ ] 2.1 Add top-level `aliases` to `schema/tako.schema.json` (`propertyNames` pattern `^[a-z0-9][a-z0-9_-]{0,62}$`, `not` enum for reserved names, values `oneOf` string / array of strings with `minItems: 1`); verify with assertions added to `schema/tako_schema_test.go`
- [ ] 2.2 Add an "Command Aliases" section to `docs/CONFIGURATION.md` covering list vs string form, splitter rules, extra-arg appending (including after `--`), `-e` pinning and conflicts, built-in precedence/shadow warnings, forbidden flags, no `${}` expansion, and `tako aliases`; verify each example parses with `tako validate` once groups 3-4 land (task 6.1)

## 3. Pre-dispatch resolution (cmd/root.go)

- [ ] 3.1 Implement `resolveAliasArgs(args)` in new `cmd/alias_dispatch.go`: pre-scan using value-taking flags derived from `rootCmd.PersistentFlags()`, built-in detection from `rootCmd.Commands()` names/aliases plus `help`/`completion`/`__complete*`, prefix/alias/suffix assembly; verify unit tests for `--config X`, `--config=X`, `-eX`, `-e X`, `-v`, `--`, unknown leading flag, built-in name, and built-in cobra alias (`certs list` path untouched)
- [ ] 3.2 Add cmd-side target checks (`checkAliasTargets`: visible top-level command, not `help`/`completion`/`takod`/`internal`/hidden) and shadow detection; verify unit tests including unknown target `deplyo` -> `InvalidRequestError`
- [ ] 3.3 Implement `-e/--env` pin conflict detection; verify tests for pinned-only, equal values, differing values (exit code 2 via `exitCodeForError`), and `-e` after `--` ignored
- [ ] 3.4 Wire `resolveAliasArgs` into `Execute()` (`cmd/root.go:121-132`) with `rootCmd.SetArgs`; missing config keeps cobra's unknown-command error plus the "no tako.yaml/tako.json found for project aliases" hint; invalid aliases -> exit 2; `-v` prints the expansion to stderr only; verify with an in-process test that runs `tako logsw --follow` against a temp config and asserts the dispatched command and args equal `logs web --follow`
- [ ] 3.5 Add a test that an alias to `env pull` with `--output json` returns the human-only rejection (exit 2) and an alias to `ps` keeps the full contract (no extra stdout); also assert that `--events ndjson` through an alias emits no alias-specific event

## 4. Help and completion

- [ ] 4.1 Register runtime-only placeholder commands and the `commands`/`aliases` groups (plus `SetHelpCommandGroupID`/`SetCompletionCommandGroupID`) only when aliases are loaded on help, no-arg, `__complete*`, or alias paths. Add both groups with `rootCmd.AddGroup` before assigning any `GroupID` and before `ExecuteContext` (design D7); verify a test that `tako --help` output is byte-identical with no config and lists "Project Aliases:" with expansions when aliases exist, and that no group-not-found error occurs
- [ ] 4.2 Verify dynamic completion: test `rootCmd` `__complete ""`/`__complete con` output includes alias names with descriptions, and is unchanged (no error) when the config is missing or invalid; add a test that `__complete logsw --` returns no target-command flags and no error- [ ] 4.3 Run `go test ./cmd -run TestTrackedCompletionsMatchGeneratedOutput` and confirm placeholders never reach generated completions; regenerate `completions/` only if the new `aliases` command changes them
- [ ] 4.4 Document in the `docs/CONFIGURATION.md` "Command Aliases" section that completion covers alias names only and that the target command's flags do not complete after an alias name; verify by review

## 5. `tako aliases` command

- [ ] 5.1 Add `engine.KindAliasList` and `AliasListResult`/`AliasEntry` types in `pkg/engine`; verify JSON field names (`name`, `args`, `form`, `target`, `shadowed`, `shadowedBy`) with a marshal test
- [ ] 5.2 Implement `cmd/aliases.go` (`tako aliases`): sorted human table via `humanOut()`, "no aliases defined" message, `emitResultDocument`, config-not-found -> exit 2, shadow/target findings; verify tests for human output, `--output json`, `--events ndjson` terminal result event, empty list, and missing config
- [ ] 5.3 Add `"tako aliases"` to `machineFullContractCommands` in `cmd/machine_coverage_test.go` and to the full-contract row of the command coverage table plus a short `AliasList` result-document entry and an "aliases inherit the target command's category" note in `docs/MACHINE-INTERFACE.md`; verify `go test ./cmd -run TestMachineCommandCoverage`
- [ ] 5.4 Add shadowed-alias warning and target error findings to `tako validate` (`cmd/validate.go`); verify a test that a `logs` alias yields `Valid: true` with one `warn` finding
- [ ] 5.5 Regenerate man pages with `make man` (adds `man/tako-aliases.1`, updates `man/tako.1` SEE ALSO); verify no alias placeholder pages appear when run in a directory whose `tako.yaml` defines aliases

## 6. Integration checks

- [ ] 6.1 Run `go build ./...` and `go test ./...` (Windows-only known failures excepted) and validate every docs/CONFIGURATION.md alias example with `tako validate` against a sample config
- [ ] 6.2 Manually run `tako console` (alias to `exec -it web -- sh`) against a dev node to confirm interactive exec through an alias keeps TTY behavior
