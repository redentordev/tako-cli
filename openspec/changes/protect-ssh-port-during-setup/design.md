# Design

## Context

Observed behavior today. Line numbers are from the current branch.

- **Connection.** `runSetupNode` connects once through the pool using `server.Host, server.Port, server.User, server.SSHKey, server.Password` (`cmd/setup.go:102`). `server.Port` defaults to 22 during validation (`pkg/config/validator.go:699-701`). Setup never switches users. `SetupDeployUser(server.User)` ensures that the *same* user exists and is in the takod access group (`cmd/setup.go:222`, `pkg/provisioner/provisioner.go:1499-1535`). No key is copied, so there is no separate "deploy user" whose authentication needs verifying.
- **Auth order.** `ssh.NewClientWithAuth` tries the key file, then the agent, then the password as a fallback (`pkg/ssh/client.go:255-287`). Setup cannot tell which method succeeded.
- **Firewall step** (`pkg/provisioner/provisioner.go:1211-1285`):
  - If `ufw` is missing and the OS family is not Debian, the step logs a message and returns nil (`:1216-1225`, `shouldSkipUFWFirewall` `:1287-1292`). If the OS is Debian or unknown, it installs ufw with apt.
  - If UFW is active, it runs `ufw --force disable`. If UFW is inactive, it runs `ufw --force reset` (`:1240-1250`). It then sets `default deny incoming`.
  - The allow commands come from `firewallAllowCommands` (`:1306-1321`). They contain a hard-coded `22/tcp` and end in `|| true`, and their errors are ignored (`:1266-1270`). The step then runs `ufw --force enable` (`:1273`).
  - Nothing verifies that the SSH rule exists, and nothing checks reachability afterwards.
- **Firewall reporting.** `FirewallAllowedPorts` (`:1296-1304`) is computed independently in `collectSetupNodeFacts` (`cmd/setup.go:175`). Because of that, `firewallPorts` is reported even when the firewall step skipped UFW, and the step title always says "(UFW)" (`cmd/setup.go:219`).
- **Converge.** The firewall step runs in every setup mode, including converge (`cmd/setup.go:219`, `skip=false`). Hardening runs only in fresh and reapply mode (`cmd/setup.go:220`). `tako upgrade servers` never touches the firewall or sshd (`cmd/upgrade.go:156-287`).
- **Hardening** (`pkg/provisioner/provisioner.go:1324-1423`):
  - The fail2ban jail uses `port = ssh` (`:1471-1488`), so a custom port is never banned.
  - The sshd edits are individual `sed`/`tee` calls whose errors are ignored (`:1358-1390`). There is no backup and no `sshd -t`.
  - Password auth is turned off only when a `PasswordAuthentication yes` line exists, active or commented (`:1378-1379`). This happens even when Tako authenticated with a password.
  - `PermitRootLogin no` is rewritten to `prohibit-password` (`:1383`). An active setting is never left alone.
  - sshd is then *restarted* through both unit names, with errors ignored (`:1407-1408`).
- **Live session port.** `detectSSHClientIP` already reads `$SSH_CONNECTION` (`:1490-1496`). Its 4th field is the server-side port.
- **Help text.** "Security hardening (disable root login, fail2ban)" (`cmd/setup.go:34`, `man/tako-setup.1:19`).
- **Manifest.** `setup.CurrentVersion = "1.2.1"` (`pkg/setup/version.go:15`). A lower version on the server triggers reapply mode (`cmd/setup.go:121-125`).
- **Test seams.** `Provisioner` wraps a concrete `*ssh.Client` (`pkg/provisioner/provisioner.go:48-50`). Existing tests therefore exercise pure command-builder helpers (`pkg/provisioner/firewall_test.go`), and cmd-level tests use the `recordingSetupRefresher` fake (`cmd/setup_test.go:237-304`).

## Goals / Non-Goals

**Goals:**
- Make the firewall step fail closed with respect to SSH access, and make the hardening step fail safe with respect to sshd.
- Keep the changes testable through pure command builders and parsers plus an injectable probe. No live host is needed in unit tests.

**Non-Goals:**
- Restructuring setup into a declarative plan, adding new setup step keys, or adding takod involvement. All changes are CLI-side SSH operations. **No takod capability gate is needed**, because node-agent behavior does not change.

## Decisions

