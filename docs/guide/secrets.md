# Secret Management

## Auto-Generation

By default, the operator generates cryptographic secrets for values not explicitly provided:

```yaml
spec:
  secrets:
    autoGenerate:
      enabled: true      # default: true
```

Auto-generated values are stored in a Secret named `<instance>-generated-secrets`:

| Key | Purpose |
|---|---|
| `nextauth-secret` | NextAuth session encryption |
| `salt` | Encryption salt |
| `clickhouse-username` | Managed ClickHouse username |
| `clickhouse-password` | Managed ClickHouse password |
| `redis-password` | Managed Redis password |
| `database-url` | Managed PostgreSQL connection string |

To provide your own values instead, set `secretRef` on the relevant spec fields. The operator skips auto-generation for any field with an explicit reference.

## Secret Rotation

The operator watches every Secret referenced in the spec. When one changes it stamps a hash annotation on the pod templates, which rolls the pods:

```yaml
spec:
  secrets:
    rotation:
      enabled: true     # not read — rotation detection is always on
```

The hash is **one composite** over every referenced Secret, and it is stamped on both Deployments, so any referenced Secret changing restarts Web *and* Worker. There is no per-secret mapping today, and `spec.secrets.rotation` — both `enabled` and `customMappings` — is not read: detection cannot be turned off, and custom mappings do nothing.

Rotating a credential is safe with respect to the datastore-target freeze: only Secret *names* and endpoint *key names* are part of an instance's target, never the values. See [Changing the datastore target](../reference/langfuseinstance.md#changing-the-datastore-target).

## How It Works

1. The Secret Controller watches every Secret referenced by the `LangfuseInstance` spec
2. On change it hashes each Secret's keys and values in sorted order into one SHA-256 digest
3. The digest is stamped on both pod templates:
   ```
   langfuse.palena.ai/secret-hash: <sha256>
   ```
4. Kubernetes sees the annotation change and rolls the pods
5. `status.secrets.lastRotationCheck` is stamped only when a hash actually changed — writing it every pass would make the operator rewrite status forever
