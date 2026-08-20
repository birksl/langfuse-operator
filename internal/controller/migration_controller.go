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
	"errors"
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

	// The identity covers the ClickHouse database and the datastore references as
	// well as the tag: pointing an unchanged image at a different database or
	// Secret gives the workload a target that has no schema.
	desiredTag := instance.Spec.Image.Tag
	desiredIdentity := migrationIdentity(instance)

	// Establish a baseline for instances migrated before appliedIdentity existed.
	// Their status records only a version, so the reference and key components
	// have nothing to compare against and a later repoint would pass unnoticed.
	// This runs ahead of the runOnDeploy check because it writes status only — it
	// starts no migration — and an instance with migrations disabled still needs
	// the workload freeze to work.
	if baseline, ok := legacyBaselineIdentity(instance, desiredIdentity); ok {
		if instance.Status.Migration == nil {
			instance.Status.Migration = &v1alpha1.MigrationStatus{}
		}
		instance.Status.Migration.AppliedIdentity = baseline
		if err := updateInstanceStatus(ctx, r.Client, instance, original); err != nil {
			return statusWriteFailed(err, "recording migration baseline")
		}
		log.Info("recorded a migration baseline for a pre-existing instance", "identity", baseline)
		return ctrl.Result{}, nil
	}

	// Skip if migration is disabled
	if instance.Spec.Database != nil && instance.Spec.Database.Migration != nil &&
		instance.Spec.Database.Migration.RunOnDeploy != nil && !*instance.Spec.Database.Migration.RunOnDeploy {
		return ctrl.Result{}, nil
	}

	// Gate on what the last migration ran against, not Status.Version.
	// Status.Version is written by the instance controller on every pass and
	// means "deployed", so whichever controller won the race on a fresh CR
	// decided whether migrations ever ran — and once set it never reopened.
	if migrationUpToDate(appliedMigrationIdentity(instance), desiredIdentity) {
		// Nothing to migrate, check if an existing Job needs cleanup
		return r.cleanupCompletedJobs(ctx, instance)
	}

	// The datastore target is fixed once a schema exists. Re-pointing at another
	// database would leave every row already written behind, invisible to the
	// application; clustering fixes the table engine at CREATE time and cannot be
	// converted in place. Neither is something to do by editing a live CR, so
	// refuse and say what to do instead.
	if changed := retargetedComponents(appliedMigrationIdentity(instance), desiredIdentity); len(changed) > 0 {
		meta.SetStatusCondition(&instance.Status.Conditions, metav1.Condition{
			Type:   conditionMigrationsComplete,
			Status: metav1.ConditionFalse,
			Reason: reasonMigrationFailed,
			Message: fmt.Sprintf("%s changed after migrations already ran. The ClickHouse target is "+
				"fixed once its schema exists: another database would leave the existing data behind, "+
				"and a table's engine cannot be changed after CREATE. Revert the change, or create a "+
				"separate LangfuseInstance for the new target and cut over once it has migrated",
				strings.Join(changed, " and ")),
			ObservedGeneration: instance.Generation,
		})
		retry := noteStatusWriteFailure(ctx, updateInstanceStatus(ctx, r.Client, instance, original))
		// Info, not Error: the refusal is this controller working as designed, and
		// it recurs on every pass for as long as the spec stands. The state that
		// matters is on the MigrationsComplete condition.
		log.Info("refusing to retarget a migrated instance", "changed", changed)
		return requeueIf(retry), nil
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
		retry := noteStatusWriteFailure(ctx, updateInstanceStatus(ctx, r.Client, instance, original))
		log.Info("refusing to create migration job", "missingKeys", missing)
		return requeueIf(retry), nil
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
		// Retry flag ignored: this path requeues while the Job runs regardless.
		noteStatusWriteFailure(ctx, updateInstanceStatus(ctx, r.Client, instance, original))

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
		// Already terminating: wait rather than issue a second delete. Foreground
		// deletion keeps the Job visible until its pods are gone, so this is how
		// the drain is observed.
		if !existingJob.DeletionTimestamp.IsZero() {
			log.V(1).Info("waiting for the previous migration job to drain", "job", jobName)
			return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
		}

		log.Info("replacing migration job from a different target",
			"jobIdentity", jobIdentity, "desiredIdentity", desiredIdentity)
		// Foreground, not background: background deletion removes the Job object
		// immediately and reaps its pods afterwards, so the replacement Job could
		// start while the previous migration pod is still running. Two concurrent
		// migration pods is exactly the Prisma advisory-lock contention the
		// operator avoids by disabling migrations in the web and worker
		// entrypoints. Foreground keeps the Job until its pods are gone.
		propagation := metav1.DeletePropagationForeground
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
		// This write is the record that the migration happened. Losing it to a
		// conflict and returning without a requeue would leave the gate open
		// against a database that is already migrated.
		retry := noteStatusWriteFailure(ctx, updateInstanceStatus(ctx, r.Client, instance, original))
		r.Recorder.Eventf(instance, "Normal", "MigrationCompleted",
			"Migration job %s completed successfully", jobName)
		return requeueIf(retry), nil
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
			summary := fmt.Sprintf("Migration job failed after %d attempts", existingJob.Status.Failed)
			_, message := summarizePodIssues(issues, reasonMigrationFailed, summary)
			// A real error value, so the stacktrace this level attaches has
			// something to sit under; logr treats a nil error as a programming
			// mistake and the line arrives without any of the cause.
			log.Error(errors.New(message), "migration job failed",
				"version", desiredTag, "failures", existingJob.Status.Failed)
			meta.SetStatusCondition(&instance.Status.Conditions, metav1.Condition{
				Type:               conditionMigrationsComplete,
				Status:             metav1.ConditionFalse,
				Reason:             reasonMigrationFailed,
				Message:            message,
				ObservedGeneration: instance.Generation,
			})
			retry := noteStatusWriteFailure(ctx, updateInstanceStatus(ctx, r.Client, instance, original))
			r.Recorder.Eventf(instance, "Warning", "MigrationFailed",
				"Migration job %s failed after %d attempts: %s", jobName, existingJob.Status.Failed, message)
			return requeueIf(retry), nil
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
	// Retry flag ignored: the 10s poll below redoes a lost write soon enough.
	noteStatusWriteFailure(ctx, updateInstanceStatus(ctx, r.Client, instance, original))
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

// migrationIdentity describes everything spec-visible that decides what a
// migration produces and where it lands: the Langfuse version, the datastores it
// runs against, and the ClickHouse database and clustering mode its tables are
// created with.
//
// Datastores are identified by reference and endpoint key name only, never by the
// values behind them — reading the Secrets would make a rotated credential look
// like a new datastore.
func migrationIdentity(instance *v1alpha1.LangfuseInstance) string {
	return buildMigrationIdentity(migrationTarget{
		version:         instance.Spec.Image.Tag,
		postgresRef:     postgresRefName(instance),
		clickHouseRef:   clickHouseRefName(instance),
		clickHouseDB:    clickHouseDatabase(instance),
		clickHouseClust: instance.Spec.ClickHouse.ClusterEnabled(),
	})
}

// migrationTarget is everything spec-visible that decides what a migration
// produces and where it lands.
type migrationTarget struct {
	version         string
	postgresRef     string
	clickHouseRef   string
	clickHouseDB    string
	clickHouseClust bool
}

// buildMigrationIdentity renders a target. The database goes last because a
// ClickHouse database name may contain the separator; the reference names cannot,
// being Kubernetes object names.
func buildMigrationIdentity(t migrationTarget) string {
	return fmt.Sprintf("%s|%s%s|%s%s|%s%t|%s%s",
		langfuse.NormalizeVersion(t.version),
		migrationIdentityPostgresRefKey, t.postgresRef,
		migrationIdentityClickHouseRefKey, t.clickHouseRef,
		migrationIdentityClusterKey, t.clickHouseClust,
		migrationIdentityDatabaseKey, t.clickHouseDB)
}

// postgresRefName names the Secret or CNPG Cluster the schema lives behind,
// together with the keys that select an endpoint inside it — a Secret can hold
// several, so the name alone does not identify a datastore.
//
// directUrl counts as much as url: Langfuse's Prisma datasource declares
// directUrl = env("DIRECT_URL"), and prisma migrate deploy prefers it when set,
// so it — not url — is where the schema actually lands. Credential keys are
// excluded; rotating a password does not change which datastore this is.
func postgresRefName(instance *v1alpha1.LangfuseInstance) string {
	db := instance.Spec.Database
	switch {
	case db == nil:
		return ""
	case db.CloudNativePG != nil:
		return "cnpg/" + db.CloudNativePG.ClusterRef.Name
	case db.External != nil:
		return fmt.Sprintf("secret/%s#url=%s,directUrl=%s",
			db.External.SecretRef.Name,
			postgresURLKey(db.External.SecretRef),
			db.External.SecretRef.Keys["directUrl"])
	}
	return ""
}

func clickHouseRefName(instance *v1alpha1.LangfuseInstance) string {
	ch := instance.Spec.ClickHouse
	switch {
	case ch == nil:
		return ""
	case ch.Managed != nil:
		return "managed"
	case ch.External != nil:
		return fmt.Sprintf("secret/%s#url=%s,migrationUrl=%s",
			ch.External.SecretRef.Name,
			clickHouseURLKey(ch.External.SecretRef),
			ch.External.SecretRef.Keys["migrationUrl"])
	}
	return ""
}

// postgresURLKey and clickHouseURLKey resolve the endpoint key the same way the
// env config and the probes do, so omitting a mapping and setting it explicitly
// to its default describe the same target rather than looking like a move.
func postgresURLKey(ref v1alpha1.SecretKeysRef) string {
	if key := ref.Keys["url"]; key != "" {
		return key
	}
	return "database_url"
}

func clickHouseURLKey(ref v1alpha1.SecretKeysRef) string {
	if key := ref.Keys["url"]; key != "" {
		return key
	}
	return "url"
}

const (
	// migrationIdentityClusterKey prefixes the clustering component. Clustering
	// cannot be re-migrated into, so it has to be recoverable from a recorded
	// identity.
	migrationIdentityClusterKey = "clickhouse-cluster="
	// migrationIdentityPostgresRefKey and migrationIdentityClickHouseRefKey
	// prefix the datastore references. Repointing either moves the workload onto
	// a different store, which is a retarget rather than an upgrade.
	migrationIdentityPostgresRefKey   = "postgres-ref="
	migrationIdentityClickHouseRefKey = "clickhouse-ref="
	// migrationIdentityDatabaseKey prefixes the database component. It comes last
	// so a database name containing the separator still round-trips.
	migrationIdentityDatabaseKey = "clickhouse-db="
)

// migrationUpToDate reports whether a recorded identity already covers the
// desired target.
//
// It compares component by component rather than as a string, because a recorded
// identity may predate a component: those written before the datastore references
// were tracked carry only version, clustering and database. Whole-string equality
// would make every such instance look stale and re-migrate on an operator
// upgrade, and would do so again the next time a component is added. References
// get the same treatment one level down, per endpoint key.
func migrationUpToDate(applied, desired string) bool {
	if applied == "" {
		return false
	}
	if identityVersion(applied) != identityVersion(desired) {
		return false
	}
	for _, key := range []string{
		migrationIdentityPostgresRefKey,
		migrationIdentityClickHouseRefKey,
	} {
		was, ok := identityComponent(applied, key)
		if !ok {
			continue
		}
		if want, _ := identityComponent(desired, key); !refsAgree(was, want) {
			return false
		}
	}
	if was, ok := identityClusterMode(applied); ok {
		if want, _ := identityClusterMode(desired); was != want {
			return false
		}
	}
	if was, ok := identityDatabase(applied); ok {
		if want, _ := identityDatabase(desired); was != want {
			return false
		}
	}
	return true
}

// identityVersion returns the leading, unprefixed version component.
func identityVersion(identity string) string {
	version, _, _ := strings.Cut(identity, "|")
	return version
}

// identityComponent extracts a prefixed component of a recorded identity.
func identityComponent(identity, key string) (string, bool) {
	for _, part := range strings.Split(identity, "|") {
		if value, ok := strings.CutPrefix(part, key); ok {
			return value, true
		}
	}
	return "", false
}

// identityClusterMode extracts the clustering component of a recorded identity.
func identityClusterMode(identity string) (string, bool) {
	return identityComponent(identity, migrationIdentityClusterKey)
}

// identityLookup adapts a prefixed component to the lookup signature used below.
func identityLookup(key string) func(string) (string, bool) {
	return func(identity string) (string, bool) { return identityComponent(identity, key) }
}

// refsAgree reports whether a recorded reference still describes the desired one.
// The object name must match, and so must every endpoint key the recording
// carries — but only those: the set of keys tracked has grown, and a reference
// written before a key was tracked must not read as a move. That is the rule
// migrationUpToDate applies to whole components, applied per key.
func refsAgree(applied, desired string) bool {
	appliedName, appliedKeys := splitRef(applied)
	desiredName, desiredKeys := splitRef(desired)
	if appliedName != desiredName {
		return false
	}
	for key, was := range appliedKeys {
		if want, ok := desiredKeys[key]; !ok || was != want {
			return false
		}
	}
	return true
}

// splitRef separates a reference into its object name and its endpoint key
// mappings: "secret/pg#url=database_url,directUrl=direct_url". Neither separator
// can occur in a Secret name or a Secret key, so the split is unambiguous.
func splitRef(ref string) (string, map[string]string) {
	name, mappings, found := strings.Cut(ref, "#")
	if !found {
		return name, nil
	}
	keys := make(map[string]string)
	for _, mapping := range strings.Split(mappings, ",") {
		key, value, _ := strings.Cut(mapping, "=")
		keys[key] = value
	}
	return name, keys
}

// retargetedComponents lists which parts of the datastore target differ from
// what the last successful migration used. Both are fixed when the schema is
// created: pointing at another database orphans every row already written, and
// ClickHouse fixes a table's engine at CREATE time, so clustering cannot be
// converted in place either.
//
// The version is deliberately excluded — upgrading in place is the normal path,
// and that is what re-running migrations is for.
func retargetedComponents(applied, desired string) []string {
	if applied == "" {
		return nil // never migrated; anything goes
	}

	// Each component is compared only when the recorded identity carries it, so
	// identities written before a component existed do not read as a retarget.
	equal := func(was, want string) bool { return was == want }
	var changed []string
	for _, c := range []struct {
		field  string
		lookup func(string) (string, bool)
		agree  func(was, want string) bool
	}{
		{"spec.database", identityLookup(migrationIdentityPostgresRefKey), refsAgree},
		{"spec.clickhouse (connection)", identityLookup(migrationIdentityClickHouseRefKey), refsAgree},
		{"spec.clickhouse.cluster.enabled", identityLookup(migrationIdentityClusterKey), equal},
		{"spec.clickhouse.database", identityDatabase, equal},
	} {
		was, ok := c.lookup(applied)
		if !ok {
			continue
		}
		if want, _ := c.lookup(desired); !c.agree(was, want) {
			changed = append(changed, fmt.Sprintf("%s %q -> %q", c.field, was, want))
		}
	}
	return changed
}

// identityDatabase extracts the database component of a recorded identity. It is
// the final component, so everything after the marker is the name.
func identityDatabase(identity string) (string, bool) {
	if i := strings.Index(identity, "|"+migrationIdentityDatabaseKey); i >= 0 {
		return identity[i+len("|"+migrationIdentityDatabaseKey):], true
	}
	return "", false
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
		// Record only what those operators actually determined: the version, the
		// "default" database they had no way to change, and unclustered, which
		// they forced. The datastore references are deliberately absent — they
		// were never recorded, and an absent component is skipped by
		// retargetedComponents rather than read as a change. The reference checks
		// therefore start applying after this instance's next real migration.
		return fmt.Sprintf("%s|%s%t|%s%s",
			langfuse.NormalizeVersion(instance.Status.Database.MigrationVersion),
			migrationIdentityClusterKey, false,
			migrationIdentityDatabaseKey, defaultClickHouseDatabase)
	}
	return ""
}

// legacyBaselineIdentity returns the identity to persist for an instance that
// migrated before appliedIdentity existed, and whether there is one to persist.
//
// Such an instance records only status.database.migrationVersion, so the
// components added since — the datastore references, their endpoint keys, the
// clustering mode — have nothing to compare against and are skipped. That is the
// right call for one reconcile, but left unpersisted it is permanent: a Secret or
// key repointed later would never register as a change. Writing the full current
// identity once fixes the baseline.
//
// It only fires when the components that *were* recorded still match the spec.
// If they differ the target has already moved, and recording the new one would
// launder a retarget into an applied state and lose the detection for good.
func legacyBaselineIdentity(instance *v1alpha1.LangfuseInstance, desired string) (string, bool) {
	if instance.Status.Migration != nil && instance.Status.Migration.AppliedIdentity != "" {
		return "", false // already baselined
	}
	legacy := appliedMigrationIdentity(instance)
	if legacy == "" {
		return "", false // never migrated; the first real migration records it
	}
	if !migrationUpToDate(legacy, desired) {
		return "", false
	}
	return desired, true
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
