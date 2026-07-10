# Native Sentry recipe

This is a capability proof for running the official `getsentry/self-hosted`
26.6.0 topology on native Tako primitives. It is not a supported Sentry
installation path. Sentry supports its Docker Compose packaging and
`install.sh`; use that path when upstream supportability matters more than
native Tako operation.

The `production` environment is the upstream errors-only profile: 31 workloads
plus three deploy-time bootstrap runs. The `feature-complete` environment
inherits those services and adds the 41 services in the upstream
feature-complete profile plus its profiles bucket bootstrap. The anchors and
merge keys are intentional: this file is about the same size as upstream's
916-line Compose file rather than the roughly 5,000 lines a fully expanded
two-profile translation would require.

The image tags, commands, dependency topology, and profile split were
translated from the `getsentry/self-hosted` `26.6.0` tag. The recipe uses one
shared Sentry image build, immutable operator files, external data volumes,
native raw port publication for nginx, a native scheduled cleanup job, and
fingerprinted `kind: run` migration/bootstrap steps.

## Capacity and host requirements

- Errors-only: start with at least 2 CPU and 7 GB RAM.
- Feature-complete: start with at least 4 CPU and 14 GB RAM.
- Feature-complete requires an x86-64 CPU with SSE4.2. Verify with
  `grep -q sse4_2 /proc/cpuinfo` on the target before deploying.
- This recipe has config/preflight coverage but has not been exercised in this
  repository against a 14 GB SSE4.2 playground. Treat that as a live acceptance
  blocker, not as proof that the topology has run successfully.

Budget 1-2 operator hours each month to compare the next self-hosted tag and
port its packaging changes. Major dependency upgrades can take a day because
upstream's stateful upgrade ladders remain manual.

## Required pre-first-deploy steps

1. Copy `.env.example` to `.env` and set the real host, SSH key, bind port, and
   mail hostname.
2. Generate a Sentry key without storing it in YAML:

   ```sh
   sentry_secret="$(docker run --rm ghcr.io/getsentry/sentry:26.6.0 config generate-secret-key)"
   tako secrets set "SENTRY_SYSTEM_SECRET_KEY=$sentry_secret" --env production
   tako secrets set "SENTRY_SYSTEM_SECRET_KEY=$sentry_secret" --env feature-complete
   unset sentry_secret
   ```

   Generate the key once and store that identical value in both environments.
   They share Sentry's persistent data; a different value is an unintended key
   rotation that invalidates sessions and encrypted state.

   Feature-complete also needs
   `tako secrets set "LAUNCHPAD_RPC_SHARED_SECRET=$(openssl rand -hex 32)" --env feature-complete`.
3. Generate Relay credentials with the pinned upstream image at the ignored
   path configured by `SENTRY_RELAY_CREDENTIALS_FILE`:

   ```sh
   docker run --rm ghcr.io/getsentry/relay:26.6.0 credentials generate \
     > config/runtime/relay/credentials.json
   ```

   The runtime directory is ignored by Git; keep it that way. The Tako file
   declaration publishes the generated file as secret content.
4. Create every volume marked `external: true` before the first deploy:

   ```sh
   for volume in sentry-data sentry-postgres sentry-redis sentry-kafka sentry-clickhouse sentry-seaweedfs; do
     docker volume create "$volume"
   done
   ```

   On a remote node, run those commands on the node selected by `sentry`.
5. Validate and deploy errors-only first:

   ```sh
   tako validate --env production
   tako deploy --env production
   ```

The deploy graph runs `snuba-bootstrap`, `sentry-upgrade`, and
`s3-nodestore-bootstrap` before their consumers. These operations are
fingerprinted and rerun when their image, command, inputs, files, or dependencies
change. Create the first user after the first healthy deploy with an explicit
one-off container or the supported upstream tooling; Tako does not model an
interactive run.

For the full profile, complete the higher capacity/SSE4.2 checks and deploy:

```sh
tako remove --env production --yes
tako validate --env feature-complete
tako deploy --env feature-complete
```

The two profiles are separate Tako environments but intentionally share the
same external volumes and host port. They cannot run concurrently. `tako
remove` stops and removes the production fleet before the profile switch and
protects the six declared external volumes. Non-external volumes are not shared
with the feature environment and remain inactive under production-scoped names;
back up or migrate any of those you intentionally need first.
Do not deploy `feature-complete` while `production` is still running.

## Operating boundary

Do not translate or skip the stateful ladders in upstream `install.sh` merely
because the service images changed. ClickHouse and PostgreSQL major-version
ladders, config mutation, user prompts, and other state inspection remain
manual operator work or explicit additional `kind: run` steps after review.
The monthly drift review must compare `.env`, `docker-compose.yml`, `install.sh`,
the `install/` scripts, and all operator config templates.

GeoIP is intentionally disabled in both Sentry and Relay because this recipe
does not distribute licensed MaxMind data. To enable it, publish a valid
`GeoLite2-City.mmdb` with `files:` to `/geoip/GeoLite2-City.mmdb` for both
services, restore `geoip_path` in Relay, and set `GEOIP_PATH_MMDB` in
`sentry.conf.py`.

Sentry upstream upgrades use a stop-world sequence: stop old services, migrate,
then start the new services. Tako currently orders the new migration runs before
new consumers but does not stop all old consumers first. During a migration,
old consumers can therefore overlap briefly with the migration. Kafka buffers
ingest traffic, which reduces impact but does not make every schema migration
safe. Schedule a maintenance window, stop affected old consumers manually when
the upstream migration requires it, and confirm the queues drain afterward.
A declarative `stops:` primitive is intentionally deferred.

See [UPGRADE.md](UPGRADE.md) for the pinned-tag drift workflow.
