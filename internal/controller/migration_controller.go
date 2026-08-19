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
	"time"

	batchv1 "k8s.io/api/batch/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	v1alpha1 "github.com/PalenaAI/langfuse-operator/api/v1alpha1"
	"github.com/PalenaAI/langfuse-operator/internal/langfuse"
	"github.com/PalenaAI/langfuse-operator/internal/resources"
)

// MigrationController watches LangfuseInstance for version changes and manages migration Jobs.
type MigrationController struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder record.EventRecorder
}

// +kubebuilder:rbac:groups=langfuse.palena.ai,resources=langfuseinstances,verbs=get;list;watch
// +kubebuilder:rbac:groups=langfuse.palena.ai,resources=langfuseinstances/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=batch,resources=jobs,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=core,resources=events,verbs=create;patch
// +kubebuilder:rbac:groups=core,resources=pods,verbs=get;list;watch

func (r *MigrationController) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	instance := &v1alpha1.LangfuseInstance{}
	if err := r.Get(ctx, req.NamespacedName, instance); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// Don't kick off new migration Jobs while the CR is being deleted —
	// the Job's owner reference is the LangfuseInstance, which GC is removing.
	if !instance.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, nil
	}

	original := instance.DeepCopy()

	// Skip if migration is disabled
	if instance.Spec.Database != nil && instance.Spec.Database.Migration != nil &&
		instance.Spec.Database.Migration.RunOnDeploy != nil && !*instance.Spec.Database.Migration.RunOnDeploy {
		return ctrl.Result{}, nil
	}

	// Gate on what the last migration ran against, not Status.Version.
	// Status.Version is written by the instance controller on every pass and
	// means "deployed", so whichever controller won the race on a fresh CR
	// decided whether migrations ever ran — and once set it never reopened.
	//
	// The identity covers the ClickHouse database as well as the tag: pointing
	// an unchanged image at a different database gives the workload a new
	// CLICKHOUSE_DB, and that database needs its tables.
	desiredTag := instance.Spec.Image.Tag
	desiredIdentity := migrationIdentity(instance)
	if applied := appliedMigrationIdentity(instance); applied == desiredIdentity {
		// Nothing to migrate, check if an existing Job needs cleanup
		return r.cleanupCompletedJobs(ctx, instance)
	}

	// The Job's migration step exits non-zero without these, so refuse to
	// create a Job that cannot succeed and say which keys are missing.
	if missing := missingClickHouseMigrationKeys(instance); len(missing) > 0 {
		meta.SetStatusCondition(&instance.Status.Conditions, metav1.Condition{
			Type:   conditionMigrationsComplete,
			Status: metav1.ConditionFalse,
			Reason: reasonMigrationFailed,
			Message: fmt.Sprintf("spec.clickhouse.external.secretRef.keys is missing %s, "+
				"which the ClickHouse migration step requires", strings.Join(missing, ", ")),
			ObservedGeneration: instance.Generation,
		})
		if statusErr := updateInstanceStatus(ctx, r.Client, instance, original); statusErr != nil {
			log.Error(statusErr, "failed to update status")
		}
		log.Error(nil, "refusing to create migration job", "missingKeys", missing)
		return ctrl.Result{}, nil
	}

	// Build config for migration job env vars
	config, err := langfuse.BuildConfig(instance)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("building config for migration: %w", err)
	}

	// Check for existing migration job
	jobName := resources.MigrationJobName(instance)
	existingJob := &batchv1.Job{}
	err = r.Get(ctx, client.ObjectKey{Name: jobName, Namespace: instance.Namespace}, existingJob)

	if apierrors.IsNotFound(err) {
		// Create migration job
		log.Info("creating migration job", "version", desiredTag, "identity", desiredIdentity)
		job := resources.BuildMigrationJob(instance, config)
		if job.Annotations == nil {
			job.Annotations = map[string]string{}
		}
		job.Annotations[migrationIdentityAnnotation] = desiredIdentity
		if err := controllerutil.SetControllerReference(instance, job, r.Scheme); err != nil {
			return ctrl.Result{}, fmt.Errorf("setting owner reference on migration job: %w", err)
		}

		meta.SetStatusCondition(&instance.Status.Conditions, metav1.Condition{
			Type:               conditionMigrationsComplete,
			Status:             metav1.ConditionFalse,
			Reason:             reasonMigrationStarted,
			Message:            fmt.Sprintf("Running migrations for version %s", desiredTag),
			ObservedGeneration: instance.Generation,
		})
		if statusErr := updateInstanceStatus(ctx, r.Client, instance, original); statusErr != nil {
			log.Error(statusErr, "failed to update status")
		}

		if err := r.Create(ctx, job); err != nil {
			return ctrl.Result{}, fmt.Errorf("creating migration job: %w", err)
		}
		r.Recorder.Eventf(instance, "Normal", "MigrationStarted",
			"Started migration job %s for version %s", jobName, desiredTag)

		return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
	}
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("getting migration job: %w", err)
	}

	// The Job name carries no version, and a succeeded Job lingers for its
	// TTLSecondsAfterFinished (1h). Without this check, upgrading inside that
	// window would read the previous version's success as this version's and
	// record it as migrated without running anything — starting the new
	// application against the old schema. A Job spec is immutable, so a
	// mismatch has to be replaced rather than updated. Re-running migrations is
	// idempotent (prisma migrate deploy and golang-migrate both track what they
	// have applied), so replacing an unrecognised Job is safe.
	if jobIdentity := existingJob.Annotations[migrationIdentityAnnotation]; jobIdentity != desiredIdentity {
		log.Info("replacing migration job from a different target",
			"jobIdentity", jobIdentity, "desiredIdentity", desiredIdentity)
		propagation := metav1.DeletePropagationBackground
		if err := r.Delete(ctx, existingJob, &client.DeleteOptions{
			PropagationPolicy: &propagation,
		}); err != nil && !apierrors.IsNotFound(err) {
			return ctrl.Result{}, fmt.Errorf("deleting stale migration job: %w", err)
		}
		return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
	}

	// Job exists — check status
	if existingJob.Status.Succeeded > 0 {
		log.Info("migration job succeeded", "version", desiredTag)
		meta.SetStatusCondition(&instance.Status.Conditions, metav1.Condition{
			Type:               conditionMigrationsComplete,
			Status:             metav1.ConditionTrue,
			Reason:             reasonMigrationSucceeded,
			Message:            fmt.Sprintf("Migrations completed for version %s", desiredTag),
			ObservedGeneration: instance.Generation,
		})
		if instance.Status.Database == nil {
			instance.Status.Database = &v1alpha1.DatabaseStatus{}
		}
		instance.Status.Database.MigrationVersion = langfuse.NormalizeVersion(desiredTag)
		if instance.Status.Migration == nil {
			instance.Status.Migration = &v1alpha1.MigrationStatus{}
		}
		instance.Status.Migration.AppliedIdentity = desiredIdentity
		if statusErr := updateInstanceStatus(ctx, r.Client, instance, original); statusErr != nil {
			log.Error(statusErr, "failed to update status")
		}
		r.Recorder.Eventf(instance, "Normal", "MigrationCompleted",
			"Migration job %s completed successfully", jobName)
		return ctrl.Result{}, nil
	}

	// Diagnose the migration pods so a stuck or failed migration explains
	// itself (bad DATABASE_URL, unreachable store, missing Secret key) instead
	// of only reporting an attempt count.
	// Mutate in place: replacing the struct would drop AppliedIdentity, which is
	// what the gate reads.
	issues := r.migrationPodIssues(ctx, instance)
	if instance.Status.Migration == nil {
		instance.Status.Migration = &v1alpha1.MigrationStatus{}
	}
	instance.Status.Migration.JobName = jobName
	instance.Status.Migration.Failed = existingJob.Status.Failed
	instance.Status.Migration.Issues = issues

	if existingJob.Status.Failed > 0 {
		backoffLimit := int32(3)
		if existingJob.Spec.BackoffLimit != nil {
			backoffLimit = *existingJob.Spec.BackoffLimit
		}
		if existingJob.Status.Failed >= backoffLimit {
			log.Error(nil, "migration job failed", "version", desiredTag, "failures", existingJob.Status.Failed)
			summary := fmt.Sprintf("Migration job failed after %d attempts", existingJob.Status.Failed)
			_, message := summarizePodIssues(issues, reasonMigrationFailed, summary)
			meta.SetStatusCondition(&instance.Status.Conditions, metav1.Condition{
				Type:               conditionMigrationsComplete,
				Status:             metav1.ConditionFalse,
				Reason:             reasonMigrationFailed,
				Message:            message,
				ObservedGeneration: instance.Generation,
			})
			if statusErr := updateInstanceStatus(ctx, r.Client, instance, original); statusErr != nil {
				log.Error(statusErr, "failed to update status")
			}
			r.Recorder.Eventf(instance, "Warning", "MigrationFailed",
				"Migration job %s failed after %d attempts: %s", jobName, existingJob.Status.Failed, message)
			return ctrl.Result{}, nil
		}
	}

	// Job still running — surface any in-flight pod problems (a fatal one, such
	// as a missing Secret key, will never clear on its own) and requeue.
	log.Info("migration job in progress", "version", desiredTag)
	if len(issues) > 0 {
		summary := "Migration job in progress"
		_, message := summarizePodIssues(issues, reasonMigrationInProgress, summary)
		meta.SetStatusCondition(&instance.Status.Conditions, metav1.Condition{
			Type:               conditionMigrationsComplete,
			Status:             metav1.ConditionFalse,
			Reason:             reasonMigrationInProgress,
			Message:            message,
			ObservedGeneration: instance.Generation,
		})
	}
	if statusErr := updateInstanceStatus(ctx, r.Client, instance, original); statusErr != nil {
		log.Error(statusErr, "failed to update status")
	}
	return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
}

