/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controller

import (
	"context"
	"fmt"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	policyv1 "k8s.io/api/policy/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/apiutil"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	v1alpha1 "github.com/PalenaAI/langfuse-operator/api/v1alpha1"
	"github.com/PalenaAI/langfuse-operator/internal/langfuse"
	"github.com/PalenaAI/langfuse-operator/internal/resources"
)

const (
	phasePending   = "Pending"
	phaseRunning   = "Running"
	phaseMigrating = "Migrating"
	phaseDegraded  = "Degraded"
	phaseError     = "Error"

	// conditionTypeReady is the status condition type set on every CR in
	// this group; centralising the literal avoids drift across reconcilers.
	conditionTypeReady = "Ready"

	// conditionTypeDeprecated warns that the spec uses fields scheduled for
	// removal, so users find out from the CR rather than from a failed upgrade.
	conditionTypeDeprecated = "Deprecated"

	// conditionTypeDatastoreTarget reports that the spec points at a datastore
	// the current schema does not live in, so workload reconciliation is held.
	conditionTypeDatastoreTarget = "DatastoreTargetUnchanged"

	// fieldOwnerInstance identifies this controller in managedFields. Sibling
	// controllers must use a different owner, or server-side apply prunes the
	// fields they set but this one does not declare.
	fieldOwnerInstance = "langfuse-operator"
)

// LangfuseInstanceReconciler reconciles a LangfuseInstance object
type LangfuseInstanceReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// +kubebuilder:rbac:groups=langfuse.palena.ai,resources=langfuseinstances,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=langfuse.palena.ai,resources=langfuseinstances/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=langfuse.palena.ai,resources=langfuseinstances/finalizers,verbs=update
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=core,resources=services,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=core,resources=secrets,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=networking.k8s.io,resources=networkpolicies,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=networking.k8s.io,resources=ingresses,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=route.openshift.io,resources=routes,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=gateway.networking.k8s.io,resources=httproutes,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=apps,resources=statefulsets,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=core,resources=configmaps,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=core,resources=persistentvolumeclaims,verbs=get;list;watch
// +kubebuilder:rbac:groups=core,resources=pods,verbs=get;list;watch
// +kubebuilder:rbac:groups=autoscaling,resources=horizontalpodautoscalers,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=policy,resources=poddisruptionbudgets,verbs=get;list;watch;create;update;patch;delete
// ServiceMonitors are only ever removed now, so delete is the only verb left.
// +kubebuilder:rbac:groups=monitoring.coreos.com,resources=servicemonitors,verbs=delete
// +kubebuilder:rbac:groups=core,resources=events,verbs=create;patch

