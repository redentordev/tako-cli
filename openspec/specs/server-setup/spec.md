# server-setup Specification

## Purpose
Defines the safety guarantees `tako setup` provides when it changes a server's access controls (host firewall and sshd), so that provisioning never locks the operator out, never silently weakens existing settings, and reports exactly what it changed.

## Requirements

### Requirement: Firewall allows the active SSH port
When setup configures the host firewall, it SHALL allow inbound TCP on every SSH port in the SSH port set: `22`, the server's configured `port` (default 22), and the server-side port of the SSH session setup is running over. Duplicate ports SHALL be collapsed.

#### Scenario: Custom configured SSH port
- **WHEN** a server is configured with `port: 2222` and setup configures UFW
- **THEN** inbound `2222/tcp` is allowed
- **AND** inbound `22/tcp` is still allowed

#### Scenario: Live session port differs from configured port
- **WHEN** the configured `port` is `2200` but the SSH session reports server-side port `22` (for example, behind provider port forwarding)
- **THEN** both `2200/tcp` and `22/tcp` are allowed

#### Scenario: Default port
- **WHEN** a server has no `port` configured and the session reports server-side port `22`
- **THEN** exactly one SSH allowance, `22/tcp`, is applied

### Requirement: SSH allowance precedes firewall enablement
Setup SHALL enable the host firewall only after it has confirmed that an allow rule exists for every port in the SSH port set. If any SSH allow rule cannot be applied or confirmed, setup MUST NOT enable the firewall and MUST fail the firewall step with an error naming the missing port.

#### Scenario: SSH rule fails to apply
- **WHEN** applying the allow rule for SSH port `2222` fails
- **THEN** the firewall is not enabled
- **AND** the firewall step is reported as failed with an error naming `2222/tcp`

#### Scenario: All SSH rules confirmed
- **WHEN** an allow rule is confirmed for every port in the SSH port set
- **THEN** setup proceeds to enable the firewall

### Requirement: Lockout detection after enabling the firewall
After enabling the host firewall, setup SHALL open a new SSH connection to the server's configured host and port. If that connection cannot be established, setup MUST disable the firewall over the existing session and fail the firewall step with an error stating that a lockout was averted.

#### Scenario: Probe connection succeeds
- **WHEN** the firewall is enabled and a new SSH connection to the configured host and port succeeds
- **THEN** the firewall step completes and the firewall remains enabled

#### Scenario: Probe connection fails
- **WHEN** the firewall is enabled and a new SSH connection to the configured host and port fails
- **THEN** setup disables the firewall using the existing session
- **AND** the firewall step fails with an error that says the firewall was disabled to avoid a lockout

### Requirement: Firewall configuration is idempotent and never removes SSH access
Re-running setup SHALL leave existing firewall allow rules in place, add any missing SSH port allowances, and MUST NOT delete or deny any port that was previously allowed for SSH.

#### Scenario: Re-run with unchanged configuration
- **WHEN** setup runs again on a server whose firewall already allows the SSH port set
- **THEN** no allow rule is removed and no duplicate rules are created

#### Scenario: SSH port changed between runs
- **WHEN** a server previously set up with `port: 22` is reconfigured to `port: 2222` and setup runs again
- **THEN** `2222/tcp` is allowed and the existing `22/tcp` allowance is kept

### Requirement: Stale SSH allowances are reported
Setup SHALL report any SSH allow rule that setup created and whose port is no longer in the SSH port set. It SHALL report the rule as a warning that includes the manual removal command, and SHALL list it in the setup result. It MUST NOT remove the rule itself.

#### Scenario: Stale SSH port after a port move
- **WHEN** the firewall carries a Tako SSH allowance for `2200/tcp` and the current SSH port set is `22` and `2222`
- **THEN** setup prints a warning naming `2200/tcp` with the command to remove it
- **AND** the node's firewall result lists `2200/tcp` under stale SSH ports
- **AND** the `2200/tcp` rule is still present after setup

