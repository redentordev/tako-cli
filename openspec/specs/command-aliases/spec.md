# command-aliases Specification

## Purpose
Lets a project define named shortcuts for Tako invocations in `tako.yaml`, so `tako <alias>` expands to a full Tako command line without a shell, while built-in commands, machine output, and configuration compatibility stay unchanged.

## Requirements

### Requirement: Aliases are declared in an optional top-level map
The configuration SHALL accept an optional top-level `aliases` map in both YAML and JSON form. Each key is an alias name and each value is either a list of strings (Tako arguments, used verbatim) or a single string (split into arguments). Configurations without `aliases` SHALL load exactly as before.

#### Scenario: List-form alias is accepted
- **WHEN** `tako.yaml` contains `aliases: { console: [exec, -it, web, --, bin/rails, console] }`
- **THEN** `tako validate` succeeds
- **AND** the alias `console` is recorded with those six arguments in order

#### Scenario: Config without aliases is unaffected
- **WHEN** a `tako.yaml` that validated before this change is validated
- **THEN** it still validates with identical findings

#### Scenario: Non-string value is rejected
- **WHEN** an alias value is a number, a map, or a list containing a non-string item
- **THEN** config loading fails with an error naming `aliases.<name>`

### Requirement: Alias names follow a fixed format
Alias names MUST match `^[a-z0-9][a-z0-9_-]*$` and be at most 63 characters. Any other name SHALL be a config validation error that names the alias.

#### Scenario: Invalid name is rejected
- **WHEN** `tako.yaml` declares an alias named `Console` or `-x` or `db:shell`
- **THEN** config loading fails with an error naming the alias and the allowed format

#### Scenario: Valid name with digits, hyphen and underscore
- **WHEN** `tako.yaml` declares an alias named `db_shell-2`
- **THEN** config loading succeeds

### Requirement: Reserved alias names are rejected
The names `help`, `completion`, `__complete`, `__completeNoDesc`, and `aliases` SHALL be rejected as alias names with a config validation error.

#### Scenario: Reserved name
- **WHEN** `tako.yaml` declares an alias named `help`
- **THEN** config loading fails with an error stating that `help` is reserved

### Requirement: String-form values are split without a shell
A string-form alias value SHALL be split into arguments on unquoted whitespace, honoring single quotes (literal), double quotes (with `\"` and `\\` escapes), and backslash escapes outside quotes. No variable expansion, globbing, command substitution, or operator interpretation SHALL occur. An unterminated quote or an empty result SHALL be a validation error.

#### Scenario: Quoted segment stays one argument
- **WHEN** an alias is `console: exec -it web -- sh -c "bin/rails console"`
- **THEN** the alias expands to `exec`, `-it`, `web`, `--`, `sh`, `-c`, `bin/rails console`

#### Scenario: Shell operators are literal
- **WHEN** an alias string contains `;`, `|`, `&&`, `$(...)`, or `*`
- **THEN** each is passed to Tako as a literal argument and no shell is invoked

#### Scenario: Unterminated quote
- **WHEN** an alias string is `exec web -- echo "hi`
- **THEN** config loading fails with an error naming the alias and the unterminated quote

### Requirement: Alias values target a built-in Tako command
The first argument of an alias value MUST name a visible top-level Tako command. It MUST NOT start with `-`, be `tako`, be another alias name, or name `help`, `completion`, a hidden command, or an infrastructure command. Violations SHALL be reported as validation errors.

#### Scenario: Alias referencing another alias
- **WHEN** `aliases: { a: [logs, web], b: [a, --follow] }` and `tako b` is invoked
- **THEN** the command fails with exit code 2 and an error stating that aliases cannot reference other aliases

#### Scenario: Leading tako token
- **WHEN** an alias value is `tako exec web -- env`
- **THEN** validation fails with an error telling the user to omit the leading `tako`

#### Scenario: Unknown target command
- **WHEN** an alias value starts with `deplyo` and the alias is invoked
- **THEN** the command fails with exit code 2 and an error naming the alias and the unknown command `deplyo`

### Requirement: Aliases cannot change config source or SSH trust
An alias value MUST NOT contain `--config` or `--host-key-mode` in any spelling (`--flag value` or `--flag=value`) before a `--` separator. Such aliases SHALL be validation errors.

#### Scenario: Alias tries to downgrade host key checking
- **WHEN** an alias value is `[--host-key-mode, tofu, deploy]` or `[deploy, --host-key-mode=tofu]`
- **THEN** config loading fails with an error naming the alias and the forbidden flag