func (r *LangfuseInstanceReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	// 1. Fetch the LangfuseInstance CR
	instance := &v1alpha1.LangfuseInstance{}
	if err := r.Get(ctx, req.NamespacedName, instance); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, fmt.Errorf("fetching LangfuseInstance: %w", err)
	}

	// If the instance is being deleted, stop reconciling. Owner references
	// drive garbage collection of every child resource. Continuing to
	// reconcile would re-create Deployments that the GC is trying to delete,
	// fighting `foregroundDeletion` until the CR is wedged.
	if !instance.DeletionTimestamp.IsZero() {
		log.Info("instance is being deleted, skipping reconcile")
		return ctrl.Result{}, nil
	}

	original := instance.DeepCopy()

	// 2. Set initial phase
	if instance.Status.Phase == "" {
		instance.Status.Phase = phasePending
	}

	// 2b. Stop before touching any child resource if the datastore target has
	// moved since the last successful migration. The migration controller refuses
	// such a change, but its return only stops itself — this controller would
	// still build config from the new spec and roll Web/Worker onto a target that
	// has no schema. Freezing the workload where it is keeps the instance serving
	// until the spec is reverted, or the new target is given its own instance.
	if changed := retargetedComponents(appliedMigrationIdentity(instance), migrationIdentity(instance)); len(changed) > 0 {
		meta.SetStatusCondition(&instance.Status.Conditions, metav1.Condition{
			Type:   conditionTypeDatastoreTarget,
			Status: metav1.ConditionFalse,
			Reason: "TargetChangedAfterMigration",
			Message: fmt.Sprintf("Not reconciling workloads: %s changed after migrations ran. "+
				"Revert the change, or create a separate LangfuseInstance for the new target",
				strings.Join(changed, " and ")),
			ObservedGeneration: instance.Generation,
		})
		if statusErr := r.updateStatus(ctx, instance, original, false); statusErr != nil {
			log.Error(statusErr, "failed to update status")
		}
		log.Error(nil, "refusing to reconcile workloads onto a retargeted datastore", "changed", changed)
		return ctrl.Result{}, nil
	}
	meta.RemoveStatusCondition(&instance.Status.Conditions, conditionTypeDatastoreTarget)

	// 3. Build env var config
	config, err := langfuse.BuildConfig(instance)
	if err != nil {
		meta.SetStatusCondition(&instance.Status.Conditions, metav1.Condition{
			Type:               conditionTypeReady,
			Status:             metav1.ConditionFalse,
			Reason:             "ConfigError",
			Message:            err.Error(),
			ObservedGeneration: instance.Generation,
		})
		if statusErr := updateInstanceStatus(ctx, r.Client, instance, original); statusErr != nil {
			log.Error(statusErr, "failed to update status")
		}
		return ctrl.Result{}, fmt.Errorf("building config: %w", err)
	}

	// 3z. Warn about deprecated spec fields. Purely informational — it must not
	// block reconciliation, since these modes still work until 0.11.0.
	setDeprecationCondition(ctx, instance)

	// 3a. Reconcile managed ClickHouse
	if instance.Spec.ClickHouse != nil && instance.Spec.ClickHouse.Managed != nil {
		if err := r.reconcileClickHouse(ctx, instance); err != nil {
			return ctrl.Result{}, fmt.Errorf("reconciling clickhouse: %w", err)
		}
		log.Info("reconciled managed clickhouse")
	}

	// 3b. Reconcile managed Redis
	if instance.Spec.Redis != nil && instance.Spec.Redis.Managed != nil {
		if err := r.reconcileRedis(ctx, instance); err != nil {
			return ctrl.Result{}, fmt.Errorf("reconciling redis: %w", err)
		}
		log.Info("reconciled managed redis")
	}

	// 4. Reconcile Web Deployment
	webDeploy := resources.BuildWebDeployment(instance, config)
	if err := r.apply(ctx, instance, webDeploy); err != nil {
		return ctrl.Result{}, fmt.Errorf("reconciling web deployment: %w", err)
	}
	log.Info("reconciled web deployment", "name", webDeploy.Name)

	// 5. Reconcile Web Service
	webSvc := resources.BuildWebService(instance)
	if err := r.apply(ctx, instance, webSvc); err != nil {
		return ctrl.Result{}, fmt.Errorf("reconciling web service: %w", err)
	}
	log.Info("reconciled web service", "name", webSvc.Name)

	// 6. Reconcile Worker Deployment
	workerDeploy := resources.BuildWorkerDeployment(instance, config)
	if err := r.apply(ctx, instance, workerDeploy); err != nil {
		return ctrl.Result{}, fmt.Errorf("reconciling worker deployment: %w", err)
	}
	log.Info("reconciled worker deployment", "name", workerDeploy.Name)

	// 7. Reconcile networking (NetworkPolicies, Ingress, Route, HTTPRoute)
	if err := r.reconcileNetworking(ctx, instance); err != nil {
		return ctrl.Result{}, err
	}

	// 8. Reconcile platform resources (HPA, PDB, ServiceMonitor)
	if err := r.reconcilePlatform(ctx, instance); err != nil {
		return ctrl.Result{}, err
	}

	// 14. Update status
	if err := r.updateStatus(ctx, instance, original, true); err != nil {
		return ctrl.Result{}, fmt.Errorf("updating status: %w", err)
	}

	return ctrl.Result{}, nil
}