### Requirement: Firewall outcome is reported accurately
The setup result for each node SHALL include a firewall report with the backend (`ufw` or `none`), the status (`configured` or `skipped`), the skip reason, and the SSH ports allowed. `firewallPorts` MUST list only ports that were actually applied. It MUST be omitted when the firewall step was skipped.

#### Scenario: UFW configured
- **WHEN** setup configures UFW on a Debian-family host
- **THEN** the node result's firewall report has backend `ufw` and status `configured`
- **AND** `firewallPorts` includes every SSH port in the SSH port set, `80/tcp`, `443/tcp`, `443/udp`, and the mesh UDP port

#### Scenario: Non-Debian host without UFW
- **WHEN** setup runs on a RHEL-family host where UFW is not installed
- **THEN** the firewall step completes with firewall status `skipped`, backend `none`, and a reason stating UFW is unavailable on that OS
- **AND** `firewallPorts` is omitted from the node result
- **AND** human output warns that Tako did not configure a host firewall and lists the ports the operator must open

#### Scenario: Native firewall detected
- **WHEN** UFW is skipped and firewalld is active on the host
- **THEN** the skip warning and the firewall report state that firewalld is active and was not modified by Tako

### Requirement: Brute-force protection covers the active SSH ports
When setup configures the fail2ban SSH jail, the jail SHALL apply bans to every port in the SSH port set rather than only the standard SSH port.

#### Scenario: Custom SSH port jail
- **WHEN** setup configures fail2ban for a server with `port: 2222`
- **THEN** the sshd jail bans on ports `22` and `2222`

### Requirement: sshd changes are validated before reload
Setup SHALL validate the full sshd configuration after making its hardening edits and before reloading sshd. If validation fails, setup MUST restore the sshd configuration that was in place before its edits, MUST NOT reload or restart sshd, and MUST fail the hardening step with the validator output.

#### Scenario: Valid configuration
- **WHEN** hardening edits produce a configuration that passes sshd validation
- **THEN** sshd is reloaded and the hardening step completes

#### Scenario: Invalid configuration
- **WHEN** hardening edits produce a configuration that fails sshd validation
- **THEN** the previous sshd configuration is restored
- **AND** sshd is not reloaded or restarted
- **AND** the hardening step fails with an error containing the validation output

### Requirement: Password authentication is disabled only when key access is proven
Setup SHALL disable SSH password authentication only after it has proven that a new connection as the configured user succeeds with key or agent authentication alone. If that cannot be proven, setup MUST leave password authentication unchanged and print a warning.

#### Scenario: Operator connects with a key
- **WHEN** a new connection using only key or agent authentication succeeds
- **THEN** setup disables password authentication

#### Scenario: Operator connects with a password only
- **WHEN** the server is configured with `password` and a key/agent-only connection fails
- **THEN** password authentication is left unchanged
- **AND** setup warns that password authentication was kept enabled because key-based access could not be verified

### Requirement: Root login policy is not weakened
Setup SHALL set `PermitRootLogin prohibit-password` only when the sshd configuration has no active `PermitRootLogin` directive. It MUST NOT change an existing active `PermitRootLogin` value.

#### Scenario: Directive absent
- **WHEN** sshd_config has no active `PermitRootLogin` directive
- **THEN** setup sets `PermitRootLogin prohibit-password`

#### Scenario: Root login explicitly disabled
- **WHEN** sshd_config contains `PermitRootLogin no`
- **THEN** the directive remains `PermitRootLogin no` after setup

### Requirement: Setup documentation matches applied hardening
The `tako setup` help text and its man page SHALL describe the root-login policy that setup actually applies. They MUST NOT claim that setup disables root login. They SHALL state that UFW is configured only where available, and that the configured SSH port is allowed.

#### Scenario: Help text
- **WHEN** a user runs `tako setup --help`
- **THEN** the output states that root password login is blocked and that key-based root login is left allowed
- **AND** it does not contain the phrase "disable root login"