### D1. SSH port set = {22} ∪ {configured port} ∪ {live server-side port}
Add `ResolveSSHPorts(configuredPort int) ([]int, error)` on `Provisioner`. It parses field 4 of `$SSH_CONNECTION` with a pure `parseSSHConnectionServerPort`. A missing or invalid value is not an error, because the configured port still applies. The function returns a sorted, de-duplicated list. `runSetupNode` computes the set once and passes it to both the firewall and hardening steps.
- *Why keep 22 (resolved):* today 22 is always allowed, and removing it could close the operator's own fallback path in the middle of setup. Leaving 22 open when nothing listens on it is low risk. `22/tcp` stays in the set permanently; no later change is scheduled to remove it. Operators who want it closed delete the rule themselves.
- *Why include the live port:* behind provider NAT, the configured port, for example 2222 forwarded to 22, is not the port the local firewall sees.
- *NAT assumption (resolved, accepted):* provider port-forwarding setups where the configured port differs from the server-side port are assumed rare. Allowing both opens at most one extra non-listening port. Tako does not try to discover NAT mappings any further; the post-enable probe (D2) is the safety net for any mapping the port set misses.
- *Alternatives:* parsing `sshd -T` listen ports, which is more complete but fails on socket-activated `ssh.socket` and needs root. Allowing only the configured port misses NAT setups.

### D2. Firewall step: apply → confirm → enable → probe → (rollback)
Change the signature to `ConfigureFirewall(opts FirewallOptions) (*FirewallReport, error)`. `FirewallOptions` holds `MeshListenPort`, `SSHPorts`, and `Probe func() error`. The new sequence:
1. Keep the existing disable/reset and default-policy commands.
2. Apply SSH rules with `ufw allow <p>/tcp comment 'SSH (fail2ban protected)'`, keeping the existing comment so that existing rules update in place. Errors are checked: ufw exits 0 on "Skipping adding existing rule", so `|| true` is dropped for the SSH rules only.
3. Confirm the rules with `sudo ufw show added`. This lists rules even while UFW is inactive, unlike `ufw status`. A pure `parseUFWAddedRules` extracts the port, protocol, and comment. A missing SSH port returns an error *before* `ufw --force enable`.
4. Enable UFW.
5. Call `opts.Probe`. If it fails, run `sudo ufw --force disable` over the existing session and return `firewall disabled to avert SSH lockout: <probe error>`. The existing session survives the enable because UFW's `before.rules` accepts ESTABLISHED connections.
- *Probe construction (cmd side):* `ssh.NewClientWithAuth` (or the pinned variant when the server has a recorded host key), *not* the pool, because the pool returns the cached live client. The probe uses a bounded dial: 2 TCP attempts and a ≤10 s timeout. Without that, `connectTCPAttempts` defaults to up to 6 attempts with ~31-35 s backoff (`pkg/ssh/client.go:532-559`, `docs/CI-CD.md:174-181`), which could hang the step for minutes. This needs a small `ssh.ProbeOptions` or an attempts/timeout override on the client constructor. The probe closes its connection immediately.
- *Alternative rejected:* "ufw allow SSH only, no probe". This catches neither NAT/port mismatches nor a provider firewall interaction.

### D3. Never delete. Report stale SSH rules instead
Rules from `ufw show added` that carry the Tako SSH comment and whose port is not in the current set go into `FirewallReport.StaleSSHPorts`. Setup emits a warning: `sudo ufw delete allow <p>/tcp` once the new port is confirmed. The fresh-path `ufw --force reset` (`provisioner.go:1247-1250`) only runs when UFW is inactive, so it cannot close a port that is live. It is kept.
- *Alternative rejected:* automatically deleting old SSH ports. That would close the port the operator may still be using mid-migration.

### D4. Accurate firewall reporting; detect native firewall, do not manage it
`FirewallReport{Backend, Status, Reason, SSHPorts, AllowedPorts, StaleSSHPorts, NativeFirewall}` maps to a new additive `SetupNodeResult.Firewall *SetupFirewall` (`json:"firewall,omitempty"`) in `pkg/engine/setup.go`. `collectSetupNodeFacts` sets `FirewallPorts` from `report.AllowedPorts` instead of recomputing it (`cmd/setup.go:175`), so the field is empty, and therefore omitted, when the step is skipped. On skip, setup probes `systemctl is-active firewalld` and records `nativeFirewall: "firewalld"`. The human warning lists the ports to open, from the same `FirewallAllowedPorts(mesh, sshPorts)`. The step title becomes "Configuring firewall". The step key `firewall` is unchanged.
- *Why no firewalld management:* RHEL/SUSE hosts are commonly fronted by provider firewalls, and the existing code deliberately defers to them (`provisioner.go:1208-1210`). Managing firewalld zones (permanent vs runtime rules, zone selection) would double this change's surface. It is listed as a follow-up.