#### Scenario: Forbidden flag after separator is remote data
- **WHEN** an alias value is `[exec, web, --, mytool, --config, /etc/app.conf]`
- **THEN** the alias is accepted because the flag follows `--`

### Requirement: Alias values are not environment-expanded
Alias values SHALL be taken literally from the raw config file. A value containing `${` SHALL be a validation error, reported before environment expansion so a missing variable does not mask it.

#### Scenario: Env reference in alias
- **WHEN** an alias value contains `${SERVICE}` and `SERVICE` is unset
- **THEN** config loading fails with an error stating that aliases do not support `${...}` references, not a missing-variable error

### Requirement: Invoking an alias runs the expanded Tako command
When the first positional argument of `tako` is not a built-in command and matches an alias, Tako SHALL run the command line formed by global flags given before the alias name, then the alias arguments, then all arguments given after the alias name, unchanged and in order.

#### Scenario: Extra arguments are appended
- **WHEN** `aliases: { logsw: [logs, web] }` and the user runs `tako logsw --follow`
- **THEN** Tako behaves exactly as `tako logs web --follow`

#### Scenario: Global flags before the alias are kept
- **WHEN** the user runs `tako --config deploy/tako.yaml -v logsw`
- **THEN** aliases are read from `deploy/tako.yaml`
- **AND** Tako behaves exactly as `tako --config deploy/tako.yaml -v logs web`

#### Scenario: Extra arguments after a separator reach the remote command
- **WHEN** `aliases: { rails: [exec, -it, web, --, bin/rails] }` and the user runs `tako rails db:migrate:status`
- **THEN** Tako behaves exactly as `tako exec -it web -- bin/rails db:migrate:status`

### Requirement: Built-in commands take precedence over aliases
A first positional argument that names a built-in command or a built-in command alias SHALL always dispatch to that command, without reading the config for aliases. An alias whose name collides with a built-in SHALL NOT be a load error; `tako validate` and `tako aliases` SHALL report it as a warning that the alias is unreachable.

#### Scenario: Shadowed alias
- **WHEN** `tako.yaml` declares an alias named `logs` and the user runs `tako logs web`
- **THEN** the built-in `tako logs` runs
- **AND** `tako validate` succeeds with a warning finding that alias `logs` is shadowed by a built-in command

#### Scenario: Built-in commands do not depend on alias parsing
- **WHEN** the config contains an invalid `aliases` section and the user runs `tako --version` or `tako completion bash`
- **THEN** that command's behavior is unchanged by the invalid aliases

#### Scenario: Config-loading commands validate aliases
- **WHEN** the config contains an invalid `aliases` section and the user runs `tako deploy` or `tako validate`
- **THEN** config loading fails with the alias validation error, as for any other invalid config section

### Requirement: Aliases may pin an environment
An alias MAY include `-e`/`--env` before any `--` separator. If the invocation also supplies `-e`/`--env` with a different value, Tako SHALL fail with exit code 2 before running the target command. An identical value SHALL be accepted. Without a pinned env, the existing `-e` and default-environment rules SHALL apply.

#### Scenario: Pinned env used
- **WHEN** `aliases: { stg-console: [exec, -it, web, -e, staging, --, bin/rails, console] }` and the user runs `tako stg-console`
- **THEN** the exec targets the `staging` environment

#### Scenario: Conflicting env
- **WHEN** the user runs `tako -e production stg-console`
- **THEN** Tako exits with code 2 and an error stating that alias `stg-console` pins environment `staging`

#### Scenario: Unpinned alias follows -e
- **WHEN** `aliases: { logsw: [logs, web] }` and the user runs `tako -e staging logsw`
- **THEN** logs are read from the `staging` environment

### Requirement: Alias resolution errors are reported clearly
If the first positional argument is neither a built-in command nor resolvable as an alias, Tako SHALL fail as an unknown command. When no config file exists, the error SHALL state that no config was found to resolve aliases. When the config exists but cannot be read or its `aliases` section is invalid, Tako SHALL fail with exit code 2 and the config error.

#### Scenario: No config file
- **WHEN** no `tako.yaml` or `tako.json` exists and the user runs `tako console`
- **THEN** Tako fails with an unknown-command error that mentions no config file was found for project aliases

#### Scenario: Unknown name with config present
- **WHEN** `tako.yaml` exists, defines no alias `consle`, and the user runs `tako consle`
- **THEN** Tako fails with cobra's unknown-command error for `consle`

