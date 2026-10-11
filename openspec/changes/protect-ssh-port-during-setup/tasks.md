# Tasks

## 1. SSH port resolution and bounded probe

- [ ] 1.1 Add pure `parseSSHConnectionServerPort` and `sshPortSet(configured, live)` in `pkg/provisioner` (always includes 22, sorted, deduped, ignores invalid/missing live port); verify with table tests in `pkg/provisioner/firewall_test.go` covering default, custom, NAT-mismatch, and empty `$SSH_CONNECTION`.
- [ ] 1.2 Add `(*Provisioner).ResolveSSHPorts(configuredPort int)` reading `$SSH_CONNECTION` (reuse the `detectSSHClientIP` command pattern at `provisioner.go:1490`); verify `go build ./...`.
- [ ] 1.3 Add a bounded-dial option to `pkg/ssh` (e.g. `ProbeAuth(host, port, user, keyPath, password, ProbeOptions{Attempts, Timeout}, pinned *RecordedHostKey)`) that dials a fresh, non-pooled connection and closes it; verify a unit test asserts attempts/timeout are honored without reading `TAKO_SSH_CONNECT_ATTEMPTS`, and existing `pkg/ssh` tests still pass.

## 2. Firewall step: confirm, enable, probe, report

- [ ] 2.1 Change `FirewallAllowedPorts`/`firewallAllowCommands` to take the SSH port set (one `ufw allow <p>/tcp comment 'SSH (fail2ban protected)'` per port, no `|| true` on SSH rules); update `TestFirewallAllowCommands*` and add a custom-port test; verify `go test ./pkg/provisioner/`.
- [ ] 2.2 Add pure `parseUFWAddedRules` for `ufw show added` output plus `missingSSHPorts`/`staleSSHPorts` helpers; verify table tests with real ufw output samples (including IPv6 and comment-less rules).
- [ ] 2.3 Add `FirewallOptions`/`FirewallReport` and rework `ConfigureFirewall` per design D2 (confirm SSH rules before `ufw --force enable`, return error naming missing port, call `Probe` after enable, `ufw --force disable` + lockout-averted error on probe failure); verify `TestConfigureFirewallRejectsInvalidMeshPort` still passes and `go build ./...`.
- [ ] 2.4 On the skip path return `Status: skipped`, `Backend: none`, a reason, and `NativeFirewall: firewalld` when `systemctl is-active firewalld` reports active; emit a warning listing ports to open; verify a test on the pure warning/report builder.
- [ ] 2.5 Add `SetupFirewall` and `SetupNodeResult.Firewall` (`json:"firewall,omitempty"`) in `pkg/engine/setup.go`; verify `go build ./...`.

## 3. Setup command wiring and result document

- [ ] 3.1 In `cmd/setup.go` resolve the SSH port set once per node, build the firewall probe (configured host/port, full auth, pinned host key when recorded, bounded dial) and pass `FirewallOptions`; rename the step title to "Configuring firewall"; verify `go build ./...`.
- [ ] 3.2 Populate `node.Firewall` and set `node.FirewallPorts` from the report's applied ports (empty on skip) instead of recomputing at `cmd/setup.go:175`; emit stale-port warnings via `emitSetupLine`; verify a cmd test that a skipped report yields no `firewallPorts`.
- [ ] 3.3 Update `currentSetupRefresher`/`setupNodeProvisioner` signatures and the `recordingSetupRefresher` fake in `cmd/setup_test.go`; verify `go test ./cmd/ -run Setup`.
- [ ] 3.4 Update `TestSetupResultDocumentGolden` (`cmd/machine_output_test.go:1111`) with the `firewall` object; document `firewall` and the narrowed `firewallPorts` semantics in `docs/MACHINE-INTERFACE.md` (setup paragraph ~line 266); verify the golden test passes.

## 4. SSH hardening safety

- [ ] 4.1 Add pure `sshdHardeningScript(opts)` per design D5 (backup, existing tuning edits, `PermitRootLogin prohibit-password` only when no active directive, password-auth edit only when enabled, `sshd -t` with restore + non-zero exit on failure); verify string tests assert backup precedes edits, `sshd -t` precedes exit 0, `PermitRootLogin no` is never rewritten, and password edit is absent when disabled.
- [ ] 4.2 Change `fail2banSSHDJailConfig` to take the SSH port set and emit `port = 22,2222`; verify unit test for default and custom ports.
- [ ] 4.3 Change `HardenSecurity` to `HardenSecurity(opts HardeningOptions)`: run the key-only probe, warn and keep password auth on failure, run the script via `runRootScript` with errors surfaced, then `systemctl reload ssh || systemctl reload sshd`; remove the double restart; verify `go build ./...` and that the hardening step fails with validator output when the script exits non-zero (fake/command-builder test).
- [ ] 4.4 Wire the key-only probe (`server.SSHKey`, no password, bounded dial) and SSH port set into the hardening step in `cmd/setup.go`; verify `go test ./cmd/ -run Setup`.

## 5. Version bump and documentation

- [ ] 5.1 Bump `setup.CurrentVersion` to `1.2.2` in `pkg/setup/version.go` and the manifest example in `docs/SERVER-SETUP-VERSIONING.md`; verify `go test ./pkg/setup/ ./cmd/`.
- [ ] 5.2 Update `docs/SERVER-SETUP-VERSIONING.md` provisioning path (step 6/7: SSH port set, confirm-before-enable, lockout probe, non-Debian skip reporting, validated sshd reload, no `PermitRootLogin no` rewrite) and upgrade note that re-running `tako setup` refreshes SSH firewall rules; verify by review against spec.
- [ ] 5.3 Document in `docs/CONFIGURATION.md` (servers section near line 150) that `port` is allowed through UFW during setup, that old ports are never closed automatically, and the manual `ufw delete` step; verify by review.
- [ ] 5.4 Fix `tako setup` Long help (`cmd/setup.go:28-38`): replace "disable root login" with "block root password login (key-based root login stays allowed)", note UFW only where available and that the configured SSH port is allowed; regenerate `man/tako-setup.1` with `make man`; verify `tako setup --help` output and that `git diff man/` shows only the setup page.
- [ ] 5.5 Update `README.md` setup lines (~180, ~273) and `docs/CI-CD.md:179-181` wording to mention the configured SSH port; verify by review.

## 6. Integration verification

- [ ] 6.1 Run `go build ./...` and `go test ./...` (Windows-only known failures excepted) and confirm `schema/tako.schema.json` and `completions/` are unchanged (no config/flag changes).
- [ ] 6.2 Manual check on a disposable Debian VPS with sshd on 2222: `tako setup` completes, `ufw status` lists 22 and 2222, a new `ssh -p 2222` works; then temporarily break the probe (e.g. wrong configured port forwarded nowhere) and confirm UFW is disabled and the step fails with the lockout-averted error.
- [ ] 6.3 Manual check on a Rocky/Alma host without ufw: result document shows `firewall.status: skipped`, no `firewallPorts`, and the human warning lists required ports (and firewalld when active).

## Workflow follow-up

- Archive the change after review and the manual VPS checks pass.
