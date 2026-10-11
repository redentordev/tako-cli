# Proposal

## Why

Teams repeat the same long Tako invocations: `tako exec -it web -- bin/rails console`, `tako logs web --follow -e staging`, `tako exec web --oneoff -- php artisan migrate`. Kamal solves this with project-defined `aliases:` (`kamal console`). Tako has no equivalent: the strict loader rejects any unknown top-level key (`pkg/config/types.go:1274-1278`), and there is no place to record per-project shortcuts in Git next to `tako.yaml`.

## What Changes

- New optional top-level `aliases:` map in `tako.yaml` / `tako.json`: alias name -> Tako argument list (YAML/JSON list, preferred) or a single string split with a documented, shell-free quoting splitter.
- New invocation form `tako <alias> [extra args...]`. The alias expands to `tako <alias args...> <extra args...>` before cobra dispatch. `tako run` is not reused (it is the configless image deploy, `cmd/run.go:60`).
- Aliases expand only into Tako subcommand argv. No shell, no environment-variable expansion, no command chaining. Aliases cannot target other aliases, hidden/infrastructure commands, `help`, or `completion`, and cannot set `--config` or `--host-key-mode`.
- An alias may pin an environment with `-e/--env`; a differing `-e` on the command line is an invalid request (exit 2). Without a pinned env, the normal `-e`/default-environment rules apply.
- Built-in commands always win. Names reserved by cobra or Tako (`help`, `completion`, `__complete`, `__completeNoDesc`, `aliases`) are a config validation error; an alias name that equals a built-in command or built-in command alias is a `tako validate` warning (the alias is unreachable) rather than an error, so a future Tako release that adds a command never invalidates an existing `tako.yaml`.
- Aliases appear in `tako --help` under a "Project Aliases" group and in shell completion (dynamic via cobra's `__complete`) when a config is present. Generated man pages and tracked completion scripts stay project-independent.
- New command `tako aliases` that lists aliases and their expansions, with the full machine contract (`AliasList` result document).
- An invoked alias inherits the machine-interface category, flags, exit codes, and result document of the command it expands to.

Breaking changes: none. `aliases` is a new optional key; configs that validate today keep validating. CLI flags and existing result documents are unchanged. A `tako.yaml` that uses `aliases:` is rejected by older Tako versions with `unknown field "aliases"` (forward-compat only).

### Out of scope

- Shell aliases, multi-command chains, or running local programs (see sibling change `add-local-deploy-hooks` for local commands).
- Per-environment alias maps (`environments.<env>.aliases`); env selection is via `-e` or a pinned `-e` inside the alias.
- Parameter substitution/templating inside aliases (`$1`, `{service}`).
- User-global aliases outside the project config (e.g. `~/.config/tako`).
- Changing cobra's behavior or exit code for unknown commands when no alias matches.
- Any takod or node-side behavior; aliases are resolved entirely in the CLI.

## Capabilities

### New Capabilities
- `command-aliases`: project-defined command aliases in `tako.yaml` — definition syntax and validation, expansion and dispatch, environment interaction, discovery (`tako aliases`, help, completion), and safety limits.

### Modified Capabilities
<!-- none: no existing specs under openspec/specs/ -->

## Impact

- `pkg/config`: `Config.Aliases` field, alias validation, raw `${...}` check, lightweight `LoadAliases` reader, string splitter.
- `cmd/root.go`: pre-dispatch alias resolution in `Execute()`, runtime help group and completion placeholders.
- New `cmd/aliases.go` (`tako aliases`), `pkg/engine` `AliasList` result kind.
- `cmd/validate.go`: shadowed-alias warnings.
- `schema/tako.schema.json` and `schema/tako_schema_test.go`; `docs/CONFIGURATION.md`, `docs/MACHINE-INTERFACE.md`; `man/` (new `tako-aliases.1`); `completions/` regeneration; `cmd/machine_coverage_test.go`.
- No new dependencies; no takod capability.
