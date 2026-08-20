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
	"sort"
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

const defaultSchemaDriftCheckIntervalMinutes = 60

// SchemaDriftController detects ClickHouse schema drift for LangfuseInstance objects.
type SchemaDriftController struct {
	client.Client
	Scheme *runtime.Scheme
}

// +kubebuilder:rbac:groups=langfuse.palena.ai,resources=langfuseinstances,verbs=get;list;watch
// +kubebuilder:rbac:groups=langfuse.palena.ai,resources=langfuseinstances/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=core,resources=events,verbs=create;patch

func (r *SchemaDriftController) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
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

	// 2. Determine check interval
	checkInterval := defaultSchemaDriftCheckIntervalMinutes
	if instance.Spec.ClickHouse != nil &&
		instance.Spec.ClickHouse.SchemaDrift != nil &&
		instance.Spec.ClickHouse.SchemaDrift.CheckIntervalMinutes > 0 {
		checkInterval = int(instance.Spec.ClickHouse.SchemaDrift.CheckIntervalMinutes)
	}
	requeueAfter := time.Duration(checkInterval) * time.Minute

	// 3. If schema drift detection is disabled, skip
	if instance.Spec.ClickHouse == nil ||
		instance.Spec.ClickHouse.SchemaDrift == nil ||
		!instance.Spec.ClickHouse.SchemaDrift.Enabled {
		meta.SetStatusCondition(&instance.Status.Conditions, metav1.Condition{
			Type:               "SchemaDriftChecked",
			Status:             metav1.ConditionFalse,
			Reason:             "Disabled",
			Message:            "Schema drift detection is disabled",
			ObservedGeneration: instance.Generation,
		})
		if err := updateInstanceStatus(ctx, r.Client, instance, original); err != nil {
			return statusWriteFailed(err, "updating schema drift status")
		}
		return ctrl.Result{RequeueAfter: requeueAfter}, nil
	}

	schemaDrift := instance.Spec.ClickHouse.SchemaDrift

	// 4. Perform the schema drift check against ClickHouse.
	log.V(1).Info("running schema drift check",
		"instance", instance.Name,
		"autoRepair", schemaDrift.AutoRepair,
		"checkInterval", checkInterval,
	)

	if instance.Status.ClickHouse == nil {
		instance.Status.ClickHouse = &v1alpha1.ClickHouseStatus{}
	}

	clustered := instance.Spec.ClickHouse.ClusterEnabled()
	schemas, err := r.inspectSchema(ctx, instance)
	drift := findSchemaDrift(schemas, clustered)
	switch {
	case err != nil:
		// Unknown, not "no drift" — the check previously reported success
		// without ever querying ClickHouse.
		log.Error(err, "schema drift check failed")
		meta.SetStatusCondition(&instance.Status.Conditions, metav1.Condition{
			Type:               "SchemaDriftChecked",
			Status:             metav1.ConditionUnknown,
			Reason:             "CheckFailed",
			Message:            fmt.Sprintf("Cannot inspect ClickHouse schema: %v", err),
			ObservedGeneration: instance.Generation,
		})

	case !drift.empty():
		instance.Status.ClickHouse.SchemaDrift = ptrTo(true)
		// autoRepair is deliberately not acted on: recreating Langfuse's tables
		// belongs to its own migrations, and a wrong DDL here would corrupt the
		// schema. Missing tables almost always mean migrations have not run.
		repairNote := ""
		if schemaDrift.AutoRepair {
			repairNote = " autoRepair cannot fix this — Langfuse owns its schema; check that migrations completed."
		}
		reason := "TablesMissing"
		if len(drift.missing) == 0 {
			reason = "TablesNotReplicated"
		}
		meta.SetStatusCondition(&instance.Status.Conditions, metav1.Condition{
			Type:               "SchemaDriftChecked",
			Status:             metav1.ConditionFalse,
			Reason:             reason,
			Message:            drift.describe(clickHouseDatabase(instance)) + "." + repairNote,
			ObservedGeneration: instance.Generation,
		})

	default:
		instance.Status.ClickHouse.SchemaDrift = ptrTo(false)
		meta.SetStatusCondition(&instance.Status.Conditions, metav1.Condition{
			Type:   "SchemaDriftChecked",
			Status: metav1.ConditionTrue,
			Reason: "NoDriftDetected",
			Message: fmt.Sprintf("All %d expected Langfuse tables present in database %q on %d node(s) (next check in %dm)",
				len(expectedClickHouseTables), clickHouseDatabase(instance), drift.nodes, checkInterval),
			ObservedGeneration: instance.Generation,
		})
	}

	if err := updateInstanceStatus(ctx, r.Client, instance, original); err != nil {
		return statusWriteFailed(err, "updating schema drift status")
	}

	log.Info("reconciled schema drift detection", "instance", instance.Name, "requeueAfter", requeueAfter)
	return ctrl.Result{RequeueAfter: requeueAfter}, nil
}