### D5. Hardening runs as one validated root script
Replace the ignored `sed` loop (`provisioner.go:1358-1390`) with a single script generated by a pure `sshdHardeningScript(opts)` and run through `runRootScript` (`:1186-1196`) with `set -eu`. The script:
1. Copies `/etc/ssh/sshd_config` to `/etc/ssh/sshd_config.tako-bak`.
2. Applies the MaxStartups, MaxSessions, ClientAlive*, and LoginGraceTime edits exactly as today.
3. Applies `PermitRootLogin prohibit-password` only when `grep -Eq '^[[:space:]]*PermitRootLogin[[:space:]]'` finds no active directive.
4. Disables password auth only when `opts.DisablePasswordAuth` is set.
5. Runs `sshd -t`, resolving `sshd` through `command -v sshd || /usr/sbin/sshd`. On failure it restores the backup, prints the validator output, and exits non-zero.

After that, `systemctl reload ssh || systemctl reload sshd` runs. Reload keeps existing sessions and avoids the double restart. The `systemctl enable` calls stay best-effort.
- *Why reload:* there is no change to the listen address or port, so reload is sufficient and less disruptive.
- *Caveat:* appending directives after a trailing `Match` block, which today's code also does, can produce an invalid config. Validation plus restore now turns that into a clear failure instead of a broken sshd.

### D6. Password-auth gate by key-only probe
`HardenSecurity(opts HardeningOptions)`, where `HardeningOptions` holds `SSHPorts` and `KeyOnlyProbe func() error`. The cmd side builds the probe as `NewClientWithAuth(host, port, user, server.SSHKey, "")`, which uses key or agent only and the same bounded dial as D2. Success sets `DisablePasswordAuth`. Failure keeps password auth and emits a warning. The probe runs before the script.

### D7. fail2ban jail ports
`fail2banSSHDJailConfig(clientIP string, sshPorts []int)` writes `port = 22,2222`, a comma-joined list, instead of `port = ssh`.

### D8. Setup manifest bump to 1.2.2
The firewall fix needs no bump, because the firewall step runs in converge mode. The hardening fixes (D5-D7) apply only in fresh or reapply mode (`cmd/setup.go:220`). Bumping `CurrentVersion` to `1.2.2` makes the next `tako setup` on an existing server run in reapply mode, so custom-port servers get the corrected fail2ban jail and a validated sshd reload. Reapply is the documented upgrade path (`docs/SERVER-SETUP-VERSIONING.md:70-76`). `tako upgrade servers` keeps managing only takod.
- *Alternative:* moving hardening into converge. Rejected because it changes the documented converge contract (`pkg/engine/setup.go:29-33`, `docs/MACHINE-INTERFACE.md:268-270`).

### D9. Interface ripple
`currentSetupRefresher.ConfigureFirewall` and `setupNodeProvisioner.HardenSecurity` change signature (`cmd/setup.go:198-208, 316-320`). The `recordingSetupRefresher` fake is updated to match (`cmd/setup_test.go:246, 291`). `setupNodeSteps` captures the report into the node result through a closure.

## Risks / Trade-offs

- [`$SSH_CONNECTION` is absent in some session types] → The configured port and 22 are still allowed. The probe catches anything they miss.
- [The probe fails for a reason other than the firewall, such as a host-key prompt or transient network loss] → Setup disables UFW and fails loudly. That leaves the host open rather than locked out, which is the intended fail-safe direction. The operator re-runs setup.
- [Disabling UFW during rollback briefly leaves the host without a host firewall] → This is the same exposure as the existing disable-update-enable window (`provisioner.go:1240-1244`). It is reported in the error.
- [Keeping 22 always open on hosts that moved SSH elsewhere] → This matches current behavior and is a deliberate, permanent choice (D1). Operators can delete the rule manually.
- [`sshd_config.d` drop-ins can override the main-file edits] → Out of scope. `sshd -t` still validates the merged config.
- [The bounded probe dial needs an SSH client option] → It is a small, additive API in `pkg/ssh`. Production dial behavior is unchanged.
- [The `firewallPorts` semantic narrowing] → Consumers that read `firewallPorts` on skipped hosts get nothing instead of a misleading list. This is documented in `docs/MACHINE-INTERFACE.md`, and the new `firewall.status` field disambiguates.

## Migration Plan

- No tako.yaml migration. After upgrading the CLI, operators run `tako setup`. Servers at manifest `1.2.1` reapply. Servers already at `1.2.2` converge and still refresh firewall SSH rules.
- Rollback is to install the previous CLI. Rules Tako added remain in place, which is harmless. The `.tako-bak` sshd backup is left on disk.
