# LangfuseInstance

`langfuseinstances.langfuse.palena.ai/v1alpha1`

Deploys and manages the complete Langfuse stack: Web, Worker, and all dependent services.

## Spec

| Field | Type | Default | Description |
|---|---|---|---|
| `image` | [`ImageSpec`](#imagespec) | | Container image configuration |
| `web` | [`WebSpec`](#webspec) | | Web component configuration |
| `worker` | [`WorkerSpec`](#workerspec) | | Worker component configuration |
| `auth` | [`AuthSpec`](#authspec) | | Authentication configuration |
| `tls` | [`TLSSpec`](#tlsspec) | | Trusted CA for encrypted datastore connections; see [Datastore TLS](../guide/datastore-tls.md) |
| `eeLicenseKey` | *SecretValue | | `LANGFUSE_EE_LICENSE_KEY` reference. Required for the `LangfuseOrganization`/`LangfuseProject` CRDs (EE/Pro-gated org-management API); see [Multi-Tenancy](../guide/multi-tenancy.md) |
| `secrets` | [`SecretManagementSpec`](#secretmanagementspec) | | Secret generation and rotation |
| `database` | [`DatabaseSpec`](#databasespec) | | PostgreSQL configuration |
| `clickhouse` | [`ClickHouseSpec`](#clickhousespec) | | ClickHouse configuration |
| `redis` | [`RedisSpec`](#redisspec) | | Redis configuration |
| `blobStorage` | [`BlobStorageSpec`](#blobstoragespec) | | Blob storage configuration |
| `llm` | [`LLMSpec`](#llmspec) | | LLM integration |
| `ingress` | [`IngressSpec`](#ingressspec) | | Kubernetes Ingress |
| `route` | [`RouteSpec`](#routespec) | | OpenShift Route |
| `gatewayAPI` | [`GatewayAPISpec`](#gatewayapispec) | | Gateway API HTTPRoute |
| `security` | [`SecuritySpec`](#securityspec) | | Security settings |
| `observability` | [`ObservabilitySpec`](#observabilityspec) | | Monitoring and tracing |
| `circuitBreaker` | [`CircuitBreakerSpec`](#circuitbreakerspec) | | Dependency circuit breaking |
| `upgrade` | [`UpgradeSpec`](#upgradespec) | | Upgrade strategy |

## Status

| Field | Type | Description |
|---|---|---|
| `ready` | bool | Whether the instance is fully operational |
| `phase` | string | `Pending`, `Migrating`, `Running`, `Degraded`, or `Error` |
| `web` | ComponentStatus | Web component state: `replicas`, `readyReplicas`, `endpoint`, and [`issues`](#podissue) |
| `worker` | WorkerComponentStatus | Worker component state: as above plus `queueDepth`, `circuitBreakerActive` |
| `database` | DatabaseStatus | Database connection and migration state |
| `migration` | [`MigrationStatus`](#migrationstatus) | Migration Job state, including pod-level failures |
| `clickhouse` | ClickHouseStatus | ClickHouse state including storage |
| `redis` | ConnectionStatus | Redis connection state |
| `blobStorage` | BlobStorageStatus | Blob storage state |
| `secrets` | SecretsStatus | Secret management state |
| `version` | string | Langfuse version actually deployed. Held back while `DatastoreTargetUnchanged` is `False`, since nothing rolled out |
| `publicUrl` | string | Public URL of the instance, held back on the same terms as `version` |
| `organizations` | int32 | Never written — the field exists but no controller counts them. Use `kubectl get langfuseorganizations` |
| `projects` | int32 | Never written; as above |
| `conditions` | []Condition | Standard Kubernetes conditions |

### Conditions

Watch the polarity: most are named for the healthy state, but `StoragePressure` and `Deprecated` are named for the problem, so `True` is the bad news. Two are absent entirely when there is nothing wrong.

| Type | `True` means | Notable reasons |
|---|---|---|
| `Ready` | The instance is fully operational — mirrors `status.ready` | The reason restates the phase: `AllComponentsReady`, `ComponentsStarting`, `MigrationInProgress`, `ComponentDegraded`, `ReconcileError`; plus `ConfigError` when the spec cannot be turned into a valid configuration |
| `DatabaseReady` | PostgreSQL answered a connection probe | `Connected`, `Unreachable`, `ConfigError` |
| `ClickHouseReady` | ClickHouse answered `/ping` | `Connected`, `Unreachable`, `ConfigError` |
| `RedisReady` | Redis answered `PING` | `Connected`, `Unreachable`, `ConfigError` |
| `BlobStorageReady` | The endpoint accepted a TCP connection — **or** none is configured, which reports `True` with reason `NotConfigured` | `Connected`, `Unreachable`, `ConfigError` |
| `WebReady`, `WorkerReady` | The component's Deployment has ready replicas | `DeploymentReady`, `DeploymentNotReady`, `DeploymentNotFound`, `FetchError` — the message names the pod-level cause when there is one |
| `MigrationsComplete` | Migrations have finished for the deployed version and target | `MigrationStarted`, `MigrationInProgress`, `MigrationFailed`, and a refusal when the target cannot be migrated into |
| `DatastoreTargetUnchanged` | *Absent when nothing is wrong.* Present and `False` while the spec points at datastores the schema was not migrated into, which freezes reconciliation — see [Changing the datastore target](#changing-the-datastore-target) | `TargetChangedAfterMigration` |
| `RetentionConfigured` | ClickHouse accepted every TTL statement | `NoTTLConfigured`, `ApplyFailed` |
| `SchemaDriftChecked` | The check ran and found every expected table, on every node | `Disabled`, `TablesMissing` and `TablesNotReplicated` report `False`; `CheckFailed` reports `Unknown` |
| `StoragePressure` | **A threshold is exceeded** — `False` is the healthy state here | `WarningThresholdExceeded`, `CriticalThresholdExceeded`, `WithinThresholds`; `QueryFailed` and `NoCapacityReported` report `Unknown`. See [StoragePressureSpec](#storagepressurespec) |
| `CircuitBreakerHealthy` | No circuit breaker is tripped. The tripped state also appears in `status.worker.circuitBreakerActive` | `AllDependenciesHealthy`, `CircuitOpen` |
| `Deprecated` | *Absent when nothing is wrong.* The spec uses fields scheduled for removal. The message names each field and the release it goes in | `DeprecatedFieldsInUse` |

---

## Type Reference

### ImageSpec

| Field | Type | Default | Description |
|---|---|---|---|
| `repository` | string | `langfuse/langfuse` | Container image repository |
| `tag` | string | **required** | Image tag |
| `pullPolicy` | string | `IfNotPresent` | `Always`, `IfNotPresent`, or `Never` |
| `pullSecrets` | []LocalObjectReference | | Image pull secrets |

### WebSpec

| Field | Type | Default | Description |
|---|---|---|---|
| `replicas` | *int32 | `1` | Number of Web pod replicas |
| `autoscaling` | *AutoscalingSpec | | HPA configuration |
| `resources` | *ResourceRequirements | | CPU/memory requests and limits |
| `podDisruptionBudget` | *PDBSpec | | PDB configuration |
| `topologySpreadConstraints` | [`*TopologySpreadSpec`](#topologyspreadspec) | | Not read; use `affinity` |
| `extraEnv` | []EnvVar | | Additional environment variables |
| `extraVolumeMounts` | []VolumeMount | | Additional volume mounts |
| `extraVolumes` | []Volume | | Additional volumes |
| `nodeSelector` | map[string]string | | Node selector |
| `tolerations` | []Toleration | | Tolerations |
| `affinity` | *Affinity | | Affinity rules |

### WorkerSpec

| Field | Type | Default | Description |
|---|---|---|---|
| `replicas` | *int32 | `1` | Number of Worker pod replicas |
| `autoscaling` | *AutoscalingSpec | | HPA configuration |
| `resources` | *ResourceRequirements | | CPU/memory requests and limits |
| `concurrency` | *int32 | `10` | `LANGFUSE_WORKER_CONCURRENCY` |
| `extraEnv` | []EnvVar | | Additional environment variables |
| `extraVolumeMounts` | []VolumeMount | | Additional volume mounts on the Worker container |
| `extraVolumes` | []Volume | | Additional volumes on the Worker pod |
| `nodeSelector` | map[string]string | | Node selector |
| `tolerations` | []Toleration | | Tolerations |
| `affinity` | *Affinity | | Affinity rules |

### AuthSpec

| Field | Type | Description |
|---|---|---|
| `nextAuthUrl` | string | Canonical URL for NextAuth (`NEXTAUTH_URL`) |
| `nextAuthSecret` | *SecretValue | Secret reference or auto-generate |
| `salt` | *SecretValue | Encryption salt reference or auto-generate |
| `emailPassword` | *EmailPasswordSpec | Email/password auth settings |
| `oidc` | *OIDCSpec | OpenID Connect settings |
| `initUser` | *InitUserSpec | Initial admin user |
| `adminApiKey` | *SecretValue | `ADMIN_API_KEY` reference or auto-generate; used by the Organization/Project controllers (see [Multi-Tenancy](../guide/multi-tenancy.md)) |

### DatabaseSpec

| Field | Type | Description |
|---|---|---|
| `cloudnativepg` | *CloudNativePGSpec | Reference a CNPG Cluster (recommended for production) |
| `external` | *ExternalDatabaseSpec | External PostgreSQL (recommended for production) |
| `managed` | *ManagedDatabaseSpec | **Not implemented** — reserved for a future release |
| `migration` | *MigrationSpec | Migration behavior |

### ClickHouseSpec

| Field | Type | Description |
|---|---|---|
| `external` | *ExternalClickHouseSpec | External ClickHouse (recommended for production) |
| `managed` | *ManagedClickHouseSpec | Single-node StatefulSet (dev / preview only — no replication, no backups) |
| `encryption` | *ClickHouseEncryptionSpec | Encryption settings |
| `retention` | *RetentionSpec | Data retention policies |
| `schemaDrift` | *SchemaDriftSpec | Schema drift detection |
| `database` | string | ClickHouse database (`CLICKHOUSE_DB`), also used by schema-drift detection and retention. Defaults to `default`. **The database must already exist** — no Langfuse migration creates it, and neither does the operator. Never put it in the connection URL. Fixed once migrations have succeeded; see [Changing the datastore target](#changing-the-datastore-target). |
| `cluster` | *ClickHouseClusterSpec | Replicated (clustered) ClickHouse — see below |

### ClickHouseClusterSpec

| Field | Type | Default | Description |
|---|---|---|---|
| `enabled` | bool | `false` | Use Langfuse's clustered migrations (`ReplicatedReplacingMergeTree` tables with `ON CLUSTER` DDL) and `clusterAllReplicas` at query time. |

Only for `external` ClickHouse that is a genuine replicated cluster with Keeper — for example one deployed by the Altinity ClickHouse operator. It is rejected for `managed`, which is a single node. Leave it off for a single-node endpoint or ClickHouse Cloud; upstream's own `docker-compose` defaults it off, even though Langfuse's env schema defaults it on.

Two constraints worth knowing before you enable it:

- **Your cluster must be named `default`.** Langfuse's clustered migrations hardcode `ON CLUSTER default` and golang-migrate does not template SQL, so any other cluster name requires hand-written migrations. The operator deliberately exposes no cluster-name field rather than one that only half works.
- **It cannot be changed after migrations have run.** ClickHouse fixes a table's engine at `CREATE` time, so switching would need every table rebuilt via `INSERT SELECT`. The operator refuses the change and reports it on `MigrationsComplete` rather than starting a migration that would fail against the existing tables.

Both `cluster.enabled` and `database` are part of the instance's datastore target, described below.

### Changing the datastore target

The operator treats the datastores an instance was migrated into as fixed. Only the image tag moves in place — that is the upgrade path. The target is:

| Part of the target | Not part of the target |
|---|---|
| `database.external.secretRef.name`, or the CNPG `clusterRef.name` | The values behind any key — rotate credentials freely |
| The `url` and `directUrl` key names under `database.external.secretRef.keys` | The `username`/`password` key names |
| `clickhouse.external.secretRef.name`, or `managed` | `image.tag` — upgrading is the normal path |
| The `url` and `migrationUrl` key names under `clickhouse.external.secretRef.keys` | |
| `clickhouse.database` and `clickhouse.cluster.enabled` | |

Endpoint key names count because a Secret can hold several endpoints, so the Secret name alone does not identify a datastore. `directUrl` counts as much as `url`: Langfuse's Prisma datasource declares `directUrl = env("DIRECT_URL")` and `prisma migrate deploy` prefers it when set, so it is where the schema actually lands.

Change any of them after a successful migration and the operator sets `DatastoreTargetUnchanged` to `False`, reports `Error`, and stops reconciling every child resource before it touches one — the running pods keep the configuration they were migrated for. `status.version` and `status.publicUrl` keep describing what is actually deployed rather than the edited spec. Reverting the spec clears the condition and reconciliation resumes.

To move an instance to different datastores, create a **separate `LangfuseInstance`** for the new target and cut over once it has migrated. The old instance keeps serving the old data until you remove it, which an in-place edit would not: re-pointing at another database leaves every row already written behind, invisible to the application, with nothing to warn you.

### RedisSpec

| Field | Type | Description |
|---|---|---|
| `external` | *ExternalRedisSpec | External Redis (recommended for production) |
| `managed` | *ManagedRedisSpec | Single-pod StatefulSet (dev / preview only — no HA, no backups) |

### BlobStorageSpec

| Field | Type | Description |
|---|---|---|
| `provider` | string | `s3`, `azure`, or `gcs` |
| `s3` | *S3Spec | S3-compatible storage config |
| `azure` | *AzureBlobSpec | Azure Blob Storage config |
| `gcs` | *GCSSpec | Google Cloud Storage config |

### SecuritySpec

| Field | Type | Default | Description |
|---|---|---|---|
| `readOnlyRootFilesystem` | *bool | `true` | Read-only root filesystem |
| `runAsNonRoot` | *bool | `true` | Run containers as non-root |
| `runAsUser` | *int64 | `1001` | Numeric UID to run as. Required alongside `runAsNonRoot`, because the Langfuse images declare a non-numeric `USER` the kubelet cannot verify. Override only if you build images with a different `ARG UID`. |
| `networkPolicy.enabled` | *bool | `true` | Create NetworkPolicy |
| `telemetry.enabled` | *bool | `true` | Langfuse telemetry |

### CircuitBreakerSpec

| Field | Type | Default | Description |
|---|---|---|---|
| `enabled` | *bool | `true` | Enable circuit breakers |
| `clickhouse` | *ComponentCircuitBreakerSpec | | ClickHouse circuit breaker |
| `redis` | *ComponentCircuitBreakerSpec | | Redis circuit breaker |
| `database` | *ComponentCircuitBreakerSpec | | Database circuit breaker |

### UpgradeSpec

::: danger Deprecated and ignored, removal in 0.12.0
No controller reads `spec.upgrade`. Setting any field below changes nothing about how an upgrade runs, raises the `Deprecated` condition, and the block goes away in 0.12.0. Upgrading works — change `spec.image.tag` — it just is not configurable here. See [Upgrades](../guide/upgrades.md#specupgrade-is-deprecated-and-ignored) for what governs each behaviour instead.
:::

| Field | Type | Default | Description |
|---|---|---|---|
| `strategy` | string | `rolling` | Not read |
| `preUpgrade` | [`*PreUpgradeSpec`](#preupgradespec) | | Not read |
| `rollingUpdate` | [`*RollingUpdateSpec`](#rollingupdatespec) | | Not read |
| `postUpgrade` | [`*PostUpgradeSpec`](#postupgradespec) | | Not read |

---

### Nested types

The remaining `*Spec` types referenced above. Field defaults marked `*T` mean the field is a pointer (omitting it falls back to the default; setting it explicitly to the zero value sticks).

### SecretValue

| Field | Type | Description |
|---|---|---|
| `secretRef` | *SecretKeyRef | Reference to an existing Secret key. When nil and auto-generation is enabled, the operator generates the value. |

### AutoscalingSpec

| Field | Type | Default | Description |
|---|---|---|---|
| `enabled` | bool | `false` | Toggle HPA creation |
| `minReplicas` | *int32 | `1` | Lower bound |
| `maxReplicas` | int32 | `10` | Upper bound |
| `targetCPUUtilization` | *int32 | `80` | Target CPU utilization (%) |
| `customMetrics` | []CustomMetric | | Additional scaling metrics (`type`, `threshold`) |

### PDBSpec

| Field | Type | Description |
|---|---|---|
| `enabled` | bool | Toggle PDB creation |
| `minAvailable` | *int32 | Minimum pods that must remain available |

### TopologySpreadSpec

**Deprecated and ignored, removal in 0.12.0** — no controller reads it, so no `topologySpreadConstraints` reach the pod templates, and using it raises the `Deprecated` condition. Spread pods with `spec.web.affinity` / `spec.worker.affinity` instead.

| Field | Type | Default | Description |
|---|---|---|---|
| `enabled` | bool | `false` | Not read |
| `maxSkew` | *int32 | `1` | Not read |
| `topologyKey` | string | `topology.kubernetes.io/zone` | Not read |

### EmailPasswordSpec

| Field | Type | Default | Description |
|---|---|---|---|
| `enabled` | *bool | `true` | Toggle email/password auth |
| `disableSignup` | bool | `false` | Block new user registration |

### OIDCSpec

Configures Langfuse's generic custom OIDC provider (mapped to the upstream `AUTH_CUSTOM_*` variables). The IdP must whitelist the callback URL `<NEXTAUTH_URL>/api/auth/callback/custom`.

| Field | Type | Description |
|---|---|---|
| `enabled` | bool | Toggle OIDC |
| `issuer` | string | OIDC issuer URL → `AUTH_CUSTOM_ISSUER` |
| `clientId` | *SecretKeyRef | Reference to OIDC client ID → `AUTH_CUSTOM_CLIENT_ID` |
| `clientSecret` | *SecretKeyRef | Reference to OIDC client secret → `AUTH_CUSTOM_CLIENT_SECRET` |
| `name` | string | Login button label → `AUTH_CUSTOM_NAME` (default `SSO`) |
| `scope` | []string | Requested OAuth scopes → `AUTH_CUSTOM_SCOPE`, space-joined (default `openid email profile`) |
| `ssoEnforcedDomains` | []string | Domains forced to sign in via SSO → `AUTH_DOMAINS_WITH_SSO_ENFORCEMENT`, comma-joined (password login disabled for them) |

### InitUserSpec

| Field | Type | Default | Description |
|---|---|---|---|
| `enabled` | bool | `false` | Create the initial admin user on first boot |
| `email` | string | | Initial user email |
| `password` | *SecretKeyRef | | Reference to the initial password |
| `orgName` | string | `Default` | Default organization name |
| `projectName` | string | `Default` | Default project name |

### SecretManagementSpec

| Field | Type | Description |
|---|---|---|
| `autoGenerate` | [`*AutoGenerateSpec`](#autogeneratespec) | Auto-generation of `NEXTAUTH_SECRET`, `SALT`, etc. |
| `rotation` | [`*RotationSpec`](#rotationspec) | Secret-rotation detection and restart |

### AutoGenerateSpec

| Field | Type | Default | Description |
|---|---|---|---|
| `enabled` | *bool | `true` | Toggle auto-generation of operator-owned secrets |

### RotationSpec

| Field | Type | Default | Description |
|---|---|---|---|
| `enabled` | *bool | `true` | Detect secret changes and trigger component restarts |
| `customMappings` | []SecretRestartMapping | | Map a Secret name → components to restart (`secretName`, `restartComponents: [web, worker]`) |

### CloudNativePGSpec

| Field | Type | Default | Description |
|---|---|---|---|
| `clusterRef` | ObjectReference | | Reference to an existing CNPG `Cluster` |
| `database` | string | `langfuse` | Database name within the cluster |

### ManagedDatabaseSpec

**Rejected since 0.10.0, removed in 0.11.0** — `database.managed` was never implemented, so every field below is unreachable. See [Database](../guide/database.md#managed-deprecated).

| Field | Type | Default | Description |
|---|---|---|---|
| `instances` | *int32 | `1` | Number of PostgreSQL instances |
| `storageSize` | string | `10Gi` | PVC size for each instance |
| `storageClass` | string | | Storage class for PVCs |
| `backup` | [`*DatabaseBackupSpec`](#databasebackupspec) | | Automated backup configuration |

### DatabaseBackupSpec

Reachable only from the rejected `managed` mode above, so the operator takes no backups of any kind. Configure them on the CloudNativePG `Cluster` you reference, or on your managed provider.

| Field | Type | Default | Description |
|---|---|---|---|
| `enabled` | bool | `false` | Toggle automated backups |
| `schedule` | string | `0 2 * * *` | Cron schedule |

### ExternalDatabaseSpec

| Field | Type | Description |
|---|---|---|
| `secretRef` | SecretKeysRef | Reference to a Secret with connection details. Recognised keys: `url` (required, `postgres://…`), `directUrl` (optional, bypasses pooling — Prisma runs migrations through it when set). With a `tls` block the `url` must **not** contain a query string. Percent-encode `/` (`%2F`), `?` (`%3F`) and `#` (`%23`) in the password — each ends the URL's authority and hides the host. `@` and `:` need no encoding: Prisma, the migration Job and the operator's probe all split credentials at the last `@`.<br><br>The Secret name and the `url`/`directUrl` key names are part of the instance's datastore target, so changing them is refused once a schema exists — see [Changing the datastore target](#changing-the-datastore-target). Credential keys and the values behind any key are not: rotate freely. |
| `tls` | [`DatabaseTLSSpec`](#databasetlsspec) | TLS for the PostgreSQL connection. |

### MigrationSpec

| Field | Type | Default | Description |
|---|---|---|---|
| `runOnDeploy` | *bool | `true` | Run migrations on every deployment |
| `backgroundMigrations` | [`*BackgroundMigrationSpec`](#backgroundmigrationspec) | | Not read; see below |

### BackgroundMigrationSpec

**Deprecated and ignored, removal in 0.12.0** — the operator has a client for `/api/public/background-migrations` but no controller calls it, so background migrations are neither monitored nor waited on, and using this raises the `Deprecated` condition. Langfuse runs them itself in the worker.

| Field | Type | Default | Description |
|---|---|---|---|
| `enabled` | *bool | `true` | Not read |
| `timeout` | string | `3600s` | Not read |

### ManagedClickHouseSpec

| Field | Type | Default | Description |
|---|---|---|---|
| `shards` | *int32 | `1` | Number of shards |
| `replicas` | *int32 | `1` | Replicas per shard |
| `storageSize` | string | `50Gi` | PVC size |
| `storageClass` | string | | Storage class |
| `resources` | [`*ClickHouseResourceSpec`](#clickhouseresourcespec) | | Resource preset or custom |
| `auth` | [`*ClickHouseAuthSpec`](#clickhouseauthspec) | | Credentials reference |

### ClickHouseResourceSpec

| Field | Type | Description |
|---|---|---|
| `preset` | string | One of `small`, `medium`, `large`, `custom` |
| `custom` | *ResourceRequirements | Used when `preset: custom` |

### ClickHouseAuthSpec

| Field | Type | Description |
|---|---|---|
| `secretRef` | *SecretKeysRef | Reference to a Secret with `username` and `password` keys. Omit to let the operator auto-generate. |

### ExternalClickHouseSpec

| Field | Type | Description |
|---|---|---|
| `secretRef` | SecretKeysRef | Reference to a Secret with connection details. Recognised keys: `url` (HTTP, e.g. `http://ch:8123`), `migrationUrl` (native, e.g. `clickhouse://ch:9000`), `username`, `password`. With a `tls` block, use the TLS scheme/port (`https://…:8443`, `clickhouse://…:9440`).<br><br>`migrationUrl`, `username` and `password` are **required**: the migration Job exits non-zero without them. The `url` must be an origin — never put a database or path in it, as ClickHouse's HTTP interface selects the database by parameter, not path. Use `clickhouse.database` instead.<br><br>The Secret name and the `url`/`migrationUrl` key names are part of the instance's datastore target; see [Changing the datastore target](#changing-the-datastore-target). |
| `tls` | [`ClickHouseTLSSpec`](#clickhousetlsspec) | TLS for the ClickHouse connection. |

### ClickHouseEncryptionSpec

**Deprecated and ignored, removal in 0.12.0** — no controller reads it, and using it raises the `Deprecated` condition. Encrypt at the storage layer instead; see [ClickHouse](../guide/clickhouse.md#encryption).

| Field | Type | Description |
|---|---|---|
| `enabled` | bool | Not read |
| `blobStorage` | bool | Not read |

### RetentionSpec

Applies to the ClickHouse database named by [`clickhouse.database`](#clickhousespec). `events` carries no TTL field even though the operator knows the table, so event rows are not expired by this spec.

| Field | Type | Description |
|---|---|---|
| `traces` | [`*TableRetentionSpec`](#tableretentionspec) | TTL on `traces.timestamp` |
| `observations` | [`*TableRetentionSpec`](#tableretentionspec) | TTL on `observations.start_time` |
| `scores` | [`*TableRetentionSpec`](#tableretentionspec) | TTL on `scores.timestamp` |
| `storagePressure` | [`*StoragePressureSpec`](#storagepressurespec) | Disk-usage reporting; see below |

### TableRetentionSpec

| Field | Type | Description |
|---|---|---|
| `ttlDays` | int32 | Days to retain data; `0` = infinite |

### StoragePressureSpec

| Field | Type | Default | Description |
|---|---|---|---|
| `enabled` | bool | `false` | Monitor ClickHouse storage pressure |
| `warningThresholdPercent` | int32 | `75` | Report `StoragePressure=True` with reason `WarningThresholdExceeded` above this |
| `criticalThresholdPercent` | int32 | `90` | Report `StoragePressure=True` with reason `CriticalThresholdExceeded` above this |
| `pruneOldestPartitions` | bool | `false` | **Deprecated and ignored, removal in 0.12.0.** Dropping partitions is irreversible data loss, so the operator reports and leaves the decision to a human. Setting it raises the `Deprecated` condition |
| `minRetainDays` | int32 | `7` | Ignored — it only had meaning alongside pruning |

The percentage is the **fullest node's**, not the cluster average: ClickHouse writes fail on whichever node runs out of disk, and averaging it with emptier peers hides that until it happens. On a cluster the condition message names that node and carries the cluster totals beside its own.

`storageUsed` and `storageTotal` in the status sum every node's disks, so replicated data counts once per replica — they describe the cluster's raw disks rather than its usable capacity. Reading them requires `clusterAllReplicas` when `clickhouse.cluster.enabled` is set, so a node that cannot be reached fails the read and reports `StoragePressure=Unknown` rather than a partial figure presented as the whole.

### SchemaDriftSpec

| Field | Type | Default | Description |
|---|---|---|---|
| `enabled` | bool | `false` | Periodic schema drift detection. Reports on `SchemaDriftChecked` and `status.clickhouse.schemaDrift` |
| `checkIntervalMinutes` | int32 | `60` | Interval between checks |
| `autoRepair` | bool | `false` | **Deprecated and ignored, removal in 0.12.0.** Repair means recreating Langfuse's own tables, and wrong DDL here would corrupt the schema; the condition reports the drift instead |

The check is table-level (`traces`, `observations`, `scores`, `schema_migrations`), not column-level — Langfuse changes its schema between versions, so a column manifest here would report drift on every upgrade.

On a clustered instance it reads `system.tables` through `clusterAllReplicas`, so it sees every node and reports which ones are short of a table. It also checks engines: with `cluster.enabled` set, a table whose engine is not `Replicated*` is reported as `TablesNotReplicated` — that is what an unclustered migration produces against a replicated cluster, and it makes every query routed to another replica fail while the tables look present from the node that has them.

### ManagedRedisSpec

| Field | Type | Default | Description |
|---|---|---|---|
| `replicas` | *int32 | `1` | Number of Redis replicas |
| `storageSize` | string | `5Gi` | PVC size |

### ExternalRedisSpec

| Field | Type | Description |
|---|---|---|
| `secretRef` | SecretKeysRef | Reference to a Secret with connection details. Recognised keys: `host`, `port`, `password`, `tls` (legacy boolean; prefer the `tls` block). |
| `tls` | [`RedisTLSSpec`](#redistlsspec) | TLS for the Redis connection. |

### TLSSpec

Trust configuration for encrypted datastore connections. See [Datastore TLS](../guide/datastore-tls.md).

| Field | Type | Description |
|---|---|---|
| `trustedCASecretRef` | [`CACertSecretRef`](#cacertsecretref) | CA mounted into Web + Worker and exported as `NODE_EXTRA_CA_CERTS`. Covers ClickHouse HTTPS, and is the default CA for Redis/PostgreSQL. |

### DatabaseTLSSpec

| Field | Type | Default | Description |
|---|---|---|---|
| `sslMode` | string | `require` | `disable`, `require`, `verify-ca`, or `verify-full`. Mapped to Prisma's `sslmode`/`sslaccept` parameters (Prisma has no CA-only mode, so `verify-ca` ≡ `verify-full`). |
| `caSecretRef` | [`CACertSecretRef`](#cacertsecretref) | | CA used as Prisma's `sslcert`. Defaults to `spec.tls.trustedCASecretRef`. |

The operator composes `DATABASE_URL` as `$(DATABASE_URL_BASE)?<params>` via env interpolation, so the `url` in the Secret must not contain its own query string.

### ClickHouseTLSSpec

| Field | Type | Default | Description |
|---|---|---|---|
| `enabled` | bool | `false` | Sets `CLICKHOUSE_MIGRATION_SSL=true`. The runtime HTTPS client trusts the CA via `NODE_EXTRA_CA_CERTS`. URLs in the Secret must use the TLS scheme/port. |

### RedisTLSSpec

| Field | Type | Default | Description |
|---|---|---|---|
| `enabled` | bool | `false` | Sets `REDIS_TLS_ENABLED=true` on Web + Worker. |
| `caSecretRef` | [`CACertSecretRef`](#cacertsecretref) | | CA for `REDIS_TLS_CA_PATH`. Defaults to `spec.tls.trustedCASecretRef` (ioredis ignores `NODE_EXTRA_CA_CERTS`). |
| `clientCertSecretRef` | [`ClientCertSecretRef`](#clientcertsecretref) | | Client cert/key for mutual TLS (`REDIS_TLS_CERT_PATH` / `REDIS_TLS_KEY_PATH`). |
| `serverName` | string | | TLS SNI/hostname override (`REDIS_TLS_SERVERNAME`). |

### CACertSecretRef

| Field | Type | Default | Description |
|---|---|---|---|
| `name` | string | | Secret name. |
| `key` | string | `ca.crt` | Secret key holding the PEM CA certificate. |

### ClientCertSecretRef

| Field | Type | Default | Description |
|---|---|---|---|
| `name` | string | | Secret name. |
| `certKey` | string | `tls.crt` | Secret key holding the PEM client certificate. |
| `keyKey` | string | `tls.key` | Secret key holding the PEM client private key. |

### PodIssue

A pod-level failure surfaced into `status.web.issues`, `status.worker.issues`, or `status.migration.issues`, so a stuck component can be diagnosed without inspecting pods by hand. Populated only while a component is not ready.

| Field | Type | Description |
|---|---|---|
| `pod` | string | Name of the affected pod. |
| `container` | string | Container that reported the problem. Empty for pod-level problems such as scheduling failures. |
| `reason` | string | Kubernetes reason, e.g. `CrashLoopBackOff`, `ImagePullBackOff`, `CreateContainerConfigError`, `Unschedulable`, `OOMKilled`. |
| `message` | string | Human-readable detail. For a crash loop this includes the previous run's exit code and captured output. |
| `restartCount` | int32 | Container restart count. |
| `fatal` | bool | The failure cannot self-heal and needs human action. Any fatal issue moves the instance to `phase: Error` instead of `Degraded`. |

**Fatal** reasons are those that never resolve on their own: `ImagePullBackOff`, `ErrImagePull`, `InvalidImageName`, `ErrImageNeverPull`, `CreateContainerConfigError`, `CreateContainerError`. `CrashLoopBackOff` is deliberately **not** fatal — Langfuse containers legitimately crash-loop while waiting for Postgres or ClickHouse during a cold start.

### MigrationStatus

| Field | Type | Description |
|---|---|---|
| `jobName` | string | Name of the migration Job. |
| `failed` | int32 | Number of failed migration pod attempts. |
| `issues` | [][`PodIssue`](#podissue) | Pod-level problems from the migration Job's pods. |

### S3Spec

| Field | Type | Description |
|---|---|---|
| `endpoint` | string | S3 endpoint URL (set for MinIO; omit for AWS) |
| `region` | string | S3 region |
| `bucket` | string | Bucket name (required) |
| `forcePathStyle` | bool | Path-style addressing (MinIO) |
| `credentials` | [`*S3CredentialsSpec`](#s3credentialsspec) | Credentials reference |

### S3CredentialsSpec

| Field | Type | Description |
|---|---|---|
| `secretRef` | SecretKeysRef | Reference to a Secret with `accessKeyId` and `secretAccessKey` |

### AzureBlobSpec

| Field | Type | Description |
|---|---|---|
| `storageAccountName` | string | Azure storage account name (used as the access key ID and to derive the default endpoint) |
| `containerName` | string | Blob container name (Langfuse's upload "bucket") |
| `endpoint` | string | Blob service endpoint override. Defaults to `https://<storageAccountName>.blob.core.windows.net` |
| `credentials` | [`*AzureCredentialsSpec`](#azurecredentialsspec) | Credentials reference |

### AzureCredentialsSpec

| Field | Type | Description |
|---|---|---|
| `secretRef` | SecretKeysRef | Reference to a Secret holding the storage **account key** under the `accountKey` key (override via the Keys map). Langfuse does not support Azure connection strings. |

### GCSSpec

| Field | Type | Description |
|---|---|---|
| `bucketName` | string | GCS bucket name |
| `projectId` | string | GCP project ID |
| `credentials` | [`*GCSCredentialsSpec`](#gcscredentialsspec) | Credentials reference |

### GCSCredentialsSpec

| Field | Type | Description |
|---|---|---|
| `secretRef` | SecretKeysRef | Reference to a Secret containing the GCP service-account JSON |

### LLMSpec

| Field | Type | Description |
|---|---|---|
| `apiBase` | string | LLM API base URL |
| `apiKey` | *SecretKeyRef | Reference to the LLM API key |
| `model` | string | LLM model name |

### IngressSpec

| Field | Type | Description |
|---|---|---|
| `enabled` | bool | Toggle Ingress creation |
| `className` | string | `IngressClass` name |
| `host` | string | Ingress hostname |
| `annotations` | map[string]string | Additional Ingress annotations |
| `tls` | [`*IngressTLSSpec`](#ingresstlsspec) | TLS configuration |

### IngressTLSSpec

| Field | Type | Description |
|---|---|---|
| `enabled` | bool | Toggle TLS |
| `secretName` | string | Existing TLS Secret name |
| `certManager` | [`*CertManagerSpec`](#certmanagerspec) | cert-manager integration |

### CertManagerSpec

| Field | Type | Description |
|---|---|---|
| `issuerRef.name` | string | Issuer name |
| `issuerRef.kind` | string | `Issuer` or `ClusterIssuer` (default `ClusterIssuer`) |

### RouteSpec

| Field | Type | Description |
|---|---|---|
| `enabled` | bool | Toggle OpenShift Route creation |
| `host` | string | Route hostname |
| `annotations` | map[string]string | Additional Route annotations |

### GatewayAPISpec

| Field | Type | Description |
|---|---|---|
| `enabled` | bool | Toggle HTTPRoute creation |
| `gatewayRef.name` | string | Gateway name (required) |
| `gatewayRef.namespace` | string | Gateway namespace (default: HTTPRoute namespace) |
| `gatewayRef.sectionName` | string | Listener name on the Gateway |
| `hostname` | string | HTTP hostname to match |
| `annotations` | map[string]string | Additional HTTPRoute annotations |

### NetworkPolicySpec

| Field | Type | Default | Description |
|---|---|---|---|
| `enabled` | *bool | `true` | Create per-component NetworkPolicies |
| `extraEgressPorts` | []NetworkPolicyPort | | Additional destination ports to allow. The defaults cover the well-known datastore ports (plaintext **and** TLS); use this for non-standard ports such as a connection pooler. See [Networking](../guide/networking.md#non-standard-ports). |

### NetworkPolicyPort

| Field | Type | Default | Description |
|---|---|---|---|
| `port` | int32 | | Destination port (1–65535). |
| `protocol` | string | `TCP` | `TCP` or `UDP`. |

### TelemetrySpec

| Field | Type | Default | Description |
|---|---|---|---|
| `enabled` | *bool | `true` | Toggle Langfuse's built-in telemetry (`TELEMETRY_ENABLED`) |

### ObservabilitySpec

| Field | Type | Description |
|---|---|---|
| `serviceMonitor` | [`*ServiceMonitorSpec`](#servicemonitorspec) | Deprecated and ignored; removal in 0.11.0 |
| `otel` | [`*OTELSpec`](#otelspec) | OpenTelemetry integration |

### ServiceMonitorSpec

::: danger Ignored since 0.10.0, removed in 0.11.0
Langfuse serves no Prometheus endpoint, so the ServiceMonitor this created could only name the web pod's `/api/public/health` — a JSON route Prometheus cannot parse, leaving a target permanently reported down. Setting any field below raises the `Deprecated` condition and creates nothing, and the operator deletes the ServiceMonitor an earlier version created. Use [`otel`](#otelspec); see [Observability](../guide/observability.md#servicemonitor-removed).
:::

| Field | Type | Default | Description |
|---|---|---|---|
| `enabled` | bool | `false` | Ignored |
| `interval` | string | `30s` | Ignored |
| `labels` | map[string]string | | Ignored |

### OTELSpec

| Field | Type | Default | Description |
|---|---|---|---|
| `enabled` | bool | `false` | Toggle OTEL |
| `endpoint` | string | | OTEL collector endpoint |
| `protocol` | string | `grpc` | `grpc` or `http` |

### ComponentCircuitBreakerSpec

| Field | Type | Default | Description |
|---|---|---|---|
| `action` | string | | `scaleWorkerToZero`, `emitCriticalEvent`, or `none` |
| `probeIntervalSeconds` | int32 | `15` | Health probe interval |
| `failureThreshold` | int32 | `3` | Failures before opening the circuit |
| `recoveryAction` | string | | `restoreScale` or `none` |

### PreUpgradeSpec

Part of the deprecated `spec.upgrade` block, removed in 0.12.0; see [UpgradeSpec](#upgradespec).

| Field | Type | Default | Description |
|---|---|---|---|
| `runMigrations` | *bool | `true` | Not read. Migrations follow [`database.migration.runOnDeploy`](#migrationspec) |
| `backupDatabase` | bool | `false` | Not read. The operator has no backup mechanism |

### RollingUpdateSpec

Part of the deprecated `spec.upgrade` block, removed in 0.12.0; see [UpgradeSpec](#upgradespec).

| Field | Type | Default | Description |
|---|---|---|---|
| `maxUnavailable` | *int32 | `0` | Not read. The Deployments use Kubernetes' rolling-update defaults |
| `maxSurge` | *int32 | `1` | Not read |

### PostUpgradeSpec

Part of the deprecated `spec.upgrade` block, removed in 0.12.0; see [UpgradeSpec](#upgradespec).

| Field | Type | Default | Description |
|---|---|---|---|
| `runBackgroundMigrations` | *bool | `true` | Not read. Langfuse's background migrations are not monitored |
| `healthCheckTimeout` | string | `120s` | Not read |
| `autoRollback` | bool | `false` | Not read. Nothing rolls back, and no `UpgradeRolledBack` condition exists |

## Example

```yaml
apiVersion: langfuse.palena.ai/v1alpha1
kind: LangfuseInstance
metadata:
  name: production
  namespace: langfuse
spec:
  image:
    tag: "3"
  auth:
    nextAuthUrl: "https://langfuse.example.com"
  web:
    replicas: 3
  worker:
    replicas: 2
  database:
    external:
      secretRef:
        name: langfuse-db
        keys:
          url: database_url
  clickhouse:
    external:
      secretRef:
        name: langfuse-clickhouse
        keys:
          url: url
          username: username
          password: password
  redis:
    external:
      secretRef:
        name: langfuse-redis
        keys:
          host: host
          port: port
          password: password
```

### Print Columns

```
NAME         PHASE     READY   VERSION   AGE
production   Running   true    3         5d
```