func (r *LangfuseInstanceReconciler) reconcileNetworking(ctx context.Context, instance *v1alpha1.LangfuseInstance) error {
	log := logf.FromContext(ctx)

	if err := r.reconcileNetworkPolicies(ctx, instance); err != nil {
		return fmt.Errorf("reconciling network policies: %w", err)
	}

	if instance.Spec.Ingress != nil && instance.Spec.Ingress.Enabled {
		ingress := resources.BuildIngress(instance)
		if err := r.apply(ctx, instance, ingress); err != nil {
			return fmt.Errorf("reconciling ingress: %w", err)
		}
		log.Info("reconciled ingress", "name", ingress.Name)
	}

	if instance.Spec.Route != nil && instance.Spec.Route.Enabled {
		route := resources.BuildRoute(instance)
		if err := r.apply(ctx, instance, route); err != nil {
			return fmt.Errorf("reconciling route: %w", err)
		}
		log.Info("reconciled openshift route", "name", route.GetName())
	}

	if instance.Spec.GatewayAPI != nil && instance.Spec.GatewayAPI.Enabled {
		httpRoute := resources.BuildHTTPRoute(instance)
		if err := r.apply(ctx, instance, httpRoute); err != nil {
			return fmt.Errorf("reconciling httproute: %w", err)
		}
		log.Info("reconciled httproute", "name", httpRoute.GetName())
	}

	return nil
}

func (r *LangfuseInstanceReconciler) reconcilePlatform(ctx context.Context, instance *v1alpha1.LangfuseInstance) error {
	if err := r.reconcileHPAs(ctx, instance); err != nil {
		return fmt.Errorf("reconciling HPAs: %w", err)
	}

	if err := r.reconcilePDBs(ctx, instance); err != nil {
		return fmt.Errorf("reconciling PDBs: %w", err)
	}

	if instance.Spec.Observability != nil && instance.Spec.Observability.ServiceMonitor != nil {
		if err := r.removeRetiredServiceMonitor(ctx, instance); err != nil {
			return fmt.Errorf("removing retired servicemonitor: %w", err)
		}
	}

	return nil
}

// removeRetiredServiceMonitor deletes the ServiceMonitor earlier versions
// created for this instance.
//
// Langfuse serves no Prometheus endpoint, so the only thing that
// ServiceMonitor could name was the web pod's /api/public/health — a JSON route
// Prometheus cannot parse. The target reported down permanently, which is worse
// than no target: it looks like the instance is unreachable. Rather than leave
// that behind when the field was retired, remove it; setDeprecationCondition
// tells the user why and points at spec.observability.otel.
//
// Absent Prometheus-operator CRDs are not an error — then there is nothing to
// remove either.
func (r *LangfuseInstanceReconciler) removeRetiredServiceMonitor(ctx context.Context, instance *v1alpha1.LangfuseInstance) error {
	sm := &unstructured.Unstructured{}
	sm.SetGroupVersionKind(schema.GroupVersionKind{
		Group: "monitoring.coreos.com", Version: "v1", Kind: "ServiceMonitor",
	})
	sm.SetNamespace(instance.Namespace)
	sm.SetName(resources.ServiceMonitorName(instance))

	err := r.Delete(ctx, sm)
	switch {
	case err == nil:
		logf.FromContext(ctx).Info("removed the retired web ServiceMonitor", "name", sm.GetName())
		return nil
	case apierrors.IsNotFound(err), meta.IsNoMatchError(err):
		return nil
	default:
		return err
	}
}

// apply server-side applies desired, declaring only the fields the operator
// owns. Anything it does not set — API-server defaults, an HPA's replicas,
// annotations written by sibling controllers — is left alone. The previous
// read-then-DeepEqual-then-overwrite pattern compared a freshly built spec
// against a defaulted one, so it always saw a difference and rewrote the object
// on every pass.
func (r *LangfuseInstanceReconciler) apply(ctx context.Context, instance *v1alpha1.LangfuseInstance, desired client.Object) error {
	if err := controllerutil.SetControllerReference(instance, desired, r.Scheme); err != nil {
		return fmt.Errorf("setting owner reference: %w", err)
	}

	// Apply patches are rejected without apiVersion/kind, which the builders
	// leave off typed objects.
	gvk, err := apiutil.GVKForObject(desired, r.Scheme)
	if err != nil {
		return fmt.Errorf("resolving GVK for %T: %w", desired, err)
	}
	desired.GetObjectKind().SetGroupVersionKind(gvk)

	// ForceOwnership takes over fields still attributed to the pre-SSA manager.
	if err := r.Patch(ctx, desired, client.Apply,
		client.FieldOwner(fieldOwnerInstance), client.ForceOwnership); err != nil {
		return fmt.Errorf("applying %s %s: %w", gvk.Kind, desired.GetName(), err)
	}
	return nil
}

