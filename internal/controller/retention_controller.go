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
	"strconv"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	v1alpha1 "github.com/PalenaAI/langfuse-operator/api/v1alpha1"
)

const retentionRequeueInterval = 5 * time.Minute

// tableRetentionMapping maps a Langfuse ClickHouse table to its TTL timestamp column.
type tableRetentionMapping struct {
	Table           string
	TimestampColumn string
}

var langfuseClickHouseTables = []tableRetentionMapping{
	{Table: "traces", TimestampColumn: "timestamp"},
	{Table: "observations", TimestampColumn: "start_time"},
	{Table: "scores", TimestampColumn: "timestamp"},
	{Table: "events", TimestampColumn: "timestamp"},
}

// RetentionController manages ClickHouse data retention policies for LangfuseInstance objects.
type RetentionController struct {
	client.Client
	Scheme *runtime.Scheme
}

// +kubebuilder:rbac:groups=langfuse.palena.ai,resources=langfuseinstances,verbs=get;list;watch
// +kubebuilder:rbac:groups=langfuse.palena.ai,resources=langfuseinstances/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=core,resources=events,verbs=create;patch

func (r *RetentionController) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	// 1. Fetch the LangfuseInstance
	instance := &v1alpha1.LangfuseInstance{}
	if err := r.Get(ctx, req.NamespacedName, instance); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, fmt.Errorf("fetching LangfuseInstance: %w", err)
	}

	if !instance.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, nil
	}

	original := instance.DeepCopy()

	// 2. If ClickHouse retention is not configured, skip
	if instance.Spec.ClickHouse == nil || instance.Spec.ClickHouse.Retention == nil {
		meta.SetStatusCondition(&instance.Status.Conditions, metav1.Condition{
			Type:               "RetentionConfigured",
			Status:             metav1.ConditionFalse,
			Reason:             "NotConfigured",
			Message:            "No ClickHouse retention policy configured",
			ObservedGeneration: instance.Generation,
		})
		if err := updateInstanceStatus(ctx, r.Client, instance, original); err != nil {
			return statusWriteFailed(err, "updating retention status")
		}
		return ctrl.Result{RequeueAfter: retentionRequeueInterval}, nil
	}

	retention := instance.Spec.ClickHouse.Retention

	// 3. Build and apply the ALTER TABLE TTL statements for each configured table
	statements := r.buildRetentionStatements(instance, retention)
	if len(statements) > 0 {
		if instance.Status.ClickHouse == nil {
			instance.Status.ClickHouse = &v1alpha1.ClickHouseStatus{}
		}

		applied, err := r.applyRetention(ctx, instance, statements)
		// RetentionApplied reflects what ClickHouse actually accepted. It was
		// previously set to true purely from computing the statements, so the CR
		// claimed retention was active while data was retained forever.
		instance.Status.ClickHouse.RetentionApplied = ptrTo(err == nil)

		switch {
		case err != nil:
			log.Error(err, "failed to apply retention policies", "applied", applied, "total", len(statements))
			meta.SetStatusCondition(&instance.Status.Conditions, metav1.Condition{
				Type:   "RetentionConfigured",
				Status: metav1.ConditionFalse,
				Reason: "ApplyFailed",
				Message: fmt.Sprintf("Applied %d/%d retention TTL statements: %v",
					applied, len(statements), err),
				ObservedGeneration: instance.Generation,
			})
		default:
			meta.SetStatusCondition(&instance.Status.Conditions, metav1.Condition{
				Type:               "RetentionConfigured",
				Status:             metav1.ConditionTrue,
				Reason:             "PoliciesApplied",
				Message:            fmt.Sprintf("Applied %d retention TTL statements", applied),
				ObservedGeneration: instance.Generation,
			})
		}
	} else {
		meta.SetStatusCondition(&instance.Status.Conditions, metav1.Condition{
			Type:               "RetentionConfigured",
			Status:             metav1.ConditionFalse,
			Reason:             "NoTTLConfigured",
			Message:            "Retention spec present but no table TTLs configured",
			ObservedGeneration: instance.Generation,
		})
	}

	// 4. Handle storage pressure thresholds
	if retention.StoragePressure != nil && retention.StoragePressure.Enabled {
		r.evaluateStoragePressure(ctx, instance, retention.StoragePressure)
	}

	if err := updateInstanceStatus(ctx, r.Client, instance, original); err != nil {
		return statusWriteFailed(err, "updating retention status")
	}

	log.Info("reconciled retention policies", "instance", instance.Name)
	return ctrl.Result{RequeueAfter: retentionRequeueInterval}, nil
}

