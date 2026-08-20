# Architecture

## Overview

The Langfuse Operator follows the standard Kubernetes operator pattern: it watches custom resources and reconciles the cluster state to match the desired state declared in those resources.

```
┌─────────────────────────────────────────────────────────┐
│                    Kubernetes Cluster                     │
│                                                          │
│  ┌──────────────────┐       ┌────────────────────────┐  │
│  │  LangfuseInstance │──────▶│  Instance Controller    │  │
│  │       CR          │       │  ├─ Web Deployment      │  │
│  └──────────────────┘       │  ├─ Worker Deployment   │  │
│                              │  ├─ Services            │  │
│  ┌──────────────────┐       │  ├─ Ingress / Route     │  │
│  │ LangfuseOrg CR   │──┐   │  ├─ HPA, PDB            │  │
│  └──────────────────┘  │   │  └─ NetworkPolicy        │  │
│                         │   └────────────────────────┘  │
│  ┌──────────────────┐  │   ┌────────────────────────┐  │
│  │ LangfuseProject  │──┼──▶│  Auxiliary Controllers   │  │
│  │       CR          │  │   │  ├─ Migration            │  │
│  └──────────────────┘  │   │  ├─ Health Monitor       │  │
│                         │   │  ├─ Secret Rotation      │  │
│                         │   │  ├─ Retention            │  │
│                         │   │  ├─ Schema Drift         │  │
│                         │   │  └─ Circuit Breaker      │  │
│                         │   └────────────────────────┘  │
│                         │   ┌────────────────────────┐  │
│                         └──▶│  Multi-Tenancy          │  │
│                              │  ├─ Org Controller      │  │
│                              │  └─ Project Controller  │  │
│                              └────────────────────────┘  │
└─────────────────────────────────────────────────────────┘
```

## Controllers

### LangfuseInstance Controller

The primary controller. It reconciles the `LangfuseInstance` CR and manages:

- **Web Deployment** &mdash; Langfuse frontend and API (port 3000)
- **Worker Deployment** &mdash; background job processor
- **Web Service** &mdash; ClusterIP service exposing the Web component
- **Config generation** &mdash; computes ~50+ environment variables from the spec

Both Web and Worker use the same container image (`langfuse/langfuse`), differentiated by the `LANGFUSE_WORKER_ENABLED` environment variable.

### Auxiliary Controllers

Each auxiliary controller watches `LangfuseInstance` but focuses on a single concern:

| Controller | Responsibility |
|---|---|
| **Migration** | Runs migration Jobs when the version changes, and refuses a datastore target the existing schema does not live in |
| **Health Monitor** | Periodic datastore probes and Deployment readiness, published as status conditions |
| **Secret Controller** | Auto-generates secrets, detects rotation, triggers restarts |
| **Retention** | Applies ClickHouse TTL policies and reports storage pressure |
| **Schema Drift** | Checks that Langfuse's ClickHouse tables exist |
| **Circuit Breaker** | Scales down workers when dependencies fail |

`status.phase` and `status.ready` are derived in one place, from the conditions these controllers publish — no controller writes the phase directly. Two of them computing it from different inputs is what produced a `Pending`↔`Degraded` write loop that generated most of a cluster's operator log volume.

### Multi-Tenancy Controllers

- **Organization Controller** &mdash; syncs `LangfuseOrganization` CRs to Langfuse via the Admin API
- **Project Controller** &mdash; syncs `LangfuseProject` CRs, manages API keys and stores them as Kubernetes Secrets

## Resource Ownership

All resources created by the operator carry an owner reference back to the parent CR. This enables automatic garbage collection when a CR is deleted.

```
LangfuseInstance
├── Deployment: <name>-web
├── Deployment: <name>-worker
├── Service: <name>-web
├── Secret: <name>-generated-secrets
├── StatefulSet: <name>-clickhouse (if managed)
├── Service: <name>-clickhouse (if managed)
├── ConfigMap: <name>-clickhouse (if managed)
├── StatefulSet: <name>-redis (if managed)
├── Service: <name>-redis (if managed)
├── Job: <name>-migration-<version> (on upgrade)
├── Ingress: <name>-web (if enabled)
├── HPA: <name>-web (if autoscaling)
├── PDB: <name>-web (if enabled)
├── ServiceMonitor: <name>-web (if enabled)
└── NetworkPolicy: <name> (if enabled)

LangfuseProject
├── Secret: <api-key-secret-1>
└── Secret: <api-key-secret-2>
```

## Reconciliation Flow

Each reconcile loop follows this pattern:

1. Fetch the CR
2. Validate the spec
3. Build the desired state (resource builders in `internal/resources/`)
4. Apply changes with `CreateOrUpdate` (idempotent)
5. Update status conditions
6. Return result (requeue if in-progress, done if stable)

The operator never makes destructive changes without explicit spec changes. Reconciliation is idempotent and safe to retry.

## Labeling Convention

All managed resources carry standard Kubernetes labels:

```yaml
app.kubernetes.io/name: langfuse
app.kubernetes.io/instance: <instance-name>
app.kubernetes.io/component: web | worker | migration | clickhouse | redis
app.kubernetes.io/managed-by: langfuse-operator
app.kubernetes.io/part-of: langfuse
langfuse.palena.ai/instance: <instance-name>
```