// updateStatus refreshes observed state and derives phase. workloadApplied says
// whether this pass actually reconciled the Deployments; when it did not, fields
// that describe what is running are left as they were.
func (r *LangfuseInstanceReconciler) updateStatus(ctx context.Context, instance, original *v1alpha1.LangfuseInstance, workloadApplied bool) error {
	// Fetch current deployment states
	webDeploy := &appsv1.Deployment{}
	if err := r.Get(ctx, client.ObjectKey{Name: resources.WebName(instance), Namespace: instance.Namespace}, webDeploy); err != nil {
		if !apierrors.IsNotFound(err) {
			return fmt.Errorf("getting web deployment status: %w", err)
		}
	} else {
		instance.Status.Web = &v1alpha1.ComponentStatus{
			Replicas:      webDeploy.Status.Replicas,
			ReadyReplicas: webDeploy.Status.ReadyReplicas,
			Endpoint:      fmt.Sprintf("http://%s.%s.svc:3000", resources.WebServiceName(instance), instance.Namespace),
			Issues:        r.podIssuesFor(ctx, instance, componentWeb, webDeploy),
		}
	}

	workerDeploy := &appsv1.Deployment{}
	if err := r.Get(ctx, client.ObjectKey{Name: resources.WorkerName(instance), Namespace: instance.Namespace}, workerDeploy); err != nil {
		if !apierrors.IsNotFound(err) {
			return fmt.Errorf("getting worker deployment status: %w", err)
		}
	} else {
		// Mutate in place: CircuitBreakerActive/Reason are owned by the circuit
		// breaker, and replacing the struct wipes them.
		if instance.Status.Worker == nil {
			instance.Status.Worker = &v1alpha1.WorkerComponentStatus{}
		}
		instance.Status.Worker.Replicas = workerDeploy.Status.Replicas
		instance.Status.Worker.ReadyReplicas = workerDeploy.Status.ReadyReplicas
		instance.Status.Worker.Issues = r.podIssuesFor(ctx, instance, componentWorker, workerDeploy)
	}

	instance.Status.Phase, instance.Status.Ready = derivePhase(instance)

	// Only advance these once the spec has actually been applied to the
	// Deployments. On a frozen reconcile the pods still run the previous image and
	// URL, so publishing the spec's values would claim a rollout that never
	// happened — and with migrations disabled it would claim it indefinitely.
	if workloadApplied {
		instance.Status.Version = instance.Spec.Image.Tag
		instance.Status.PublicUrl = instance.Spec.Auth.NextAuthUrl
	}

	meta.SetStatusCondition(&instance.Status.Conditions, metav1.Condition{
		Type:               conditionTypeReady,
		Status:             boolToConditionStatus(instance.Status.Ready),
		Reason:             phaseToReason(instance.Status.Phase),
		Message:            fmt.Sprintf("Phase: %s", instance.Status.Phase),
		ObservedGeneration: instance.Generation,
	})

	return updateInstanceStatus(ctx, r.Client, instance, original)
}

// setDeprecationCondition flags spec fields scheduled for removal. Managed
// datastore modes are dev/CI only and go away in 0.11.0; surfacing that on the
// CR (and in the log) gives users a release to migrate instead of discovering
// it when an upgrade rejects their spec.
//
// spec.database.managed is deliberately absent here — it is rejected outright
// in langfuse.BuildConfig, which reports a ConfigError condition instead.
func setDeprecationCondition(ctx context.Context, instance *v1alpha1.LangfuseInstance) {
	var deprecated []string
	if instance.Spec.ClickHouse != nil && instance.Spec.ClickHouse.Managed != nil {
		deprecated = append(deprecated, "spec.clickhouse.managed (single-node, no replication or backups, "+
			"unsuitable for production — use external: ClickHouse Cloud or the Altinity operator)")
	}
	if instance.Spec.Redis != nil && instance.Spec.Redis.Managed != nil {
		deprecated = append(deprecated, "spec.redis.managed (single-node, no replication or backups, "+
			"unsuitable for production — use external: a managed Redis service or a Redis operator)")
	}
	if instance.Spec.Observability != nil && instance.Spec.Observability.ServiceMonitor != nil {
		deprecated = append(deprecated, "spec.observability.serviceMonitor (ignored: Langfuse serves no "+
			"Prometheus endpoint, so the ServiceMonitor only ever pointed Prometheus at a JSON health "+
			"route and reported the target down — use spec.observability.otel)")
	}

	if len(deprecated) == 0 {
		meta.RemoveStatusCondition(&instance.Status.Conditions, conditionTypeDeprecated)
		return
	}

	message := fmt.Sprintf("Deprecated fields in use, removal in 0.11.0: %s",
		strings.Join(deprecated, "; "))

	logf.FromContext(ctx).Info("WARNING: deprecated spec fields in use", "fields", deprecated)

	meta.SetStatusCondition(&instance.Status.Conditions, metav1.Condition{
		Type:               conditionTypeDeprecated,
		Status:             metav1.ConditionTrue,
		Reason:             "ManagedDatastoresDeprecated",
		Message:            message,
		ObservedGeneration: instance.Generation,
	})
}

