# Spec Delta

## ADDED Requirements

### Requirement: Resource monitoring guide documents takod alerting
`docs/RESOURCE-MONITORING.md` SHALL document takod-run alerting (service checks, host, container and certificate alerts) as an implemented feature rather than a planned one. Which host metrics the guide may claim is governed by the `add-server-status-history` requirement "Resource monitoring docs match collected data".

#### Scenario: Alerting is documented
- **WHEN** a reader looks for how to be alerted on high disk usage
- **THEN** the guide points to `notifications.alerts.host` instead of a hand-written shell script