// expectedClickHouseTables are the core tables Langfuse's ClickHouse migrations
// create. Their absence is the drift that actually matters in practice — it
// means migrations never ran, ran against a different database, or the schema
// was partially dropped.
//
// The check is deliberately table-level, not column-level: Langfuse owns its
// schema and changes it between versions, so an operator-side column manifest
// would produce false drift on every upgrade.
// quotedExpectedTables renders the expected tables as a SQL IN list. The names
// are compile-time constants, so there is nothing to escape.
func quotedExpectedTables() string {
	quoted := make([]string, 0, len(expectedClickHouseTables))
	for _, table := range expectedClickHouseTables {
		quoted = append(quoted, "'"+table+"'")
	}
	return strings.Join(quoted, ", ")
}

var expectedClickHouseTables = []string{
	"traces",
	"observations",
	"scores",
	"schema_migrations",
}

// nodeSchema is one ClickHouse node's view of the Langfuse database: the
// expected tables it holds, and the engine each was created with.
type nodeSchema struct {
	node    string
	engines map[string]string
}

// inspectSchema reads the Langfuse tables from every ClickHouse node.
//
// system.tables is local to whichever node answers, so a single query cannot
// tell a healthy cluster from one where the tables exist on one replica only —
// which is exactly what an unclustered migration against a replicated cluster
// produces, and what makes half the application's queries fail while the check
// reports no drift. A clustered instance is therefore read through
// clusterAllReplicas, the same way storage pressure is.
func (r *SchemaDriftController) inspectSchema(ctx context.Context, instance *v1alpha1.LangfuseInstance) ([]nodeSchema, error) {
	ch, err := newClickHouseClient(ctx, r.Client, instance)
	if err != nil {
		return nil, err
	}

	source := "system.tables"
	if instance.Spec.ClickHouse.ClusterEnabled() {
		source = fmt.Sprintf("clusterAllReplicas('%s', system.tables)", langfuseClickHouseClusterName)
	}

	// system.one is the sentinel, and it is what makes this correct: filtering
	// on the Langfuse database alone returns nothing at all from a node whose
	// copy of that database is empty, so the node that is missing everything —
	// the one worth reporting — would simply not appear in the result, and the
	// remaining nodes would look complete. Every ClickHouse node has system.one,
	// so every node that answers contributes a row.
	rows, err := ch.queryRows(ctx, fmt.Sprintf(
		"SELECT hostName(), database, name, engine FROM %s "+
			"WHERE (database = '%s' AND name IN (%s)) OR (database = 'system' AND name = 'one') "+
			"ORDER BY hostName(), name",
		source, clickHouseDatabase(instance), quotedExpectedTables()))
	if err != nil {
		return nil, err
	}

	byNode := map[string]map[string]string{}
	order := []string{}
	for _, row := range rows {
		fields := strings.Split(row, "\t")
		if len(fields) != 4 {
			return nil, fmt.Errorf("unexpected %s response %q", source, row)
		}
		node, database := strings.TrimSpace(fields[0]), strings.TrimSpace(fields[1])
		name, engine := strings.TrimSpace(fields[2]), strings.TrimSpace(fields[3])
		if _, seen := byNode[node]; !seen {
			byNode[node] = map[string]string{}
			order = append(order, node)
		}
		if database == "system" {
			continue // the sentinel: it registers the node and nothing else
		}
		byNode[node][name] = engine
	}

	if len(order) == 0 {
		return nil, fmt.Errorf("%s returned no rows at all, not even system.one", source)
	}

	schemas := make([]nodeSchema, 0, len(order))
	for _, node := range order {
		schemas = append(schemas, nodeSchema{node: node, engines: byNode[node]})
	}
	return schemas, nil
}

