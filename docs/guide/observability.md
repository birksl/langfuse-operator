# Observability

## Operator Metrics

The operator's metrics endpoint is **disabled by default** — `--metrics-bind-address` defaults to `0`, and the Helm chart neither overrides it nor creates a Service for it. The chart exposes no value for the flag either, so enabling metrics today means patching the Deployment's `args` (and adding a Service plus a ServiceMonitor pointing at the operator itself):

```
--metrics-bind-address=:8443     # HTTPS, protected by authn/authz
--metrics-bind-address=:8080 --metrics-secure=false
```

What you get is controller-runtime's standard set, which is what actually matters for operating this operator:

| Metric | Type | Why you want it |
|---|---|---|
| `controller_runtime_reconcile_total` | Counter | Reconciles by controller and result. A rate that climbs while nothing changes is a reconcile loop |
| `controller_runtime_reconcile_errors_total` | Counter | Errors by controller |
| `controller_runtime_reconcile_time_seconds` | Histogram | Reconcile duration by controller |
| `workqueue_depth`, `workqueue_adds_total`, `workqueue_retries_total` | Gauge / Counter | Queue pressure and requeue storms per controller |
| `rest_client_requests_total` | Counter | API-server calls by verb, code and host — the cost side of a busy loop |
| `go_*`, `process_*` | — | Standard Go runtime and process metrics |

::: warning `langfuse_operator_*` and `langfuse_instance_*` do not work
Four `langfuse_operator_*` series (`reconcile_total`, `reconcile_errors_total`, `reconcile_duration_seconds`, `managed_instances`) are registered but never recorded, so they export nothing beyond a zero gauge. The `langfuse_instance_*` series described in older docs — replica counts, queue depth, ClickHouse storage, circuit-breaker state — were never implemented at all. Read those values from `status` instead; the operator keeps them there.
:::

## ServiceMonitor

```yaml
spec:
  observability:
    serviceMonitor:
      enabled: true
      interval: "30s"
      labels:
        release: prometheus       # match your Prometheus selector
```

This creates a `monitoring.coreos.com/v1` ServiceMonitor selecting the instance's **web** pods, scraping port `http` at `/api/public/health` on the configured interval.

::: warning That is a health endpoint, not a metrics endpoint
`/api/public/health` returns JSON. Prometheus cannot parse it as the text exposition format, so the target will report as down rather than yielding series. Langfuse OSS exposes no Prometheus endpoint — use OTEL below for Langfuse's own telemetry, and treat this ServiceMonitor as a liveness signal at best.
:::

## OpenTelemetry

Send traces from Langfuse itself to an OTEL collector:

```yaml
spec:
  observability:
    otel:
      enabled: true
      endpoint: "otel-collector.monitoring.svc:4317"
      protocol: grpc       # grpc | http
```

This sets `OTEL_EXPORTER_OTLP_ENDPOINT` and `OTEL_EXPORTER_OTLP_PROTOCOL` on both components.

## Operator Logs

The chart pins the operator to `info` and JSON — the binary is built with zap development mode, which would otherwise log every health probe at debug. See [Operator logging](./installation.md#operator-logging).

## Langfuse Telemetry

Langfuse sends anonymous usage telemetry by default. To disable:

```yaml
spec:
  security:
    telemetry:
      enabled: false
```
