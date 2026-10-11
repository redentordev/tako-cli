# Spec Delta

## Purpose

Keeps user-facing documentation consistent with the shipped loader and runtime. Config snippets are machine-checked, and documented behavior claims match the code.

## ADDED Requirements

### Requirement: Enforced docs classify every YAML snippet
In each enforced doc (`docs/CONFIGURATION.md` and `docs/ORCHESTRATION-MODEL.md`), every fenced `yaml` block SHALL be preceded by a marker on its nearest preceding non-blank line. The marker is one of `<!-- tako:snippet complete -->`, `<!-- tako:snippet partial -->` or `<!-- tako:snippet skip: <reason> -->`. An unmarked block, an unknown mode, or a `skip` without a reason MUST fail the test suite.

#### Scenario: Unmarked block
- **WHEN** a contributor adds an unmarked ```yaml block to docs/CONFIGURATION.md
- **THEN** the docs snippet test fails and names the file and line

#### Scenario: Non-enforced doc
- **WHEN** an unmarked yaml block appears in a doc outside the enforced list
- **THEN** the block is ignored, but a marked block in that doc is still checked

### Requirement: Complete snippets validate as-is
A `complete` snippet SHALL pass the same load and validation path as `tako validate`, using a fixed set of placeholder environment variables, with zero errors and zero deprecation warnings.

#### Scenario: Complete config passes
- **WHEN** the harness validates a `complete` snippet with project, servers and environments
- **THEN** it passes, and any failure reports the doc line and the loader error

### Requirement: Partial snippets validate after merging into a base config
A `partial` snippet SHALL use real key paths from the config root. It MUST pass validation after being deep-merged onto a fixed base config containing one project, one server `production` and one environment `production` that targets that server. A top-level `services:` key MUST fail the test with a message telling the author to nest under `environments.<env>.services`.

#### Scenario: Service fragment
- **WHEN** a `partial` snippet declares `environments.production.services.web` only
- **THEN** it is merged onto the base config and validated

#### Scenario: Top-level services rejected
- **WHEN** a `partial` or `complete` snippet has a top-level `services:` key
- **THEN** the test fails and names `environments.<env>.services` as the fix

### Requirement: Documented behavior matches implementation
Documentation SHALL describe behavior the code exhibits. Specifically:
- redirects from `redirectFrom` are 308;
- services deploy one at a time in dependency order;
- human-only commands reject `--output json` and `--events ndjson`;
- `tako prometheus` output is consumed through a textfile collector, not `file_sd_configs`;
- example directories and their catalog entries describe the behavior they demonstrate.

#### Scenario: Redirect status
- **WHEN** docs/CONFIGURATION.md describes `redirectFrom`
- **THEN** it states 308 permanent redirects that preserve path and request method

#### Scenario: Deploy concurrency
- **WHEN** docs describe how services are rolled out
- **THEN** they state that services deploy sequentially in dependency order, and no section claims parallel service deploys are the default

#### Scenario: Example names match behavior
- **WHEN** the examples catalog in `examples/README.md` lists the dependency-ordering example
- **THEN** it links `examples/dependency-order/`, describes sequential dependency-ordered deploys, and no example directory or catalog entry is named or described as parallel deployment

#### Scenario: Machine output coverage
- **WHEN** README.md describes `--output json`
- **THEN** it says that commands with the machine contract support it, that the human-only set is listed in docs/MACHINE-INTERFACE.md, and it does not claim every command supports it

#### Scenario: Prometheus integration
- **WHEN** docs/RESOURCE-MONITORING.md shows a Prometheus integration
- **THEN** it writes `tako prometheus` output atomically to a `.prom` file in a node_exporter textfile-collector directory and scrapes node_exporter
- **AND** it does not use `file_sd_configs` for the metrics file
