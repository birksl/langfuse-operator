# ClickHouse

Langfuse uses ClickHouse for high-performance analytics and trace storage. The operator supports **external** and **managed** modes.

::: warning Production guidance
For production workloads use **external** ClickHouse — either a managed service (ClickHouse Cloud, Altinity.Cloud, Aiven) or a cluster deployed via the [Altinity ClickHouse Operator](https://github.com/Altinity/clickhouse-operator) that you operate yourself. Managed mode in this operator is a **single-node, dev-only** deployment with no replication, no sharding, and no backups.
:::

## External

Connect to an existing ClickHouse instance:

```yaml
spec:
  clickhouse:
    external:
      secretRef:
        name: langfuse-clickhouse
        keys:
          url: url                      # HTTP interface (http://host:8123)
          migrationUrl: migration_url   # native protocol (clickhouse://host:9000)
          username: username
          password: password
```

::: info
`migrationUrl` uses the ClickHouse **native protocol** (`clickhouse://host:9000`) and is required for schema migrations. `url` uses the **HTTP interface** (`http://host:8123`) for query traffic.
:::

::: tip
For single-node ClickHouse deployments, the operator automatically sets `CLICKHOUSE_CLUSTER_ENABLED=false` to avoid `ON CLUSTER` DDL errors that require ZooKeeper/Keeper.
:::

## Managed

::: danger Deprecated — removed in 0.11.0
Managed ClickHouse is **deprecated since 0.10.0** and will be **removed in 0.11.0**. An instance using it reports a `Deprecated` status condition.

Migrate to `external`, backed by [ClickHouse Cloud](https://clickhouse.com/cloud), [Altinity.Cloud](https://altinity.com/cloud-database/), Aiven, or a cluster you run with the [Altinity ClickHouse Operator](https://github.com/Altinity/clickhouse-operator).
:::

::: warning Dev / CI only
Managed mode deploys a **plain single-node ClickHouse StatefulSet**, not a clustered deployment via the Altinity ClickHouse Operator. `CLICKHOUSE_CLUSTER_ENABLED=false` is forced — no ZooKeeper/Keeper, no `ReplicatedMergeTree`, no `ON CLUSTER` DDL. The operator does not take backups or snapshots. Suitable for local development, evaluation, and CI; **not for production**.

The `shards` field is ignored. Setting `replicas > 1` creates N independent pods that do not replicate data — do not use.
:::

Deploy a single-node ClickHouse for development:

```yaml
spec:
  clickhouse:
    managed:
      storageSize: "100Gi"
      storageClass: gp3-encrypted
      resources:
        preset: small              # small | medium | large | custom
      auth:
        secretRef:                 # optional, omit to auto-generate
          name: ch-creds
          keys:
            username: username
            password: password
```

Resource presets:

| Preset | CPU Request | Memory Request |
|---|---|---|
| `small` | 1 | 2Gi |
| `medium` | 2 | 8Gi |
| `large` | 4 | 16Gi |
| `custom` | user-defined | user-defined |

## Encryption

::: warning Accepted but not implemented
`clickhouse.encryption` is not read by any controller — setting `enabled: true` encrypts nothing. Encryption at rest belongs to the storage layer: use an encrypted `storageClass` for a self-hosted cluster, or your provider's encryption for a managed one. Blob-storage encryption is configured on the bucket.
:::

## Data Retention

Configure TTL-based retention per table type:

```yaml
spec:
  clickhouse:
    retention:
      traces:
        ttlDays: 90          # 0 = infinite
      observations:
        ttlDays: 90
      scores:
        ttlDays: 180
      storagePressure:
        enabled: true
        warningThresholdPercent: 75
        criticalThresholdPercent: 90
        minRetainDays: 7
```

TTLs are applied as `ALTER TABLE … MODIFY TTL`, which ClickHouse enforces during merges — expiring rows disappear over the following hours, not at the instant the TTL passes. The `RetentionConfigured` condition and `status.clickhouse.retentionApplied` reflect what ClickHouse actually accepted, so a failure there means the TTL is not in force whatever the spec says.

Storage pressure is **reporting only**: crossing a threshold raises the `StoragePressure` condition and nothing else. `pruneOldestPartitions` is accepted by the API but not implemented — dropping partitions is irreversible data loss, so the operator leaves that call to a human, and `minRetainDays` will only matter once something prunes. The percentage is measured per node and taken from the fullest one; see [StoragePressureSpec](../reference/langfuseinstance.md#storagepressurespec).

## Schema Drift Detection

The operator periodically checks that the tables Langfuse's migrations create are present:

```yaml
spec:
  clickhouse:
    schemaDrift:
      enabled: true
      checkIntervalMinutes: 60
```

The check is table-level — `traces`, `observations`, `scores`, `schema_migrations` — and deliberately not column-level: Langfuse owns its schema and changes it between versions, so an operator-side column manifest would report drift on every upgrade. Missing tables almost always mean migrations never ran, or ran against a different database.

The result lands on the `SchemaDriftChecked` condition, whose polarity is worth noting: `True` means the check ran and found everything, `False` with reason `TablesMissing` means drift, and `Unknown` means the check could not reach ClickHouse.

Two limitations to be aware of:

- **`autoRepair` does nothing.** The field is accepted, but repairing means recreating Langfuse's own tables, and a wrong `CREATE TABLE` here would corrupt the schema for good. When drift is found with `autoRepair: true` the condition says so rather than pretending a repair happened.
- **The check sees one node.** It reads `system.tables` from whichever node answers, so on a cluster it cannot tell you that a table is missing on *some* nodes. Storage pressure reads every node; this check does not yet.
