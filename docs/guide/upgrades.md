# Upgrades

Change `spec.image.tag` and the operator upgrades the instance. That is the only
supported way to move a live instance forward: the version is the one part of an
instance's [datastore target](../reference/langfuseinstance.md#changing-the-datastore-target)
that is allowed to move in place.

## What Actually Happens

```yaml
spec:
  image:
    tag: v3.22.0    # was v3.21.0
```

On the next reconcile:

1. The migration controller notices the tag no longer matches
   `status.migration.appliedIdentity` and creates a migration Job running the
   **new** image. The Job is stamped with the identity it was created for, so its
   success is only ever credited to that target.
2. `MigrationsComplete` goes `False` with reason `MigrationStarted`, which puts
   `status.phase` at `Migrating`.
3. The instance controller applies the new image to the Web and Worker
   Deployments, and Kubernetes rolls them.
4. When the Job succeeds, `status.migration.appliedIdentity` is updated,
   `MigrationsComplete` goes `True`, and the phase returns to `Running` once both
   Deployments report ready replicas.

::: warning The rollout is not gated on the migration
Steps 1 and 3 happen in the same pass. New pods can start before the migration
Job finishes, and the operator sets `LANGFUSE_AUTO_POSTGRES_MIGRATION_DISABLED`
and `LANGFUSE_AUTO_CLICKHOUSE_MIGRATION_DISABLED` on both components — so they do
not migrate on their own either (which is deliberate: parallel pods running
`prisma migrate deploy` deadlock on Prisma's advisory lock).

Langfuse's schema changes are written to be forward-compatible, so this is
normally invisible. But if a release's notes call for a migration before the new
code runs, drain the instance yourself rather than relying on sequencing the
operator does not do.
:::

## Monitoring an Upgrade

```bash
kubectl get langfuseinstance langfuse -n langfuse -w
```

```
NAME       PHASE       READY   VERSION   AGE
langfuse   Migrating   false   3.21.0    5d
langfuse   Running     true    3.22.0    5d
```

`status.version` is what is **deployed**, so it trails the spec until the
Deployments have actually been applied. For the migration's own progress:

```bash
kubectl get langfuseinstance langfuse -n langfuse -o jsonpath='{.status.migration}' | jq
```

A failed migration reports on `MigrationsComplete` with reason
`MigrationFailed`, and `status.migration.issues` carries the pod-level cause. See
[Troubleshooting](./troubleshooting.md#migrations).

## Rolling Back

There is no automatic rollback. To go back, set `spec.image.tag` to the previous
version; the operator treats it as any other version change and rolls the
Deployments back.

The schema is **not** reverted, and cannot be: Langfuse ships no down
migrations, and the migration Job only ever moves forward. A downgrade therefore
runs older code against a newer schema — fine for Langfuse's forward-compatible
changes, not something to count on across a major version. Take a database
backup before a major upgrade and treat that, not the tag, as the way back.

## `spec.upgrade` Is Not Implemented

The CRD accepts a `spec.upgrade` block — `strategy`, `preUpgrade`,
`rollingUpdate`, `postUpgrade` with `autoRollback` — and **no controller reads
any of it**. Setting those fields changes nothing:

| Field | Reality |
|---|---|
| `strategy`, `rollingUpdate.*` | The Deployments use Kubernetes' own rolling update defaults. Use `spec.web.replicas` / `spec.worker.replicas` and a PDB to control disruption |
| `preUpgrade.runMigrations` | Migrations are controlled by [`spec.database.migration.runOnDeploy`](../reference/langfuseinstance.md#migrationspec) |
| `preUpgrade.backupDatabase` | No backup is triggered, and the operator has no backup mechanism to trigger — configure backups on the CloudNativePG `Cluster` you point `clusterRef` at, or on your managed provider |
| `postUpgrade.runBackgroundMigrations` | Langfuse's background migrations are not monitored by the operator |
| `postUpgrade.autoRollback`, `healthCheckTimeout` | Nothing rolls back, and no `UpgradeRolledBack` condition exists |

Post-upgrade health *is* reported — the health monitor probes every datastore and
both Deployments on its own schedule regardless of upgrades — it just does not
gate or reverse anything.