// buildRetentionStatements generates ALTER TABLE TTL statements from the
// retention spec.
//
// On a clustered instance the statements carry ON CLUSTER. ALTER on a
// Replicated*MergeTree table does propagate to the other replicas of its shard
// through Keeper, so a single-shard cluster would be fine without it — but shards
// are independent, and a plain ALTER against one endpoint would leave every other
// shard on the old TTL while status reported the policy applied. The operator
// cannot see the topology, so it always distributes when clustering is on.
func (r *RetentionController) buildRetentionStatements(instance *v1alpha1.LangfuseInstance, retention *v1alpha1.RetentionSpec) []string {
	// The cluster name is fixed: Langfuse's clustered migrations hardcode
	// `ON CLUSTER default`, so the tables only exist on a cluster of that name.
	onCluster := ""
	if instance.Spec.ClickHouse.ClusterEnabled() {
		onCluster = " ON CLUSTER " + langfuseClickHouseClusterName
	}
	return buildRetentionStatementsFor(retention, onCluster)
}

// langfuseClickHouseClusterName is the only cluster name Langfuse's clustered
// migrations work against. See ClickHouseClusterSpec.Enabled.
const langfuseClickHouseClusterName = "default"

func buildRetentionStatementsFor(retention *v1alpha1.RetentionSpec, onCluster string) []string {
	tableTTLs := map[string]int32{}
	if retention.Traces != nil && retention.Traces.TTLDays > 0 {
		tableTTLs["traces"] = retention.Traces.TTLDays
	}
	if retention.Observations != nil && retention.Observations.TTLDays > 0 {
		tableTTLs["observations"] = retention.Observations.TTLDays
	}
	if retention.Scores != nil && retention.Scores.TTLDays > 0 {
		tableTTLs["scores"] = retention.Scores.TTLDays
	}

	statements := make([]string, 0, len(tableTTLs))
	for _, table := range langfuseClickHouseTables {
		ttlDays, ok := tableTTLs[table.Table]
		if !ok {
			continue
		}
		stmt := fmt.Sprintf(
			"ALTER TABLE %s%s MODIFY TTL %s + INTERVAL %d DAY",
			table.Table, onCluster, table.TimestampColumn, ttlDays,
		)
		statements = append(statements, stmt)
	}

	return statements
}

// applyRetention executes the TTL statements against ClickHouse, returning how
// many succeeded. It stops at the first failure so a broken statement doesn't
// mask itself behind later successes.
func (r *RetentionController) applyRetention(ctx context.Context, instance *v1alpha1.LangfuseInstance, statements []string) (int, error) {
	log := logf.FromContext(ctx)

	ch, err := newClickHouseClient(ctx, r.Client, instance)
	if err != nil {
		return 0, err
	}

	applied := 0
	for _, stmt := range statements {
		if _, err := ch.exec(ctx, stmt); err != nil {
			return applied, fmt.Errorf("executing %q: %w", stmt, err)
		}
		log.V(1).Info("applied retention policy", "statement", stmt)
		applied++
	}
	return applied, nil
}