// migrationPodIssues collects pod-level problems from the migration Job's pods.
// Diagnostics are best-effort — a failure to list pods must not abort the
// migration reconcile.
func (r *MigrationController) migrationPodIssues(ctx context.Context, instance *v1alpha1.LangfuseInstance) []v1alpha1.PodIssue {
	issues, err := collectPodIssues(ctx, r.Client, instance.Namespace,
		resources.SelectorLabels(instance, componentMigration))
	if err != nil {
		logf.FromContext(ctx).Error(err, "failed to collect migration pod issues")
		return nil
	}
	return issues
}

func (r *MigrationController) cleanupCompletedJobs(ctx context.Context, instance *v1alpha1.LangfuseInstance) (ctrl.Result, error) {
	jobName := resources.MigrationJobName(instance)
	existingJob := &batchv1.Job{}
	err := r.Get(ctx, client.ObjectKey{Name: jobName, Namespace: instance.Namespace}, existingJob)
	if apierrors.IsNotFound(err) {
		return ctrl.Result{}, nil
	}
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("checking migration job: %w", err)
	}

	// Clean up completed jobs older than TTL
	if existingJob.Status.Succeeded > 0 && existingJob.Status.CompletionTime != nil {
		if time.Since(existingJob.Status.CompletionTime.Time) > time.Hour {
			propagation := metav1.DeletePropagationBackground
			if err := r.Delete(ctx, existingJob, &client.DeleteOptions{
				PropagationPolicy: &propagation,
			}); err != nil && !apierrors.IsNotFound(err) {
				return ctrl.Result{}, fmt.Errorf("cleaning up migration job: %w", err)
			}
		}
	}

	return ctrl.Result{}, nil
}

