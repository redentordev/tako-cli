# Server Setup Contract

`tako setup` prepares an existing server for the single takod runtime path. A
server is considered set up only when `/etc/tako/version.json` exists and parses
as a Tako setup manifest.

## Manifest

```json
{
  "version": "1.2.0",
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
4. Install WireGuard tools.
5. Configure UFW for SSH, HTTP, HTTPS, and the mesh port.
6. Apply host hardening and auto-recovery checks.
7. Ensure the deploy user and monitoring agent.
8. Install or reuse the server-side tako binary.
9. Install and restart the takod systemd service.
10. Write /etc/tako/version.json.
```

Released CLI builds download the matching Linux release asset for the server
architecture and install it as `/usr/local/bin/tako`. Development builds reuse
an existing server-side binary when present and otherwise skip the systemd
service install; this keeps local development independent from published
releases.

## Upgrade Behavior

If a server has an older setup manifest, Tako executes the configured setup
upgrade path and then refreshes the takod runtime. If the manifest is already at
the current setup version, setup still refreshes the takod binary and systemd
service so runtime changes are applied without needing a manifest bump.

## Version Ownership

Setup versioning describes server prerequisites and node-local runtime
installation. Application desired state, deployment history, and actual
container snapshots live under the takod state paths described in
`docs/ORCHESTRATION-MODEL.md`.
