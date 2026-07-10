# Upgrade playbook

This playbook uses `26.6.0 -> <next-tag>` deliberately. Never infer the next tag
or change only image strings: select an actual upstream release and review its
packaging diff.

1. Fetch both tags and create an isolated drift report:

   ```sh
   git clone https://github.com/getsentry/self-hosted.git /tmp/sentry-self-hosted
   cd /tmp/sentry-self-hosted
   git fetch --tags
   git diff 26.6.0..<next-tag> -- .env docker-compose.yml install.sh install/ sentry/ snuba/ relay/ symbolicator/ taskbroker/ clickhouse/ nginx.conf redis.conf
   ```

2. Classify every upstream change before editing this recipe:

   - image/tag, command, environment, health check, dependency, or profile split;
   - operator file/config change;
   - new or removed persistent/external volume;
   - idempotent bootstrap/migration that can be a `kind: run`;
   - stateful/manual `install.sh` ladder that must remain an operator procedure;
   - stop-world migration that is unsafe under Tako's current overlap caveat.

3. Update `tako.yaml`, the shared build argument, operator files, and both docs.
   Keep `production` at exactly the upstream errors-only workload set and keep
   `feature-complete` as its inherited delta. If an anchor default changes,
   inspect every consumer of that anchor.
4. For stateful dependency ladders, follow the upstream scripts step by step on
   backups or a cloned environment. Do not encode a blind image jump. Record
   backup/restore proof and the exact manual commands in the change review.
5. Run local contract checks:

   ```sh
   make validate-examples
   go test ./examples ./pkg/config
   ```

6. Exercise errors-only on a disposable host first. Verify all `kind: run`
   records succeeded, `/api/0/` and `/_health/` respond through nginx, a test
   error reaches Snuba/ClickHouse, and a restart preserves external-volume data.
7. Exercise feature-complete separately on an SSE4.2 host with at least 14 GB
   RAM. Send errors, transactions, metrics, profiles, replays, and uptime data;
   verify each corresponding consumer and query path.
8. Rehearse rollback before production. Image/config rollback cannot reverse a
   database migration; use the upstream rollback guidance or restore the
   pre-upgrade volume backups when the migration is not backward compatible.
9. During the production maintenance window, stop any old consumers named by
   the upstream migration, apply the deploy, confirm the migration run records,
   then allow consumers to resume and watch Kafka lag until it returns to the
   pre-upgrade baseline.

Expected steady-state drift work is 1-2 hours per monthly release. Stop and
allocate a full day when the diff contains a major PostgreSQL, ClickHouse,
Kafka, storage, or migration transition.
