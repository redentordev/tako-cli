# Proposal

## Why

`tako setup` enables UFW with `default deny incoming` but only ever allows `22/tcp` for SSH (`pkg/provisioner/provisioner.go:1296-1321`). A server whose `servers.<name>.port` is not 22 is cut off from new SSH connections the moment setup runs `ufw --force enable`: the current session survives on conntrack, but every later `tako` command, and the operator's own `ssh`, cannot connect. Setup's SSH hardening has related gaps. It restarts sshd without validating the config, it disables password authentication even when Tako itself connected with a password, it changes an explicit `PermitRootLogin no` to `prohibit-password`, and the help text says it "disables root login", which it does not. Because these steps run unattended against production hosts, each gap can lock the operator out, quietly weaken the host, or mislead the operator.

## What Changes

- The firewall step always allows the SSH port Tako is connected on: the configured `servers.<name>.port` and the server-side port reported by the live session's `SSH_CONNECTION`. It keeps allowing `22/tcp` as it does today. It verifies that the SSH allow rules are in place before it enables UFW. If they are not, it refuses to enable UFW and fails the step.
- After enabling UFW, setup opens a fresh probe SSH connection to the configured host:port. If the probe fails, setup disables UFW over the still-open session and fails the step with a lockout-averted error.
- Setup never deletes SSH allow rules. When the SSH port changes between runs, the old port stays open. Setup reports SSH allow rules that Tako created and that no longer match the current port set as stale, and the warning includes the manual removal command.
- The fail2ban `sshd` jail bans on the resolved SSH ports instead of the fixed `ssh` (22) service name.
- Firewall reporting is accurate. The setup result node gains an additive `firewall` object (`backend`, `status` `configured|skipped`, `reason`, `sshPorts`, `staleSshPorts`, `nativeFirewall`). `firewallPorts` is populated only when UFW was actually configured. Human output prints a clear warning when the firewall step is skipped on a non-Debian host without UFW, and names firewalld when it is active.
- SSH hardening safety:
  - Setup backs up `/etc/ssh/sshd_config` before editing it and validates the result with `sshd -t`. On failure it restores the backup and fails the hardening step without reloading sshd. It reloads sshd instead of restarting it.
  - Setup disables `PasswordAuthentication` only after a key/agent-only probe connection succeeds. Otherwise it leaves password auth unchanged and warns.
  - Setup no longer rewrites an explicit `PermitRootLogin no`. It still sets `prohibit-password` when the directive is absent or commented out.
- The setup manifest version goes from `1.2.1` to `1.2.2`. The next `tako setup` on an existing server then reapplies hardening, which picks up the validated sshd reload and the fail2ban port fix. The firewall fix applies on any `tako setup` re-run, even without the version bump.
- `tako setup` help text, `man/tako-setup.1`, README, and docs describe the real behavior: root password login is blocked and key-based root login is allowed.
- **Result document change (additive, one semantic narrowing):** `SetupNodeResult.firewall` is new. `firewallPorts` is now omitted when the firewall step was skipped, where it used to list ports that were never applied. It can also include additional SSH ports. There are no tako.yaml or CLI flag changes.

### Out of scope

- firewalld, nftables, or iptables management on RHEL/SUSE/Alpine. Setup detects and reports these but does not change them.
- An opt-in setting to fully disable root SSH login, or any new tako.yaml field. This is a possible follow-up.
- Closing port 22 or deleting any firewall rule automatically.
- Changing the sshd listen port, handling `sshd_config.d` drop-in precedence, or socket-activated `ssh.socket` port changes.
- Changes to `tako upgrade servers`, which does not touch the firewall or sshd, and to any takod or node-agent behavior.

## Capabilities

### New Capabilities
- `server-setup`: safety guarantees `tako setup` provides when it changes host access controls: the firewall SSH allowance, lockout detection, sshd change validation, firewall reporting, and accurate setup documentation.

### Modified Capabilities
- None. There are no existing specs.

## Impact

- Code: `pkg/provisioner/provisioner.go` (`ConfigureFirewall`, `FirewallAllowedPorts`, `firewallAllowCommands`, `HardenSecurity`, `fail2banSSHDJailConfig`), `cmd/setup.go` (step wiring, `collectSetupNodeFacts`, help text), `pkg/engine/setup.go` (result types).
- Tests: `pkg/provisioner/firewall_test.go`, new hardening command tests, `cmd/setup_test.go` fakes, and the `cmd/machine_output_test.go` setup golden.
- Docs: `docs/MACHINE-INTERFACE.md`, `docs/SERVER-SETUP-VERSIONING.md`, `docs/CONFIGURATION.md` (server `port`), `docs/CI-CD.md`, `README.md`, and `man/tako-setup.1` (regenerated with `make man`).
- No schema, completions, takod capability, or tako.yaml changes.
