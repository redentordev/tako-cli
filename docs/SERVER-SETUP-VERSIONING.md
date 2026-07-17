# Server Setup Contract

`tako setup` prepares an existing server for the single takod runtime path. A
server is considered set up only when `/etc/tako/version.json` exists and parses
as a Tako setup manifest.

## Manifest

```json
{
  "version": "1.2.1",
  "installed_at": "2026-06-13T00:00:00Z",
  "last_upgrade": "2026-06-13T00:00:00Z",
  "components": {
    "docker": "24.0.7"
  },
  "features": [
    "docker",
    "wireguard-mesh",
    "tako-proxy",
    "firewall",
    "monitoring"
  ],
  "tako_cli_version": "v0.3.0"
}
```

Missing or invalid manifests are treated as not set up. Setup then runs the
current provisioning path from scratch.

## Current Provisioning Path

```text
1. Check Ubuntu/Debian requirements.
2. Install or refresh system packages.
3. Install and enable Docker.
4. Verify that rootful system Docker is reachable through `sudo docker info`.
5. Install WireGuard tools.
6. Configure UFW for SSH, HTTP, HTTPS, HTTP/3 UDP 443, the mesh listen port,
   routed peer mesh traffic, and persistent IPv4 forwarding for mesh routing.
7. Apply host hardening and auto-recovery checks.
8. Ensure the deploy user and monitoring agent.
9. Install or reuse the server-side tako binary.
10. Install and restart the takod systemd service with the configured node name
   and node-local actual-state refresh interval.
11. Write /etc/tako/version.json.
```

Setup opens the required host firewall path for HTTP/3, but it does not create
the shared `tako-proxy` container by itself. Deploy, scale, rollback, and other
proxy-reconciling operations create or refresh the shared proxy when public
routes exist; `tako doctor` verifies the live proxy container shape afterward.

Remote deployment nodes currently require rootful system Docker because `takod`
reconciles systemd, WireGuard, firewall rules, published ports, volumes, and the
shared proxy. Normal takod deploys stream build contexts to a remote node and do
not require the laptop's Docker daemon. Local Docker Desktop, Colima, or
rootless Docker can still be used for custom local build workflows outside the
takod deploy path, or for manually pre-building images referenced with
`image:`. Rootless remote server mode is blocked until it has a dedicated live
proof.

Released CLI builds download the matching Linux release asset for the server
architecture during explicit setup or node upgrade commands. Application
commands such as deploy, scale, and rollback never install, restart, or upgrade
node software; they fail with upgrade guidance when the agent contract is not
compatible. Development node upgrades pass a Linux binary explicitly with
`tako upgrade servers --takod-binary`.

## Upgrade Behavior

If a server has an older setup manifest, Tako executes the configured setup
upgrade path and then refreshes the takod runtime. If the manifest is already at
the current setup version, setup still refreshes the takod binary and systemd
service so runtime changes, including takod flags and background refresh
behavior, are applied without needing a manifest bump.

Use `tako doctor` to surface stale or unavailable server-side takod agents in
the normal health report. Use `tako upgrade servers` when you only need to patch
those agents without rerunning the full setup path:

```bash
tako upgrade servers --dry-run
tako upgrade servers
tako state status
tako deploy --yes
```

For enrolled clusters the command targets the authoritative cluster inventory,
independent of application environment server subsets. It preflights the
stable N/N-1 lifecycle protocol range and controller-owned SSH pins,
upgrades a deterministic worker canary, then the remaining workers, and the
single controller last. A failed canary blocks later stages. Each node keeps a
durable previous binary, atomically publishes the candidate, restarts and
verifies `/v1/status`, and commits only after the target version and capability
contract are reported. Verification failure automatically restores and
restarts the previous binary. If the protected PaaS deployment worker is
installed, its binary, systemd health, and protected ingress are verified as
part of the same transaction. A boot recovery unit restores pending rollback
evidence before either service starts after an interrupted host reboot. A
renewable cluster lease on the authoritative controller rejects overlapping
upgrade coordinators. Candidate transfer happens before a short renewable,
token-bound node transaction lease is acquired. Under that lease, a fresh
status probe captures immutable identity, membership generation, lifecycle,
and roles; the verified candidate reopens the protected identity and inventory
and checks that exact contract immediately before publication. An expired node
lease permits no-reboot recovery of pending rollback evidence. Boot recovery
clears only the node-local lease; the external controller lease survives until
its owner releases it or it expires.
Node enrollment, removal, and inventory publication acquire the same node
guard and fail while an upgrade lease is active, preventing lifecycle changes
from interleaving with activation, verification, or commit.
The normal upgrade path refuses to replace a newer agent with an older CLI
release; intentional downgrades belong to the disaster-recovery workflow.
The setup manifest is refreshed only after runtime verification. Development
builds must pass `--takod-binary` with a Linux binary.

For a rebuilt server that should keep the same logical node name, keep the node
in `tako.yaml` and run the targeted repair path:

```bash
tako setup --server <node> -e production
tako upgrade servers --server <node> -e production
tako state repair -e production
tako deploy -e production --yes
```

Use `tako state forget-node <node> --yes` only when the node is permanently
retired and has already been removed from the active environment config.

## Version Ownership

Setup versioning describes server prerequisites and node-local runtime
installation. Application desired state, deployment history, and actual
container snapshots live under the takod state paths described in
`docs/ORCHESTRATION-MODEL.md`.
