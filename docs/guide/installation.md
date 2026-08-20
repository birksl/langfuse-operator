# Installation

## Prerequisites

- Kubernetes 1.26+ cluster
- `kubectl` configured for your cluster
- Cluster-admin privileges (for CRD installation)

## Install with OLM

If your cluster has the [Operator Lifecycle Manager](https://olm.operatorframework.io/) installed (e.g., OpenShift):

```bash
# Create a CatalogSource
kubectl apply -f - <<EOF
apiVersion: operators.coreos.com/v1alpha1
kind: CatalogSource
metadata:
  name: langfuse-operator-catalog
  namespace: olm
spec:
  sourceType: grpc
  image: ghcr.io/PalenaAI/langfuse-operator-catalog:latest
  displayName: Langfuse Operator
EOF

# Create a Subscription
kubectl apply -f - <<EOF
apiVersion: operators.coreos.com/v1alpha1
kind: Subscription
metadata:
  name: langfuse-operator
  namespace: operators
spec:
  channel: stable
  name: langfuse-operator
  source: langfuse-operator-catalog
  sourceNamespace: olm
EOF
```

## Install with Helm

For clusters without OLM:

```bash
helm install langfuse-operator deploy/charts/langfuse-operator \
  --namespace langfuse-operator-system \
  --create-namespace
```

The chart defaults to the image tag matching its `appVersion` (e.g. `v0.10.0`). To pin an older release, pass `--set image.tag=v0.6.4`.

See the [chart values](https://github.com/PalenaAI/langfuse-operator/blob/main/deploy/charts/langfuse-operator/values.yaml) for all configuration options (replicas, resources, tolerations, affinity, etc.).

### Operator logging

The operator binary is built with zap's development mode on, which on its own logs every health probe at debug level and encodes lines for human eyes. The chart overrides both:

| Value | Default | Notes |
|---|---|---|
| `logLevel` | `info` | `debug`, `info`, `error`, or a zap verbosity integer. Set `debug` while diagnosing an instance — most of that volume is per-probe health and circuit-breaker lines |
| `logFormat` | `json` | `json` or `console`. Anything else fails the render rather than reaching the pod |

`console` is worth knowing about: development mode's own encoder prefixes each line with a tab-separated timestamp and level and renders only the *fields* as JSON, which is close enough to fool a log shipper expecting one object per line. Set an empty string for either value to leave the binary's built-in default alone.

### Namespace-Scoped Install

By default the operator watches all namespaces. To restrict it to specific namespaces:

```bash
helm install langfuse-operator deploy/charts/langfuse-operator \
  --namespace langfuse-operator-system \
  --create-namespace \
  --set-string watchNamespaces="langfuse\,langfuse-staging"
```

This sets the `WATCH_NAMESPACE` environment variable on the operator pod. The operator will only cache and reconcile resources in the listed namespaces.

::: tip
OLM automatically injects `WATCH_NAMESPACE` when the operator is installed in **OwnNamespace** or **SingleNamespace** mode.
:::

## Install with Manifests

Apply the raw manifests directly:

```bash
kubectl apply --server-side -f https://raw.githubusercontent.com/PalenaAI/langfuse-operator/main/dist/install.yaml
```

::: warning Server-side apply is required
The `LangfuseInstance` CRD is larger than the 262144-byte limit on the
`kubectl.kubernetes.io/last-applied-configuration` annotation that **client-side**
apply writes, so a plain `kubectl apply -f` fails with `metadata.annotations: Too long`.
Server-side apply tracks ownership in `managedFields` instead and has no such limit.

Upgrading a cluster that was previously installed client-side may additionally need
`--force-conflicts` to take over field ownership. Helm and OLM installs are unaffected.
:::

Or build from source:

```bash
git clone https://github.com/PalenaAI/langfuse-operator.git
cd langfuse-operator
make install   # Install CRDs
make deploy    # Deploy the operator
```

## Verify Installation

```bash
# Check the operator pod is running
kubectl get pods -n langfuse-operator-system

# Check CRDs are installed
kubectl get crds | grep langfuse
```

Expected CRDs:

```
langfuseinstances.langfuse.palena.ai
langfuseorganizations.langfuse.palena.ai
langfuseprojects.langfuse.palena.ai
```

## Uninstall

```bash
# Remove all Langfuse CRs first (this triggers cleanup)
kubectl delete langfuseinstances --all -A
kubectl delete langfuseprojects --all -A
kubectl delete langfuseorganizations --all -A

# Then remove the operator
make undeploy   # or helm uninstall / delete OLM subscription
make uninstall  # remove CRDs
```

::: warning
Deleting CRDs will remove **all** Langfuse custom resources and their owned objects (Deployments, Services, Secrets, etc.). Always delete CRs before CRDs to ensure clean finalization.
:::
