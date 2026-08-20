# Troubleshooting

When a component won't start, the operator reports the underlying pod failure
directly on the `LangfuseInstance` — you shouldn't need to go hunting through
pods to find out why.

## Start with the CR

```bash
kubectl describe langfuseinstance production -n langfuse
```

The `phase` tells you how to read the situation:

| Phase | Meaning |
|---|---|
| `Pending` | Components are still starting. Normal shortly after create or upgrade. |
| `Migrating` | The migration Job is running. |
| `Running` | All components ready and dependencies reachable. |
| `Degraded` | Something is unhealthy but may still recover on its own (crash loop while a dependency comes up, a failed connectivity probe). |
| `Error` | **Needs you.** A misconfiguration that will never resolve by itself — a bad image reference, a missing Secret key, or a datastore target that moved after migrations ran. |

The `Degraded` vs `Error` split is the important one: `Error` means waiting
longer will not help.

## Reconciliation is frozen

`Error` with a `DatastoreTargetUnchanged` condition set to `False` is a
deliberate stop, not a failure:

```yaml
  conditions:
  - type: DatastoreTargetUnchanged
    status: "False"
    reason: TargetChangedAfterMigration
    message: 'Not reconciling workloads: spec.database "secret/pg#url=database_url,directUrl="
      -> "secret/pg-new#url=database_url,directUrl=" changed after migrations ran.
      Revert the change, or create a separate LangfuseInstance for the new target'
```

The spec now points at datastores the existing schema does not live in, so the
operator stops before touching any child resource and leaves the pods running
what they were migrated for. `status.version` and `status.publicUrl` keep
describing what is actually deployed, so they will disagree with the spec while
this is set — that is the point.

