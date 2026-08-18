# Reconcile hot-loop investigation

**Branch:** `repro/reconcile-hot-loop` · **Base:** `0549720` (0.10.0) · **Scope:** investigation and reproduction only — no controller code was modified.

## Verdicts

| # | Hypothesis | Verdict | Headline measurement |
|---|---|---|---|
| H1 | Watch topology has no event filtering | **Confirmed** | 7 controllers `For(&LangfuseInstance{})`, 0 predicates in the tree |
| H2 | Phase ping-pong is the primary loop | **Confirmed** | **660 reconciles/s**, 330 CR writes/s, `Pending↔Degraded` forever |
| H3 | Guaranteed self-trigger in `SecretController` | **Partially refuted** | 0/s in isolation (not ~1Hz); but **+1.0 extra CR write per external event** |
| H4 | `spec.replicas` tug-of-war | **Confirmed** | worker `spec.replicas` oscillates `0↔2` at **242 spec writes/s** |
| H5 | Broken change detection amplifies everything | **Confirmed, with a correction** | **16** defaulted fields omitted; 5 HPA status writes → 5 full reconciles |

Two findings the review did not predict are in [Additional findings](#additional-findings); the most useful is that the instance controller **alone** converges perfectly, which changes how the fixes should be ordered.

### Why this shows up as log volume

`langfuseinstance_controller.go` emits five **unconditional** `log.Info` lines per pass — `reconciled web deployment` ([:157](internal/controller/langfuseinstance_controller.go:157)), `reconciled web service` ([:164](internal/controller/langfuseinstance_controller.go:164)), `reconciled worker deployment` ([:171](internal/controller/langfuseinstance_controller.go:171)), `reconciled web network policy` ([:489](internal/controller/langfuseinstance_controller.go:489)), `reconciled worker network policy` ([:495](internal/controller/langfuseinstance_controller.go:495)). They log whether or not anything changed. At the measured 330 passes/s that is **~1,650 log lines/s from one controller on one CR** — which is exactly the reported symptom: ordinary log lines arriving at an obscene rate.

---

## Watch topology (H1)

`grep -rn predicate internal/ cmd/` returns **nothing**. No controller in the repo constructs a predicate, so every watch below delivers every event — including status-only writes, which is what makes the loops possible.

| Controller | File | `For()` | `Owns()` | `Watches()` | `RequeueAfter` |
|---|---|---|---|---|---|
| `LangfuseInstanceReconciler` | [:742](internal/controller/langfuseinstance_controller.go:742) | `LangfuseInstance` | `Deployment`, `StatefulSet`, `Service`, `ConfigMap`, `NetworkPolicy`, `Ingress`, **`HorizontalPodAutoscaler`**, `PodDisruptionBudget` ([:743–750](internal/controller/langfuseinstance_controller.go:743)), plus OpenShift `Route` ([:758](internal/controller/langfuseinstance_controller.go:758)) and Gateway `HTTPRoute` ([:766](internal/controller/langfuseinstance_controller.go:766)) when those APIs are present | — | none |
| `HealthMonitorReconciler` | [:352](internal/controller/health_monitor.go:352) | `LangfuseInstance` | — | — | 30s ([:133](internal/controller/health_monitor.go:133)) |
| `MigrationController` | [:248](internal/controller/migration_controller.go:248) | `LangfuseInstance` | `batchv1.Job` ([:249](internal/controller/migration_controller.go:249)) | — | 10s ([:119](internal/controller/migration_controller.go:119), [:203](internal/controller/migration_controller.go:203)) |
| `RetentionController` | [:322](internal/controller/retention_controller.go:322) | `LangfuseInstance` | — | — | 5m ([:37](internal/controller/retention_controller.go:37)) |
| `SchemaDriftController` | [:200](internal/controller/schema_drift_controller.go:200) | `LangfuseInstance` | — | — | `checkIntervalMinutes` ([:87](internal/controller/schema_drift_controller.go:87), [:152](internal/controller/schema_drift_controller.go:152)) |
| `SecretController` | [:403](internal/controller/secret_controller.go:403) | `LangfuseInstance` | — | `corev1.Secret` → `findInstancesForSecret` ([:404–407](internal/controller/secret_controller.go:404)) | none |
| `CircuitBreakerController` | [:348](internal/controller/circuit_breaker.go:348) | `LangfuseInstance` | — | — | `min(probeIntervalSeconds)`, default 15s ([:209](internal/controller/circuit_breaker.go:209)) |
| `LangfuseOrganizationReconciler` | [:428](internal/controller/langfuseorganization_controller.go:428) | `LangfuseOrganization` | — | — | 30s / `orgResyncPeriod` |
| `LangfuseProjectReconciler` | [:568](internal/controller/langfuseproject_controller.go:568) | `LangfuseProject` | `corev1.Secret` ([:569](internal/controller/langfuseproject_controller.go:569)) | — | 30s / `projectResyncPeriod` |

All nine are registered in [cmd/main.go:233–298](cmd/main.go:233). The last two watch different kinds and are not part of the fan-out.

**Fan-out factor: 7.** Any write to a `LangfuseInstance` — spec *or* status, by any controller — bumps `resourceVersion` and enqueues a reconcile in all seven. Every one of the seven then issues at least one unconditional `Status().Update` of its own ([circuit_breaker.go:199/204](internal/controller/circuit_breaker.go:199), [health_monitor.go:129](internal/controller/health_monitor.go:129), [langfuseinstance_controller.go:386](internal/controller/langfuseinstance_controller.go:386), [migration_controller.go:200](internal/controller/migration_controller.go:200), [retention_controller.go:143](internal/controller/retention_controller.go:143), [schema_drift_controller.go:147](internal/controller/schema_drift_controller.go:147), [secret_controller.go:117](internal/controller/secret_controller.go:117)), so a single bump costs seven reconciles and seven write API calls.

Whether that fan-out *sustains* depends on whether any of those seven writes changes a byte. If none do, the API server absorbs them all and the cascade stops after one round. H2, H3 and H4 are the three places where a byte does change.

---

## H2 — Phase ping-pong · **Confirmed** (primary loop)

Three controllers compute `status.phase` from incompatible inputs and write it unconditionally.

| Writer | Phase logic | Values it can produce | Never produces |
|---|---|---|---|
| `LangfuseInstanceReconciler.updateStatus()` [:361–373](internal/controller/langfuseinstance_controller.go:361) | `webReady && workerReady` from Deployment `.status.readyReplicas` | `Running`, `Error`, `Pending` | `Degraded`, `Migrating` |
| `HealthMonitorReconciler.determineOverallHealth()` [:318–328](internal/controller/health_monitor.go:318) | all six probe conditions `True` | `Running`, `Error`, `Degraded` | `Pending`, `Migrating` |
| `MigrationController` [:101](internal/controller/migration_controller.go:101), [:173](internal/controller/migration_controller.go:173), [:197](internal/controller/migration_controller.go:197) | job existence / failure | `Migrating`, `Error` | — |

Both of the first two end in an unconditional write — [langfuseinstance_controller.go:386](internal/controller/langfuseinstance_controller.go:386) and [health_monitor.go:129](internal/controller/health_monitor.go:129) — with no comparison against the stored value. Neither writer's value set contains the other's fallback, so **the two disagree in every state except fully-Running and hard-Error**:

| Cluster state | Instance ctrl writes | Health monitor writes | Agree? |
|---|---|---|---|
| Deployments ready, all probes green | `Running` | `Running` | yes |
| Fatal pod issue (bad image, missing Secret key) | `Error` | `Error` | yes |
| **Deployments not ready** (rollout, pull, scheduling) | `Pending` | `Degraded` | **no → loop** |
| **Deployments ready, a dependency probe failing** | `Running` | `Degraded` | **no → loop** |
| **Migration in flight** | `Pending` | *(skips, [:87](internal/controller/health_monitor.go:87))* | `Migrating` stomped to `Pending` by the instance controller |
| Probes green, `readyReplicas == 0` | `Pending` | `Degraded` | **no → loop** |

Row 4 is the dangerous one in production, and the hypothesis was right to flag it: `health_probes.go` dials from the operator pod, so the operator's own NetworkPolicies ([resources/networkpolicy.go](internal/resources/networkpolicy.go)) can make a perfectly healthy instance probe `False` — a fully-working Langfuse then ping-pongs `Running↔Degraded` forever. Row 5 means the migration controller sets `Migrating` at [:101](internal/controller/migration_controller.go:101) and the instance controller overwrites it on its next pass; the health monitor's `Migrating` guard at [:87](internal/controller/health_monitor.go:87) is therefore unreliable, because by the time it looks the phase is already back to `Pending`.

### Event flow

```
                    ┌──────────────────────── the loop ────────────────────────┐
                    │                                                          │
  ┌─────────────────▼─────────────────┐                                        │
  │ LangfuseInstanceReconciler        │                                        │
  │  updateStatus()  :361             │                                        │
  │  readyReplicas == 0 → Pending     │                                        │
  │  Status().Update  :386  ← uncond. │                                        │
  └─────────────────┬─────────────────┘                                        │
                    │ phase: Degraded → Pending                                │
                    ▼                                                          │
        ┌───────────────────────┐                                              │
        │ API server            │  resourceVersion++  (a byte changed,         │
        │ langfuseinstances/    │  so this is NOT absorbed as a no-op)         │
        │ status                │                                              │
        └───────────┬───────────┘                                              │
                    │ MODIFIED watch event                                     │
                    ├──────────────────────────────► + 5 other controllers     │
                    │                                  (retention, drift,      │
                    ▼                                   migration, secret, CB) │
  ┌───────────────────────────────────┐                each: 1 reconcile,      │
  │ HealthMonitorReconciler          │                 1 status write, N logs  │
  │  For() has no predicate  :352    │                                         │
  │  probes fail → determineOverall- │                                         │
  │  Health() :318 → Degraded        │                                         │
  │  Status().Update  :129  ← uncond.│                                         │
  └─────────────────┬─────────────────┘                                        │
                    │ phase: Pending → Degraded                                │
                    ▼                                                          │
        ┌───────────────────────┐                                              │
        │ API server            │  resourceVersion++                           │
        └───────────┬───────────┘                                              │
                    │ MODIFIED watch event                                     │
                    └──────────────────────────────────────────────────────────┘

  No backoff applies: both reconciles return success, so the workqueue
  re-enqueues immediately rather than rate-limiting. The only thing bounding
  the rate is how fast one pass completes (MaxConcurrentReconciles defaults
  to 1, so each controller is serialized).
```

### Measured — `H2: instance controller + health monitor`, 15s window

```
reconciles[healthmonitor]        4956  (330.40/s)
reconciles[langfuseinstance]     4955  (330.33/s)
reconciles[TOTAL]                9911  (660.73/s)
write API calls (PUT+PATCH)     19824  (1321.60/s)
CR resourceVersion bumps         4951  (330.07/s)   ← exact, from an API-server watch
distinct RVs (sampled @500ms)   30 of 30 samples    ← saturated; sampler cannot keep up
status.phase sequence            Degraded → Pending → Degraded → Pending → … (13 flips seen)
deployment churn-web             generation +0
deployment churn-worker          generation +0
```

One reconcile pair per ~3ms. The write count decomposes exactly as H5 predicts: 4,956 health-monitor passes × 1 status write, plus 4,955 instance passes × 3 writes (1 status + 1 web Deployment + 1 worker Deployment) = 19,821 ≈ 19,824 measured. Service, ConfigMap and NetworkPolicy comparisons are stable because they compare field subsets or fully-specified fields ([reconcileService](internal/controller/langfuseinstance_controller.go:315) compares only `Ports`+`Selector`; the netpol builder sets `PolicyTypes` explicitly at [networkpolicy.go:45](internal/resources/networkpolicy.go:45)).

Deployment `generation` stays at +0 because the redundant Deployment writes re-send zeroed fields that the API server re-defaults to the same values, so the stored object is unchanged. **The writes are absorbed, not amplified** — they cost API traffic and log lines, but they do not feed the loop. The loop is fed purely by the phase disagreement.

---

## H3 — `SecretController` self-trigger · **Partially refuted**

The code is where the review said it is: [secret_controller.go:110–119](internal/controller/secret_controller.go:110) sets `Status.Secrets.LastRotationCheck = &now` from a fresh `metav1.Now()` and follows it with an unconditional `Status().Update`. (The whole `Status.Secrets` struct is also rebuilt with a fresh timestamp earlier, at [:87–92](internal/controller/secret_controller.go:87).)

**But the write is not "never a no-op", and there is no self-sustaining loop.** `metav1.Time` marshals via RFC3339 with **1-second** granularity ([apimachinery `time.go`](https://github.com/kubernetes/apimachinery/blob/master/pkg/apis/meta/v1/time.go) `MarshalJSON`/`Rfc3339Copy`). Two passes inside the same wall-clock second therefore serialize to a **byte-identical** object; the API server absorbs it as a no-op, emits **no** `resourceVersion` bump and **no** watch event. `SecretController.Reconcile` returns a bare `ctrl.Result{}` with no `RequeueAfter` ([:122](internal/controller/secret_controller.go:122)), so nothing else wakes it. The burst dies after two passes.

Measured on an untouched instance, 8s window: **0 reconciles, 0 write calls, 0 RV bumps.** Not the predicted ~1Hz — zero. The hypothesis over-read `metav1.Now()` as sub-second-precise.

**The underlying defect is still real, just differently shaped.** Every pass triggered more than a second after the previous one writes a *different* timestamp, so the write lands and costs a `resourceVersion` bump that — via H1 — fans out to all seven controllers. Driving the CR with one external touch every 1.5s (standing in for a user, a GitOps sync, or any sibling controller):

```
reconciles[secret]                 20  (1.33/s)
write API calls (PUT+PATCH)        30  (2.00/s)
CR resourceVersion bumps           20  (1.33/s)
  of which external test pokes     10  → 1.0 operator writes per external event
```

**Exactly one extra CR write per external event** — the operator doubles the write rate of anything that touches the object, and each of those extra writes is a 7-way fan-out. In production, where H2 keeps the CR moving at hundreds of Hz, that means `LastRotationCheck` writes a genuinely new value ~once per second forever, so H3 contributes a steady ~1Hz × 7 = 7 reconciles/s floor *on top of* H2 — significant but two orders of magnitude below H2 itself.

**Revised priority: H3 is a real bug and a trivial fix, but it is not a primary loop.**

---

## H4 — `spec.replicas` tug-of-war · **Confirmed** (both variants)

### HPA variant — confirmed statically

Neither builder consults `Autoscaling.Enabled`; both set `Spec.Replicas` from the CR unconditionally:

- [web_deployment.go:34–37](internal/resources/web_deployment.go:34) computes `replicas` from `instance.Spec.Web.Replicas` (default 1) and assigns it at [:84](internal/resources/web_deployment.go:84).
- [worker_deployment.go:33–36](internal/resources/worker_deployment.go:33) and [:83](internal/resources/worker_deployment.go:83) do the same.

Meanwhile `reconcileHPAs` [:657–673](internal/controller/langfuseinstance_controller.go:657) creates an HPA whenever `Autoscaling.Enabled` — so the operator asks the HPA to own replicas while continuing to overwrite them. `reconcileDeployment` [:270–274](internal/controller/langfuseinstance_controller.go:270) then does a wholesale `existing.Spec = desired.Spec`, which reverts whatever the HPA scaled to. Not reproducible in envtest (no HPA controller), as expected.

### Circuit-breaker variant — reproduced

Fully in-operator, and it does loop:

1. `openCircuitBreaker` patches the worker to 0 ([:246–251](internal/controller/circuit_breaker.go:246)) and sets `Status.Worker.CircuitBreakerActive = true` ([:262](internal/controller/circuit_breaker.go:262)).
2. `updateStatus` in the instance controller **replaces the whole `Status.Worker` struct** ([:348–355](internal/controller/langfuseinstance_controller.go:348)) with a literal that has no `CircuitBreakerActive` field — wiping the flag.
3. `reconcileDeployment` scales the worker back to `spec.worker.replicas`.
4. The breaker's already-tripped guard ([:225–227](internal/controller/circuit_breaker.go:225)) reads the flag it can no longer see, so it re-trips. Forever.

```
reconciles[circuitbreaker]       3651  (243.40/s)
reconciles[langfuseinstance]     5385  (359.00/s)
reconciles[TOTAL]                9036  (602.40/s)
write API calls (PUT+PATCH)     19867  (1324.47/s)
CR resourceVersion bumps         3622  (241.47/s)
deployment churn-worker          generation +3626  (241.73 spec writes/s)
                                 spec.replicas [0 2 0 2 0 2 0 2 0 2 0 2]
```

Worse than H2 in one respect: these are *real* `spec` writes (generation +3626), so in a live cluster each one is a rollout decision — 242 scale flips per second on a real Deployment, with the ReplicaSet and pod churn that implies.

Note `status.phase` stays `Pending` throughout: the health monitor is not registered in this spec, so H4 is a loop in its own right, independent of H2.

---

## H5 — Broken change detection · **Confirmed, with a correction**

### The builders omit 16 fields the API server defaults

Rebuilding the desired Deployment and running `equality.Semantic.DeepEqual` against the live object — the exact comparison [reconcileDeployment:270](internal/controller/langfuseinstance_controller.go:270) gates its `Update` on — yields 16 permanent differences:

```
spec.strategy.type                                            live=RollingUpdate         desired=""
spec.strategy.rollingUpdate                                   live={25% 25%}             desired=nil
spec.revisionHistoryLimit                                     live=10                    desired=nil
spec.progressDeadlineSeconds                                  live=600                   desired=nil
spec.template.spec.restartPolicy                              live=Always                desired=""
spec.template.spec.terminationGracePeriodSeconds              live=30                    desired=nil
spec.template.spec.dnsPolicy                                  live=ClusterFirst          desired=""
spec.template.spec.securityContext                            live={…}                   desired=nil
spec.template.spec.schedulerName                              live=default-scheduler     desired=""
spec.template.spec.containers[0].imagePullPolicy              live=IfNotPresent          desired=""
spec.template.spec.containers[0].terminationMessagePath        live=/dev/termination-log  desired=""
spec.template.spec.containers[0].terminationMessagePolicy      live=File                  desired=""
spec.template.spec.containers[0].livenessProbe.timeoutSeconds  live=1                     desired=0
spec.template.spec.containers[0].livenessProbe.successThreshold live=1                    desired=0
spec.template.spec.containers[0].readinessProbe.timeoutSeconds live=1                     desired=0
spec.template.spec.containers[0].readinessProbe.successThreshold live=1                   desired=0
```

`equality.Semantic` only adds custom equality for `Quantity`, `Time`, `MicroTime` and the two selector types — it does **not** treat unset-vs-defaulted as equal. So the gate is open on every single pass, exactly as hypothesised. Note `imagePullPolicy` is defaulted at the *container* level even though the CRD defaults `spec.image.pullPolicy`: [web_deployment.go:41–59](internal/resources/web_deployment.go:41) never copies it onto the container.

`reconcileHPA` ([:689](internal/controller/langfuseinstance_controller.go:689)), `reconcilePDB` ([:730](internal/controller/langfuseinstance_controller.go:730)), `reconcileNetworkPolicy` ([:567](internal/controller/langfuseinstance_controller.go:567)) and `reconcileIngress` ([:514](internal/controller/langfuseinstance_controller.go:514)) share the same full-spec-comparison shape and are exposed to the same class of bug wherever their builders omit a defaulted field. `reconcileStatefulSet` ([:623–624](internal/controller/langfuseinstance_controller.go:623)) is narrower — it compares only `Spec.Template` and `Spec.Replicas` — but `Spec.Template` still carries most of the pod-level defaults listed above, so it is affected too.

### Correction: these writes are absorbed, not amplifying

The hypothesis said this "amplifies everything". It does — but in cost, not in feedback. Deployment `generation` stayed at **+0** through 4,955 instance-controller passes: the redundant `Update` re-sends zeroed fields, the API server re-defaults them to the same values, the stored object is unchanged, so there is no `resourceVersion` bump and no `Owns()` watch event. **H5 therefore multiplies the cost of every pass (2 extra write calls + 2 log lines per instance pass) but does not create or sustain a loop.** Removing H5 alone would cut API traffic roughly in half while leaving the reconcile rate untouched.

Measured directly: with the instance controller as the *only* registered controller, steady state is **0 reconciles, 0 write calls, 0 RV bumps**. On its own it converges cleanly.

### `Owns()` has no predicate — confirmed

`Owns(&autoscalingv2.HorizontalPodAutoscaler{})` at [:749](internal/controller/langfuseinstance_controller.go:749) carries no predicate. Standing in for the real HPA controller by writing only the HPA's `status`:

```
5 HPA status-only writes → 5 instance reconciles   (1:1)
```

A real HPA rewrites status roughly every 15s, so each autoscaled instance contributes ~4 full instance reconciles/min from HPA status alone — every one of them running the entire reconcile body and emitting all five log lines. The same applies to the other nine `Owns()` watches for any controller that writes those objects' status.

---

## Additional findings

**A1 — `CircuitBreakerController`'s status gate is dead code.** [circuit_breaker.go:198–207](internal/controller/circuit_breaker.go:198): the `if statusChanged` and `else` branches are byte-identical, both calling `r.Status().Update(ctx, instance)`. The `statusChanged` bool threaded through `openCircuitBreaker`/`recoverCircuitBreaker` is computed and discarded. Whoever wrote the gate intended to skip the write; the skip never happens.

**A2 — the instance controller alone is well-behaved.** Measured above: 0 reconciles at steady state. Its status write *is* idempotent once converged, because `meta.SetStatusCondition` only moves `LastTransitionTime` when the status actually changes. This matters for fix ordering: the problem is not "the instance controller is chatty", it is "two controllers disagree". A change-gate on the instance controller's write alone would fix nothing, because its write is already effectively gated by the API server — the health monitor's disagreeing write is what re-triggers it.

---

## Reproduction

[`internal/controller/reconcile_churn_test.go`](internal/controller/reconcile_churn_test.go), in the existing Ginkgo suite. Unlike every other test in the package — which calls `Reconcile()` directly and so has no watch feedback path at all — these specs start a real `ctrl.NewManager` against the envtest API server, so informers, workqueues and watch fan-out are all live.

```bash
make setup-envtest && KUBEBUILDER_ASSETS="$(pwd)/bin/k8s/1.33.0-darwin-arm64" go test ./internal/controller/ -count=1 -timeout 900s -args -ginkgo.focus="Reconcile churn" -ginkgo.v
```

`make test` runs them too, as part of the suite.

**All 5 specs fail on `main`** — that failure is the artifact. 88s total; longest single spec 33s.

| Spec | Loop | Fails on main because |
|---|---|---|
| `H2: instance controller + health monitor` | H2 | 9,911 reconciles, 4,951 self-inflicted RV bumps, 13 phase flips vs. a budget of <10 |
| `H3: secret controller alone` | H3 | +10 operator writes on top of 10 external events |
| `H5: … defaulted object` | H5 | 16 omitted defaulted fields |
| `H5: … owned HPA` | H5 | 5 status-only HPA writes → 5 full reconciles |
| `H4: instance controller + circuit breaker` | H4 | worker `spec.replicas` = `[0 2 0 2 …]` |

### How churn is measured

Three independent instruments, so no conclusion rests on controller-runtime internals being trustworthy:

1. **`controller_runtime_reconcile_total`**, per controller, as a delta over the window, from the global `sigs.k8s.io/controller-runtime/pkg/metrics`.Registry. Also `rest_client_requests_total` (method `PUT`/`PATCH`) for write calls — this is what distinguishes "the gate never fires" from "it fires and the API server absorbs the write".
2. **An API-server watch** on the CR, counting `MODIFIED` events. Exact, and independent of the operator entirely.
3. **500ms polling** of `resourceVersion`, `status.phase` and Deployment `generation`/`spec.replicas`, as the task specified. Note this **saturates**: at 330 bumps/s every one of 30 samples differs, so `distinctRVs` is a floor. Instrument 2 exists because of this.

### Test-harness notes

- `Metrics: server.Options{BindAddress: "0"}` on each manager, so concurrent managers don't clash on `:8080`.
- `Controller: config.Controller{SkipNameValidation: ptr.To(true)}` is **required**: controller-runtime's controller-name uniqueness check uses a **process-global** `usedNames` set (`pkg/controller/name.go`), and each `SetupWithManager` hardcodes its `Named()`. Without it, the second spec to register `langfuseinstance` fails. This is a test-side setting only.
- Probe targets are `127.0.0.1:1` — connection-refused, failing in microseconds. A black-holed IP would block for the full 3s `probeTimeout` ([health_probes.go:35](internal/controller/health_probes.go:35)) × 3 probes and the health monitor's own latency would dominate the window instead of the loop.
- Each spec gets its own namespace and its own manager, so the loops are isolated from one another. envtest has no namespace controller, so namespaces are never actually deleted; the CR is removed explicitly instead.
- Absolute rates are envtest-specific (local API server, no kubelet, `MaxConcurrentReconciles` = 1). The finding is that the rate is **unbounded by anything except pass latency**, not the specific figure.

---

## Remediation sketch

Not implemented — this pass is investigation and repro only. Ordered by impact.

### 1. Single ownership of `phase`/`ready`, condition-only writes from everyone else

**Fixes the primary loop.** Make `LangfuseInstanceReconciler` the sole writer of `status.phase` and `status.ready`. `HealthMonitorReconciler`, `MigrationController`, `CircuitBreakerController`, `RetentionController` and `SchemaDriftController` write **only** their own conditions and their own status sub-structs — never `phase`.

The instance controller then derives `phase` from the conditions the others publish, so `Degraded` becomes reachable from one place: `Migrating` if the migration condition is in-flight → `Error` on a fatal pod issue or a hard config error → `Running` if deployments ready **and** all dependency conditions `True` → `Degraded` if deployments ready but a dependency condition is `False` → `Pending` otherwise. Delete `determineOverallHealth` ([health_monitor.go:309](internal/controller/health_monitor.go:309)) and the phase writes at [migration_controller.go:101/173/197](internal/controller/migration_controller.go:101).

> Turns green: `H2: instance controller + health monitor`.

Worth deciding alongside this: whether the operator should probe datastores from its own pod at all ([health_probes.go](internal/controller/health_probes.go)). The operator's network path is not the workload's, so a probe failure does not imply an unhealthy instance — which is what makes the `Running`/`Degraded` disagreement in row 4 possible in the first place. Reporting probe results as an informational condition, and deriving readiness from the workload's own probes, removes the disagreement at its root rather than arbitrating it.

### 2. `GenerationChangedPredicate` on every `For()`, and gate the `Owns()` watches

Add `WithEventFilter(predicate.GenerationChangedPredicate{})` — or `For(..., builder.WithPredicates(predicate.GenerationChangedPredicate{}))` — to all seven `LangfuseInstance` controllers. `metadata.generation` only advances on spec writes, so status writes stop triggering reconciles and the fan-out factor drops from 7 to 0 for status-only churn.

Two caveats. First, controllers that legitimately react to status written by a sibling (the health monitor's `Migrating` guard at [health_monitor.go:87](internal/controller/health_monitor.go:87)) need `predicate.Or(GenerationChangedPredicate{}, <specific status predicate>)` rather than a blanket filter. Second, this is a **containment measure, not a fix**: it stops status churn from *propagating*, but a controller with a periodic `RequeueAfter` still re-runs on its own timer, and fix 1 is what makes the writes correct. Apply both.

For `Owns()`: drop the watches nothing reacts to, and predicate the rest. The instance controller only reads Deployment `.status.readyReplicas`, so `Owns(&appsv1.Deployment{})` genuinely needs status events — but the other nine (`Service`, `ConfigMap`, `NetworkPolicy`, `Ingress`, `HorizontalPodAutoscaler`, `PodDisruptionBudget`, `StatefulSet`, `Route`, `HTTPRoute`) are drift-correction watches that want `builder.WithPredicates(predicate.GenerationChangedPredicate{})`. The HPA one matters most: it is the measured ~4 reconciles/min/instance at steady state.

> Turns green: `H5: … owned HPA`. Also collapses the *propagation* half of H2/H3/H4 without fixing their root causes.

### 3. Replicas ownership rules

In `BuildWebDeployment`/`BuildWorkerDeployment`, set `Spec.Replicas` **only** when neither the HPA nor the circuit breaker owns it: skip when `Autoscaling != nil && Autoscaling.Enabled`, and skip when `Status.Worker.CircuitBreakerActive`. Kubernetes' own convention is to leave `replicas` nil in the desired object and let the live value stand — with SSA (fix 5) this falls out for free, since an unset field in the operator's applied config is simply not owned by it.

Pair this with stopping `updateStatus` from clobbering `Status.Worker`: [langfuseinstance_controller.go:348–355](internal/controller/langfuseinstance_controller.go:348) must mutate the existing struct's `ComponentStatus` fields rather than replacing the whole `WorkerComponentStatus` literal, so `CircuitBreakerActive`/`CircuitBreakerReason` survive. Without that, the breaker's already-tripped guard stays blind and it re-trips even if replicas are left alone.

> Turns green: `H4: instance controller + circuit breaker`.

### 4. Change-gated status writes, and remove the per-pass timestamp

Give every controller a single helper that compares the status it is about to write against what it read and skips the `Update` when nothing changed — instead of the current unconditional writes at [circuit_breaker.go:199/204](internal/controller/circuit_breaker.go:199), [health_monitor.go:129](internal/controller/health_monitor.go:129), [langfuseinstance_controller.go:386](internal/controller/langfuseinstance_controller.go:386), [migration_controller.go:200](internal/controller/migration_controller.go:200), [retention_controller.go:143](internal/controller/retention_controller.go:143), [schema_drift_controller.go:147](internal/controller/schema_drift_controller.go:147), [secret_controller.go:117](internal/controller/secret_controller.go:117). Fold A1's dead branch into it. This is defence in depth: the API server already absorbs identical writes, so the win is API traffic and log volume, not loop suppression.

Then delete the `LastRotationCheck` timestamp write at [secret_controller.go:110–115](internal/controller/secret_controller.go:110) — and the fresh timestamp in the struct rebuild at [:87–92](internal/controller/secret_controller.go:87). A "when did we last look" timestamp that updates on every pass carries no information a reader can use and is precisely what makes the write non-idempotent. If the field must stay for API compatibility, set it only when the secret's content hash actually changes.

> Turns green: `H3: secret controller alone`. Also removes the residual ~1Hz × 7 floor once H2 is fixed.

### 5. Field-level updates or SSA instead of `existing.Spec = desired.Spec`

Replace the `DeepEqual`-then-wholesale-assign pattern in `reconcileDeployment` ([:270](internal/controller/langfuseinstance_controller.go:270)), `reconcileStatefulSet` ([:623](internal/controller/langfuseinstance_controller.go:623)), `reconcileHPA` ([:689](internal/controller/langfuseinstance_controller.go:689)), `reconcilePDB` ([:730](internal/controller/langfuseinstance_controller.go:730)), `reconcileNetworkPolicy` ([:567](internal/controller/langfuseinstance_controller.go:567)) and `reconcileIngress` ([:514](internal/controller/langfuseinstance_controller.go:514)) with Server-Side Apply — `r.Patch(ctx, desired, client.Apply, client.FieldOwner("langfuse-operator"), client.ForceOwnership)`. SSA compares only the fields the operator declares, so API-server defaults and other field managers' values are invisible to it and the 16-field problem disappears by construction. It also makes fix 3 fall out naturally and removes the need for `preservePodTemplateAnnotations` ([:282](internal/controller/langfuseinstance_controller.go:282)), since the secret controller becomes a separate field owner.

If SSA is too large a change for one release, the interim fix is to compare only operator-owned fields (replicas, selector, template's image/env/resources/probes/volumes) rather than the whole spec — but that list needs maintaining, which is exactly how the current bug arose.

> Turns green: `H5: change detection against a defaulted object`. Roughly halves steady-state write traffic.

### Sequencing

Fixes **1** and **2** together stop the bleeding: 1 removes the disagreement that generates the writes, 2 stops any residual write from fanning out 7-way. Either alone leaves a loop — 1 without 2 still lets a real spec change cost 7 reconciles; 2 without 1 leaves the phase writers fighting on the health monitor's own 30s timer. Ship both, then **3** (a correctness bug independent of churn — the breaker is currently non-functional), then **4** and **5** as cost reduction.
