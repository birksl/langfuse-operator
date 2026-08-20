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

There are no `langfuse_operator_*` or `langfuse_instance_*` series. Four `langfuse_operator_*` metrics were registered but never recorded by any controller, so they only ever exported an empty counter and a zero gauge; they have been removed rather than left as a trap. The `langfuse_instance_*` series described in older docs — replica counts, queue depth, ClickHouse storage, circuit-breaker state — were never implemented at all. Read those from `status`, where the operator does keep them.

## ServiceMonitor (removed)

`spec.observability.serviceMonitor` is deprecated, ignored, and goes away in 0.11.0.

Langfuse OSS serves no Prometheus endpoint, so the ServiceMonitor this used to create could only name the web pod's `/api/public/health` — a JSON route Prometheus cannot parse as the text exposition format. The result was a target permanently reported as down, which reads as an unreachable instance rather than as a misconfiguration.

Setting the field now raises the `Deprecated` condition and creates nothing. If an earlier version created one for the instance, the operator **deletes** it, so upgrading clears the stuck target on its own. Use OTEL below for Langfuse's own telemetry.

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