// evaluateStoragePressure queries ClickHouse disk usage, records it on the
// status, and raises a condition when the configured thresholds are crossed.
func (r *RetentionController) evaluateStoragePressure(ctx context.Context, instance *v1alpha1.LangfuseInstance, pressure *v1alpha1.StoragePressureSpec) {
	log := logf.FromContext(ctx)

	nodes, err := r.queryDiskUsage(ctx, instance)
	if err != nil {
		log.Error(err, "failed to query ClickHouse storage usage")
		meta.SetStatusCondition(&instance.Status.Conditions, metav1.Condition{
			Type:               "StoragePressure",
			Status:             metav1.ConditionUnknown,
			Reason:             "QueryFailed",
			Message:            fmt.Sprintf("Cannot read ClickHouse disk usage: %v", err),
			ObservedGeneration: instance.Generation,
		})
		return
	}
	usage := summarizeDiskUsage(nodes)

	if instance.Status.ClickHouse == nil {
		instance.Status.ClickHouse = &v1alpha1.ClickHouseStatus{}
	}
	instance.Status.ClickHouse.StorageUsed = humanizeBytes(usage.used)
	instance.Status.ClickHouse.StorageTotal = humanizeBytes(usage.total)

	if usage.total == 0 {
		meta.SetStatusCondition(&instance.Status.Conditions, metav1.Condition{
			Type:               "StoragePressure",
			Status:             metav1.ConditionUnknown,
			Reason:             "NoCapacityReported",
			Message:            "ClickHouse reported zero total disk capacity",
			ObservedGeneration: instance.Generation,
		})
		return
	}

	usedPercent := usage.fullestPercent
	warn, critical := pressure.WarningThresholdPercent, pressure.CriticalThresholdPercent
	summary := usage.summary(warn, critical)

	switch {
	case critical > 0 && usedPercent >= critical:
		// Pruning is intentionally not automated here: dropping partitions is
		// irreversible data loss, so the operator reports and lets a human act.
		// spec.storagePressure.pruneOldestPartitions remains unimplemented.
		meta.SetStatusCondition(&instance.Status.Conditions, metav1.Condition{
			Type:               "StoragePressure",
			Status:             metav1.ConditionTrue,
			Reason:             "CriticalThresholdExceeded",
			Message:            summary,
			ObservedGeneration: instance.Generation,
		})
	case warn > 0 && usedPercent >= warn:
		meta.SetStatusCondition(&instance.Status.Conditions, metav1.Condition{
			Type:               "StoragePressure",
			Status:             metav1.ConditionTrue,
			Reason:             "WarningThresholdExceeded",
			Message:            summary,
			ObservedGeneration: instance.Generation,
		})
	default:
		meta.SetStatusCondition(&instance.Status.Conditions, metav1.Condition{
			Type:               "StoragePressure",
			Status:             metav1.ConditionFalse,
			Reason:             "WithinThresholds",
			Message:            summary,
			ObservedGeneration: instance.Generation,
		})
	}
}

// nodeDiskUsage is one ClickHouse node's disk totals.
type nodeDiskUsage struct {
	node        string
	used, total uint64
}

// queryDiskUsage returns used and total bytes per ClickHouse node.
//
// system.disks is a local table: whichever node answers reports only its own
// disks. Behind a Service that load-balances across pods that is one arbitrary
// node per reconcile, so a clustered instance needs the cluster read through
// clusterAllReplicas — otherwise the operator reports a fraction of the
// cluster's storage as all of it, and never sees the node that is filling up.
func (r *RetentionController) queryDiskUsage(ctx context.Context, instance *v1alpha1.LangfuseInstance) ([]nodeDiskUsage, error) {
	ch, err := newClickHouseClient(ctx, r.Client, instance)
	if err != nil {
		return nil, err
	}

	// The cluster name is fixed for the same reason the retention DDL's is.
	source := "system.disks"
	if instance.Spec.ClickHouse.ClusterEnabled() {
		source = fmt.Sprintf("clusterAllReplicas('%s', system.disks)", langfuseClickHouseClusterName)
	}
	rows, err := ch.queryRows(ctx, fmt.Sprintf(
		"SELECT hostName(), sum(total_space - free_space), sum(total_space) "+
			"FROM %s GROUP BY hostName() ORDER BY hostName()", source))
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, fmt.Errorf("%s returned no rows", source)
	}

	nodes := make([]nodeDiskUsage, 0, len(rows))
	for _, row := range rows {
		fields := strings.Split(row, "\t")
		if len(fields) != 3 {
			return nil, fmt.Errorf("unexpected %s response %q", source, row)
		}
		used, err := strconv.ParseUint(strings.TrimSpace(fields[1]), 10, 64)
		if err != nil {
			return nil, fmt.Errorf("parsing used bytes: %w", err)
		}
		total, err := strconv.ParseUint(strings.TrimSpace(fields[2]), 10, 64)
		if err != nil {
			return nil, fmt.Errorf("parsing total bytes: %w", err)
		}
		nodes = append(nodes, nodeDiskUsage{node: strings.TrimSpace(fields[0]), used: used, total: total})
	}
	return nodes, nil
}