// driftReport describes what is wrong with the schema, per node. It is not
// named schemaDrift because Reconcile binds that identifier to the spec.
type driftReport struct {
	// missing lists, per node, the expected tables that node does not have.
	missing map[string][]string
	// unreplicated lists, per node, the tables created with a non-replicated
	// engine while the instance is configured as a cluster.
	unreplicated map[string][]string
	nodes        int
}

func (d driftReport) empty() bool { return len(d.missing) == 0 && len(d.unreplicated) == 0 }

// findSchemaDrift compares what each node holds against what the migrations
// should have created.
//
// Engines are checked, not just presence: Langfuse selects an entire migration
// directory from CLICKHOUSE_CLUSTER_ENABLED, and the unclustered one creates
// plain ReplacingMergeTree tables with no ON CLUSTER. Run against a replicated
// cluster that produces tables on one node, unreplicated, which reads as a
// healthy schema from that node and as a missing table from every other.
func findSchemaDrift(schemas []nodeSchema, clustered bool) driftReport {
	drift := driftReport{
		missing:      map[string][]string{},
		unreplicated: map[string][]string{},
		nodes:        len(schemas),
	}
	for _, schema := range schemas {
		for _, table := range expectedClickHouseTables {
			engine, present := schema.engines[table]
			switch {
			case !present:
				drift.missing[schema.node] = append(drift.missing[schema.node], table)
			case clustered && !strings.HasPrefix(engine, "Replicated"):
				drift.unreplicated[schema.node] = append(drift.unreplicated[schema.node],
					fmt.Sprintf("%s (%s)", table, engine))
			}
		}
	}
	return drift
}

// describe renders the drift for a status message. It names nodes only when
// there is more than one, so a single-node instance reads as it always did.
func (d driftReport) describe(database string) string {
	var parts []string
	if len(d.missing) > 0 {
		parts = append(parts, "missing "+d.perNode(d.missing))
	}
	if len(d.unreplicated) > 0 {
		parts = append(parts, "not replicated: "+d.perNode(d.unreplicated)+
			", which an unclustered migration produces against a replicated cluster")
	}
	return fmt.Sprintf("ClickHouse database %q across %d node(s): %s",
		database, d.nodes, strings.Join(parts, "; "))
}

func (d driftReport) perNode(byNode map[string][]string) string {
	nodes := make([]string, 0, len(byNode))
	for node := range byNode {
		nodes = append(nodes, node)
	}
	sort.Strings(nodes)

	parts := make([]string, 0, len(nodes))
	for _, node := range nodes {
		if d.nodes < 2 {
			parts = append(parts, strings.Join(byNode[node], ", "))
			continue
		}
		parts = append(parts, fmt.Sprintf("%s on %s", strings.Join(byNode[node], ", "), node))
	}
	return strings.Join(parts, "; ")
}

// SetupWithManager sets up the controller with the Manager.
func (r *SchemaDriftController) SetupWithManager(mgr ctrl.Manager) error {
	// Only spec changes need to reach this controller; sibling status writes do
	// not. Its own RequeueAfter picks up anything it still needs to observe.
	return ctrl.NewControllerManagedBy(mgr).
		For(&v1alpha1.LangfuseInstance{}, builder.WithPredicates(predicate.GenerationChangedPredicate{})).
		Named("schemadrift").
		Complete(r)
}
