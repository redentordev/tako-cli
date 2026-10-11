# Spec Delta

## ADDED Requirements

### Requirement: Setup does not install the legacy monitoring agent
`tako setup` SHALL NOT install the bash monitoring agent (`tako-monitor.sh` and `tako-monitor.service`) or the `bc` package for it. Once takod is installed and advertises `node.metrics-history-v1`, setup SHALL idempotently disable and remove an existing `tako-monitor` service and script. It SHALL leave the old metrics file and installed packages in place.

#### Scenario: Fresh server
- **WHEN** `tako setup` provisions a new server
- **THEN** no `tako-monitor` service or script is installed, and takod collects metrics natively

#### Scenario: Re-running setup on an older server
- **WHEN** `tako setup` runs on a server that still has `tako-monitor.service` enabled
- **THEN** setup stops and disables the service, removes its unit file and script, and reloads systemd
- **AND** running setup again reports nothing to remove

#### Scenario: Takod not ready
- **WHEN** takod on the server does not advertise `node.metrics-history-v1` after installation
- **THEN** setup leaves the legacy agent in place and reports why