// podIssuesFor returns pod-level problems for a component, but only when the
// Deployment is not fully ready — a healthy component should never carry stale
// issues, and skipping the pod List on the happy path keeps reconciles cheap.
func (r *LangfuseInstanceReconciler) podIssuesFor(
	ctx context.Context,
	instance *v1alpha1.LangfuseInstance,
	component string,
	deploy *appsv1.Deployment,
) []v1alpha1.PodIssue {
	if deploy.Status.ReadyReplicas > 0 && deploy.Status.ReadyReplicas >= deploy.Status.Replicas {
		return nil
	}

	issues, err := collectPodIssues(ctx, r.Client, instance.Namespace,
		resources.SelectorLabels(instance, component))
	if err != nil {
		// Diagnostics are best-effort: failing to read pods must not fail the
		// reconcile, which would stop the operator managing the instance.
		logf.FromContext(ctx).Error(err, "failed to collect pod issues", "component", component)
		return nil
	}
	return issues
}

func boolToConditionStatus(b bool) metav1.ConditionStatus {
	if b {
		return metav1.ConditionTrue
	}
	return metav1.ConditionFalse
}

func phaseToReason(phase string) string {
	switch phase {
	case phaseRunning:
		return "AllComponentsReady"
	case phasePending:
		return "ComponentsStarting"
	case phaseMigrating:
		return "MigrationInProgress"
	case phaseDegraded:
		return "ComponentDegraded"
	case phaseError:
		return "ReconcileError"
	default:
		return "Unknown"
	}
}

func (r *LangfuseInstanceReconciler) reconcileNetworkPolicies(ctx context.Context, instance *v1alpha1.LangfuseInstance) error {
	log := logf.FromContext(ctx)

	// Check if NetworkPolicy is disabled
	if instance.Spec.Security != nil &&
		instance.Spec.Security.NetworkPolicy != nil &&
		instance.Spec.Security.NetworkPolicy.Enabled != nil &&
		!*instance.Spec.Security.NetworkPolicy.Enabled {
		// TODO: delete existing NetworkPolicies if they were previously created
		return nil
	}

	webNetpol := resources.BuildWebNetworkPolicy(instance)
	if err := r.apply(ctx, instance, webNetpol); err != nil {
		return fmt.Errorf("web network policy: %w", err)
	}
	log.Info("reconciled web network policy", "name", webNetpol.Name)

	workerNetpol := resources.BuildWorkerNetworkPolicy(instance)
	if err := r.apply(ctx, instance, workerNetpol); err != nil {
		return fmt.Errorf("worker network policy: %w", err)
	}
	log.Info("reconciled worker network policy", "name", workerNetpol.Name)

	return nil
}

func (r *LangfuseInstanceReconciler) reconcileClickHouse(ctx context.Context, instance *v1alpha1.LangfuseInstance) error {
	// ConfigMap
	cm := resources.BuildClickHouseConfigMap(instance)
	if err := r.apply(ctx, instance, cm); err != nil {
		return fmt.Errorf("clickhouse configmap: %w", err)
	}
	// Service
	svc := resources.BuildClickHouseService(instance)
	if err := r.apply(ctx, instance, svc); err != nil {
		return fmt.Errorf("clickhouse service: %w", err)
	}
	// StatefulSet
	sts := resources.BuildClickHouseStatefulSet(instance)
	if err := r.apply(ctx, instance, sts); err != nil {
		return fmt.Errorf("clickhouse statefulset: %w", err)
	}
	return nil
}