#### Scenario: Invalid aliases section
- **WHEN** `tako.yaml` has an alias with an unterminated quote and the user runs any alias
- **THEN** Tako exits with code 2 and an error naming the offending alias

### Requirement: Aliases inherit the target command's machine contract
An invoked alias SHALL behave, for `--output json`, `--events ndjson`, exit codes, human-only rejection, and project-mutation preflight, exactly as the expanded command. Alias expansion SHALL NOT emit additional stdout content in machine modes and SHALL NOT emit any alias-specific event.

#### Scenario: NDJSON stream has no alias event
- **WHEN** `aliases: { status: [ps] }` and the user runs `tako status --events ndjson`
- **THEN** the event stream is the same as for `tako ps --events ndjson`
- **AND** it contains no `alias.expanded` or other alias-specific event

#### Scenario: Alias to a full-contract command
- **WHEN** `aliases: { status: [ps] }` and the user runs `tako status --output json`
- **THEN** stdout contains exactly the `ps` result document

#### Scenario: Alias to a human-only command
- **WHEN** `aliases: { pull: [env, pull] }` and the user runs `tako pull --output json`
- **THEN** Tako exits with code 2 with the same human-only rejection as `tako env pull --output json`

### Requirement: Verbose mode reports the expansion
With `-v/--verbose`, Tako SHALL print the alias name and its full expanded argument list to stderr before running the target command.

#### Scenario: Verbose expansion
- **WHEN** the user runs `tako -v logsw --follow`
- **THEN** stderr contains a line showing `logsw` expanded to `logs web --follow`

### Requirement: Help lists project aliases
When a config with aliases is present, `tako --help` and `tako help` SHALL list aliases in a separate "Project Aliases" section with their expansions, after the built-in commands. `tako help <alias>` SHALL show the expansion. Help SHALL NOT fail when the config is missing or invalid.

#### Scenario: Help with aliases
- **WHEN** `tako.yaml` defines alias `console` and the user runs `tako --help`
- **THEN** the output lists `console` under "Project Aliases" with its expansion

#### Scenario: Help without config
- **WHEN** no config exists and the user runs `tako --help`
- **THEN** the output is identical to the help output without the aliases feature

### Requirement: Shell completion offers project aliases
Dynamic shell completion of the first argument SHALL include alias names from the active config, with their expansion as the description. Completion SHALL silently omit aliases when the config is missing or invalid. Completion of flags or arguments after an alias name is not provided; the target command's flags SHALL NOT be offered after an alias name.

#### Scenario: Completing an alias name
- **WHEN** `tako.yaml` defines alias `console` and the shell requests completions for `tako con`
- **THEN** the candidates include `console`

#### Scenario: No target-flag completion after an alias
- **WHEN** `tako.yaml` defines alias `logsw: [logs, web]` and the shell requests completions for `tako logsw --`
- **THEN** completion returns no candidates from `tako logs`' flags and does not fail

### Requirement: Generated artifacts are project-independent
Tracked man pages and completion scripts SHALL be generated without reading any project config, so they do not contain project aliases.

#### Scenario: Regenerating man pages in a project with aliases
- **WHEN** `make man` runs in a directory whose `tako.yaml` defines aliases
- **THEN** no man page is generated for any alias

### Requirement: The aliases command lists aliases
`tako aliases` SHALL list every alias with its name, arguments, target command, and whether it is shadowed by a built-in, sorted by name, without contacting servers or requiring environment variables referenced elsewhere in the config. It SHALL honor the full machine contract, emitting an `AliasList` result document with `--output json`.

#### Scenario: Human listing
- **WHEN** `tako.yaml` defines aliases `console` and `logsw` and the user runs `tako aliases`
- **THEN** stdout lists both names with their expanded commands, sorted by name

#### Scenario: JSON listing
- **WHEN** the user runs `tako aliases --output json`
- **THEN** stdout is one JSON document with `kind: AliasList`, `apiVersion`, `configPath`, and an `aliases` array whose entries contain `name`, `args`, `target`, and `shadowed`

#### Scenario: No aliases defined
- **WHEN** the config defines no aliases
- **THEN** `tako aliases` exits 0, the human output states that no aliases are defined, and the JSON `aliases` array is empty

#### Scenario: Missing config
- **WHEN** no config file exists and the user runs `tako aliases`
- **THEN** Tako exits with code 2 and a config-not-found error