// migrationIdentityAnnotation stamps a migration Job with what it was created
// to migrate, so its success is only ever credited to that target.
const migrationIdentityAnnotation = "langfuse.palena.ai/migration-identity"

// migrationIdentity describes everything spec-visible that determines what a
// migration produces: the Langfuse version and the ClickHouse database it
// creates tables in.
//
// The Postgres and ClickHouse endpoints are deliberately absent. They come from
// Secrets the operator would have to read on every pass, and a rotated
// credential would look like a new target. Repointing an instance at a fresh
// cluster by editing its Secret therefore does not re-trigger migrations — set
// spec.clickhouse.database, or clear status.migration.appliedIdentity, to force
// them.
func migrationIdentity(instance *v1alpha1.LangfuseInstance) string {
	return buildMigrationIdentity(instance.Spec.Image.Tag, clickHouseDatabase(instance))
}

func buildMigrationIdentity(version, database string) string {
	return fmt.Sprintf("%s|clickhouse-db=%s", langfuse.NormalizeVersion(version), database)
}

// appliedMigrationIdentity returns the identity of the last successful
// migration. Instances migrated before the field existed report only a version,
// and necessarily ran against the "default" database — back-filling that keeps
// an operator upgrade from re-running migrations on every existing instance.
func appliedMigrationIdentity(instance *v1alpha1.LangfuseInstance) string {
	if instance.Status.Migration != nil && instance.Status.Migration.AppliedIdentity != "" {
		return instance.Status.Migration.AppliedIdentity
	}
	if instance.Status.Database != nil && instance.Status.Database.MigrationVersion != "" {
		return buildMigrationIdentity(instance.Status.Database.MigrationVersion, defaultClickHouseDatabase)
	}
	return ""
}

// missingClickHouseMigrationKeys lists the logical secret keys that external
// ClickHouse needs for migrations but the spec does not map. The migration
// step (packages/shared/clickhouse/scripts/up.sh in the Langfuse image) exits 1
// when CLICKHOUSE_MIGRATION_URL, CLICKHOUSE_USER or CLICKHOUSE_PASSWORD is
// unset, and the operator only emits those env vars for keys that are present.
// Managed mode is exempt: it derives all three itself.
func missingClickHouseMigrationKeys(instance *v1alpha1.LangfuseInstance) []string {
	if instance.Spec.ClickHouse == nil || instance.Spec.ClickHouse.External == nil {
		return nil
	}
	keys := instance.Spec.ClickHouse.External.SecretRef.Keys
	var missing []string
	for _, required := range []string{"migrationUrl", "username", "password"} {
		if keys[required] == "" {
			missing = append(missing, required)
		}
	}
	return missing
}

// SetupWithManager sets up the controller with the Manager.
func (r *MigrationController) SetupWithManager(mgr ctrl.Manager) error {
	// Only spec changes need to reach this controller; sibling status writes do
	// not. Its own RequeueAfter picks up anything it still needs to observe.
	return ctrl.NewControllerManagedBy(mgr).
		For(&v1alpha1.LangfuseInstance{}, builder.WithPredicates(predicate.GenerationChangedPredicate{})).
		Owns(&batchv1.Job{}).
		Named("migration").
		Complete(r)
}