func (r *LangfuseInstanceReconciler) reconcileRedis(ctx context.Context, instance *v1alpha1.LangfuseInstance) error {
	// Service
	svc := resources.BuildRedisService(instance)
	if err := r.apply(ctx, instance, svc); err != nil {
		return fmt.Errorf("redis service: %w", err)
	}
	// StatefulSet
	sts := resources.BuildRedisStatefulSet(instance)
	if err := r.apply(ctx, instance, sts); err != nil {
		return fmt.Errorf("redis statefulset: %w", err)
	}
	return nil
}

func (r *LangfuseInstanceReconciler) reconcileHPAs(ctx context.Context, instance *v1alpha1.LangfuseInstance) error {
	// Web HPA
	if instance.Spec.Web.Autoscaling != nil && instance.Spec.Web.Autoscaling.Enabled {
		hpa := resources.BuildWebHPA(instance)
		if err := r.apply(ctx, instance, hpa); err != nil {
			return fmt.Errorf("web HPA: %w", err)
		}
	}
	// Worker HPA
	if instance.Spec.Worker.Autoscaling != nil && instance.Spec.Worker.Autoscaling.Enabled {
		hpa := resources.BuildWorkerHPA(instance)
		if err := r.apply(ctx, instance, hpa); err != nil {
			return fmt.Errorf("worker HPA: %w", err)
		}
	}
	return nil
}

func (r *LangfuseInstanceReconciler) reconcilePDBs(ctx context.Context, instance *v1alpha1.LangfuseInstance) error {
	// Web PDB
	if instance.Spec.Web.PodDisruptionBudget != nil && instance.Spec.Web.PodDisruptionBudget.Enabled {
		pdb := resources.BuildWebPDB(instance)
		if err := r.apply(ctx, instance, pdb); err != nil {
			return fmt.Errorf("web PDB: %w", err)
		}
	}
	// Worker PDB
	if instance.Spec.Worker.PodDisruptionBudget != nil && instance.Spec.Worker.PodDisruptionBudget.Enabled {
		pdb := resources.BuildWorkerPDB(instance)
		if err := r.apply(ctx, instance, pdb); err != nil {
			return fmt.Errorf("worker PDB: %w", err)
		}
	}
	return nil
}

func (r *LangfuseInstanceReconciler) SetupWithManager(mgr ctrl.Manager) error {
	// driftOnly suppresses status-only events from children the operator writes
	// but never reads. Deployment is deliberately absent: derivePhase needs its
	// status.readyReplicas.
	driftOnly := builder.WithPredicates(predicate.GenerationChangedPredicate{})

	b := ctrl.NewControllerManagedBy(mgr).
		For(&v1alpha1.LangfuseInstance{}).
		Owns(&appsv1.Deployment{}).
		Owns(&appsv1.StatefulSet{}, driftOnly).
		Owns(&corev1.Service{}, driftOnly).
		Owns(&corev1.ConfigMap{}, driftOnly).
		Owns(&networkingv1.NetworkPolicy{}, driftOnly).
		Owns(&networkingv1.Ingress{}, driftOnly).
		Owns(&autoscalingv2.HorizontalPodAutoscaler{}, driftOnly).
		Owns(&policyv1.PodDisruptionBudget{}, driftOnly).
		Named("langfuseinstance")

	// Watch OpenShift Routes if the API is available
	routeGVK := schema.GroupVersionKind{Group: "route.openshift.io", Version: "v1", Kind: "Route"}
	if _, err := mgr.GetRESTMapper().RESTMapping(routeGVK.GroupKind(), routeGVK.Version); err == nil {
		route := &unstructured.Unstructured{}
		route.SetGroupVersionKind(routeGVK)
		b = b.Owns(route, driftOnly)
	}

	// Watch Gateway API HTTPRoutes if the API is available
	httpRouteGVK := schema.GroupVersionKind{Group: "gateway.networking.k8s.io", Version: "v1", Kind: "HTTPRoute"}
	if _, err := mgr.GetRESTMapper().RESTMapping(httpRouteGVK.GroupKind(), httpRouteGVK.Version); err == nil {
		httpRoute := &unstructured.Unstructured{}
		httpRoute.SetGroupVersionKind(httpRouteGVK)
		b = b.Owns(httpRoute, driftOnly)
	}

	return b.Complete(r)
}