Revert the change and reconciliation resumes on the next pass. If the new target
is what you actually want, create a separate `LangfuseInstance` for it and cut
over once it has migrated; see
[Changing the datastore target](../reference/langfuseinstance.md#changing-the-datastore-target)
for what counts as a change — the Secret *name* and the key names that select an
endpoint do, the values behind them do not.

## Reading pod issues

`status.web.issues`, `status.worker.issues`, and `status.migration.issues` list
the pod-level problems blocking each component:

```yaml
status:
  phase: Error
  ready: false
  web:
    replicas: 2
    readyReplicas: 0
    issues:
    - pod: production-web-7d9f4c8b6-abc12
      container: langfuse-web
      reason: CreateContainerConfigError
      message: 'secret "production-generated-secrets" key "admin-api-key" not found'
      fatal: true
  conditions:
  - type: WebReady
    status: "False"
    reason: CreateContainerConfigError
    message: 'Web deployment has 0/2 ready replicas; production-web-7d9f4c8b6-abc12
      (langfuse-web): CreateContainerConfigError — secret "production-generated-secrets"
      key "admin-api-key" not found'
```

The same detail lands on the `WebReady` / `WorkerReady` / `MigrationsComplete`
conditions, so `kubectl describe` surfaces it without digging into YAML.

::: tip
Issues are only populated while a component is not ready, and are capped at five
entries — status is a diagnosis, not a log.
:::

## Common reasons

| Reason | Fatal | Typical cause |
|---|---|---|
| `CreateContainerConfigError` | ✅ | A referenced Secret or key doesn't exist. Check the `secretRef` names/keys in your spec, and that the Secret is in the same namespace. |
| `ImagePullBackOff` / `ErrImagePull` | ✅ | Wrong `spec.image.tag`, or a private registry needs `spec.image.pullSecrets`. |
| `InvalidImageName` | ✅ | Malformed repository or tag. |
| `CrashLoopBackOff` | ❌ | The container starts then exits. The message includes the previous run's exit code and output — a Langfuse `ZodError` here means a required env var is missing. Often transient while Postgres/ClickHouse are still starting. |
| `OOMKilled` | ❌ | Raise `spec.web.resources.limits.memory` / `spec.worker.resources.limits.memory`. |
| `Unschedulable` | ❌ | No node satisfies the pod — insufficient CPU/memory, or unsatisfiable `nodeSelector`/`affinity`/`tolerations`. |

## Connection Secrets

A `ConfigError` on `DatabaseReady` or `ClickHouseReady` means the operator could
not make sense of the connection string — it never got as far as dialling, so
this is not a network problem. Two cases account for most of them:

**A `/` or `?` in a PostgreSQL password.** Both end the URL's authority, so the
host after them is invisible to every parser. The probe says so rather than
dialling nonsense:

```
invalid port "somepassword" — percent-encode '/' as %2F and '?' as %3F in the credentials; '@' and ':' need no encoding
```

**An `@` is not one of these cases.** Prisma takes the last `@` in the authority
as the credential separator, and so do the migration Job's init container and
this probe, so a password containing one connects normally. If you are looking at
a `DatabaseReady` condition that reads `net/url: invalid userinfo`, that is the
pre-0.10.0 probe — the operator is running an older image than you think. Check
what is actually deployed:

```bash
kubectl get pod -n langfuse-operator-system -l control-plane=controller-manager -o jsonpath='{.items[*].status.containerStatuses[*].imageID}'
```

A rebuilt image pushed under a tag that already exists on the node will not be
pulled again unless the tag is new or `imagePullPolicy` is `Always`.

**A path or query on the ClickHouse URL.** ClickHouse's HTTP interface picks a
database from a parameter, never from a path segment, so `http://ch:8123/langfuse`
does not do what it looks like — it breaks every application query, and made the
probe's `/ping` return 404:

```
Invalid ClickHouse URL: must not contain a path (got "/langfuse") — select the database with CLICKHOUSE_DB, not a URL path
```

Use a bare origin (`http://ch:8123`) and set
[`clickhouse.database`](../reference/langfuseinstance.md#clickhousespec) instead.

If `status.database` and `status.clickhouse` are missing entirely rather than
reporting `connected: false`, the health monitor has not completed a pass at all
— check the operator's own logs.

## Migrations

A migration that fails or hangs reports through `status.migration` and the
`MigrationsComplete` condition:

```bash
kubectl get langfuseinstance production -n langfuse -o jsonpath='{.status.migration}' | jq
```

The Job's `wait-for-stores` init container blocks until PostgreSQL and
ClickHouse accept TCP connections, so an init container stuck here means the
migration cannot reach a datastore — check the connection Secret and, if you use
`spec.security.networkPolicy`, that egress to the datastore's port is allowed.

### No Job appears at all

The operator does not run a migration on every reconcile, so an absent Job is
usually one of:

- **It already ran.** `status.migration.appliedIdentity` records what the last
  successful migration ran against. While it still matches the spec there is
  nothing to do, and finished Jobs are removed 3600s after completion — so a
  successful migration leaves no Job behind for long.
- **`spec.database.migration.runOnDeploy` is `false`.** Nothing will run until
  you set it back.
- **The target moved.** `MigrationsComplete` reports `MigrationFailed` with the
  components that changed, and no Job is created — see
  [Reconciliation is frozen](#reconciliation-is-frozen).

A version bump is what legitimately re-opens the gate. Deleting and re-applying
the `LangfuseInstance` also works in development, since a fresh CR has no
recorded identity — but Langfuse's migrations are idempotent, so the usual reason
to want a re-run (tables missing, `SchemaDriftChecked` reporting
`TablesMissing`) is better answered by checking that the migration reached the
database you think it did. Note that no Langfuse migration issues
`CREATE DATABASE`: `clickhouse.database` must already exist.

## When you still need the pods

The operator surfaces reasons, not logs. For full output:

```bash
kubectl logs -n langfuse -l app.kubernetes.io/instance=production,app.kubernetes.io/component=web
kubectl logs -n langfuse -l app.kubernetes.io/instance=production,app.kubernetes.io/component=worker
kubectl logs -n langfuse -l app.kubernetes.io/instance=production,app.kubernetes.io/component=migration
```

Events on the CR record transitions over time:

```bash
kubectl get events -n langfuse --field-selector involvedObject.name=production
```