// diskUsage is what the status and the thresholds each need from per-node usage.
type diskUsage struct {
	nodes int
	// used and total are summed across nodes, so replicated data counts once per
	// replica — these are the cluster's raw disks, not its usable capacity.
	used, total uint64
	// fullest is the node with the highest utilisation, which is what the
	// thresholds run off.
	fullest        nodeDiskUsage
	fullestPercent int32
}

// summarizeDiskUsage aggregates per-node usage.
//
// The thresholds follow the fullest node rather than the cluster average:
// ClickHouse writes fail on whichever node runs out of disk, and averaging that
// node together with its emptier peers hides it until it happens. Nodes are
// already ordered by name, so equally full nodes resolve the same way every
// reconcile instead of flapping the message.
func summarizeDiskUsage(nodes []nodeDiskUsage) diskUsage {
	usage := diskUsage{nodes: len(nodes)}
	for _, n := range nodes {
		usage.used += n.used
		usage.total += n.total
		if n.total == 0 {
			continue
		}
		if percent := int32(n.used * 100 / n.total); usage.fullest.total == 0 || percent > usage.fullestPercent {
			usage.fullest, usage.fullestPercent = n, percent
		}
	}
	return usage
}

// summary describes the usage for the StoragePressure condition. On more than
// one node it names the node the percentage came from, since that is the one to
// act on, and keeps the cluster totals alongside it.
func (d diskUsage) summary(warn, critical int32) string {
	thresholds := fmt.Sprintf("warn=%d%%, critical=%d%%", warn, critical)
	if d.nodes < 2 {
		return fmt.Sprintf("ClickHouse storage %d%% used (%s of %s; %s)",
			d.fullestPercent, humanizeBytes(d.used), humanizeBytes(d.total), thresholds)
	}
	return fmt.Sprintf("ClickHouse storage %d%% used on %s, the fullest of %d nodes "+
		"(%s of %s; cluster total %s of %s; %s)",
		d.fullestPercent, d.fullest.node, d.nodes,
		humanizeBytes(d.fullest.used), humanizeBytes(d.fullest.total),
		humanizeBytes(d.used), humanizeBytes(d.total), thresholds)
}

// humanizeBytes renders a byte count using binary units, matching how
// Kubernetes resource quantities read (e.g. "12.3Gi").
func humanizeBytes(b uint64) string {
	const unit = 1024
	if b < unit {
		return fmt.Sprintf("%dB", b)
	}
	div, exp := uint64(unit), 0
	for n := b / unit; n >= unit && exp < 4; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f%ci", float64(b)/float64(div), "KMGTP"[exp])
}

// SetupWithManager sets up the controller with the Manager.
func (r *RetentionController) SetupWithManager(mgr ctrl.Manager) error {
	// Only spec changes need to reach this controller; sibling status writes do
	// not. Its own RequeueAfter picks up anything it still needs to observe.
	return ctrl.NewControllerManagedBy(mgr).
		For(&v1alpha1.LangfuseInstance{}, builder.WithPredicates(predicate.GenerationChangedPredicate{})).
		Named("retention").
		Complete(r)
}
