# Spec Delta

## ADDED Requirements

### Requirement: Resource monitoring docs match collected data
`docs/RESOURCE-MONITORING.md` SHALL describe only the metrics, Prometheus series and features that Tako actually collects or ships. It SHALL describe:
- native collection in takod;
- the history retention and `tako metrics --since`;
- OS status reporting, and the fact that Tako never reboots or installs updates;
- the opt-in textfile export;
- removal of the legacy agent.

The repository SHALL NOT contain unreferenced monitoring agent scripts.

#### Scenario: Documented fields exist
- **WHEN** the docs list network I/O, disk I/O or `tako_network_*` series
- **THEN** takod's native sample and `tako prometheus` produce those values

#### Scenario: No planned item for shipped history
- **WHEN** the docs list planned features
- **THEN** metrics retention and historical queries are not listed as planned

#### Scenario: Dead scripts removed
- **WHEN** the repository is searched for `pkg/monitoring/agent.sh`, `agent_v2.sh` and `pkg/monitoring/tako-monitor.service`
- **THEN** none of those files exist
