/*
Copyright 2026 bitkaio LLC.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0
*/

package controller

import (
	"context"
	"strings"
	"testing"
)

// tablesOn renders a system.tables response for one node: the system.one
// sentinel every node returns, plus whichever Langfuse tables it holds.
func tablesOn(node string, engine string, tables ...string) []string {
	rows := []string{strings.Join([]string{node, "system", "one", "SystemOne"}, "\t")}
	for _, t := range tables {
		rows = append(rows, strings.Join([]string{node, "langfuse", t, engine}, "\t"))
	}
	return rows
}

const allTables = "traces,observations,scores,schema_migrations"

// system.tables is local to whichever node answers. Reading one node cannot
// distinguish a healthy cluster from one whose tables exist on a single replica.
func TestInspectSchema_ReadsEveryNodeWhenClustered(t *testing.T) {
	t.Run("clustered reads every replica", func(t *testing.T) {
		rows := append(
			tablesOn("ch-0", "ReplicatedReplacingMergeTree", strings.Split(allTables, ",")...),
			tablesOn("ch-1", "ReplicatedReplacingMergeTree", strings.Split(allTables, ",")...)...)
		srv, query := fakeClickHouse(t, strings.Join(rows, "\n"))
		r := &SchemaDriftController{Client: newFakeClient(t, chSecret(srv.URL))}

		schemas, err := r.inspectSchema(context.Background(), clusteredInstance())
		if err != nil {
			t.Fatalf("inspectSchema() error: %v", err)
		}
		if !strings.Contains(*query, "clusterAllReplicas('default', system.tables)") {
			t.Errorf("query = %q, want it to read every replica", *query)
		}
		if len(schemas) != 2 {
			t.Fatalf("schemas = %+v, want one entry per node", schemas)
		}
		if findSchemaDrift(schemas, true).empty() != true {
			t.Error("a fully replicated schema on both nodes is not drift")
		}
	})

	t.Run("unclustered stays local", func(t *testing.T) {
		srv, query := fakeClickHouse(t,
			strings.Join(tablesOn("ch-0", "ReplacingMergeTree", strings.Split(allTables, ",")...), "\n"))
		r := &SchemaDriftController{Client: newFakeClient(t, chSecret(srv.URL))}

		if _, err := r.inspectSchema(context.Background(), chInstance()); err != nil {
			t.Fatalf("inspectSchema() error: %v", err)
		}
		if strings.Contains(*query, "clusterAllReplicas") {
			t.Errorf("query = %q, should read the local node only", *query)
		}
	})

	t.Run("a node with an empty database still appears", func(t *testing.T) {
		// The whole point. Filtering on the Langfuse database alone returns
		// nothing from ch-1, so without the system.one sentinel the node that is
		// missing everything would be invisible and ch-0 would look complete —
		// which is precisely the outage this check exists to catch.
		rows := append(
			tablesOn("ch-0", "ReplacingMergeTree", strings.Split(allTables, ",")...),
			tablesOn("ch-1", "irrelevant")...)
		srv, query := fakeClickHouse(t, strings.Join(rows, "\n"))
		r := &SchemaDriftController{Client: newFakeClient(t, chSecret(srv.URL))}

		schemas, err := r.inspectSchema(context.Background(), clusteredInstance())
		if err != nil {
			t.Fatalf("inspectSchema() error: %v", err)
		}
		if !strings.Contains(*query, "database = 'system' AND name = 'one'") {
			t.Errorf("query = %q, want the sentinel that registers every node", *query)
		}
		if len(schemas) != 2 {
			t.Fatalf("schemas = %+v, want ch-1 present despite holding nothing", schemas)
		}
		drift := findSchemaDrift(schemas, true)
		if len(drift.missing["ch-1"]) != 4 {
			t.Errorf("missing on ch-1 = %v, want all four tables", drift.missing["ch-1"])
		}
		if len(drift.missing["ch-0"]) != 0 {
			t.Errorf("ch-0 has every table, got missing %v", drift.missing["ch-0"])
		}
	})

	t.Run("no rows at all is an error", func(t *testing.T) {
		// Not even the sentinel came back, so the query itself is broken.
		srv, _ := fakeClickHouse(t, "")
		r := &SchemaDriftController{Client: newFakeClient(t, chSecret(srv.URL))}

		if _, err := r.inspectSchema(context.Background(), clusteredInstance()); err == nil {
			t.Error("an empty response must not read as healthy")
		}
	})
}

func TestFindSchemaDrift(t *testing.T) {
	all := strings.Split(allTables, ",")

	t.Run("tables on one replica only", func(t *testing.T) {
		// The production failure: an unclustered migration ran against a
		// two-replica cluster, so the tables exist on one node, unreplicated.
		// Every query the Service routed to ch-1 failed while the old
		// single-node check reported no drift whenever ch-0 answered.
		schemas := []nodeSchema{
			{node: "ch-0", engines: enginesFor("ReplacingMergeTree", all...)},
			{node: "ch-1", engines: map[string]string{}},
		}
		drift := findSchemaDrift(schemas, true)
		if drift.empty() {
			t.Fatal("tables present on only one replica is drift")
		}
		if len(drift.missing["ch-1"]) != len(all) {
			t.Errorf("missing on ch-1 = %v, want all four tables", drift.missing["ch-1"])
		}
		message := drift.describe("langfuse")
		for _, want := range []string{"ch-1", "traces", "not replicated", "ch-0"} {
			if !strings.Contains(message, want) {
				t.Errorf("message %q should mention %q", message, want)
			}
		}
	})

	t.Run("present everywhere but not replicated", func(t *testing.T) {
		// Both nodes migrated separately: nothing is missing, but the engines
		// cannot replicate, so the two copies diverge silently.
		schemas := []nodeSchema{
			{node: "ch-0", engines: enginesFor("ReplacingMergeTree", all...)},
			{node: "ch-1", engines: enginesFor("ReplacingMergeTree", all...)},
		}
		drift := findSchemaDrift(schemas, true)
		if drift.empty() {
			t.Fatal("non-replicated engines on a cluster is drift")
		}
		if len(drift.missing) != 0 {
			t.Errorf("nothing is missing here, got %v", drift.missing)
		}
		if !strings.Contains(drift.describe("langfuse"), "ReplacingMergeTree") {
			t.Error("the message should name the engine actually found")
		}
	})

	t.Run("engines are only judged when clustering is on", func(t *testing.T) {
		// A single-node instance is supposed to have plain engines.
		schemas := []nodeSchema{{node: "ch-0", engines: enginesFor("ReplacingMergeTree", all...)}}
		if !findSchemaDrift(schemas, false).empty() {
			t.Error("plain engines are correct for an unclustered instance")
		}
	})

	t.Run("a missing table on a single node still reports", func(t *testing.T) {
		schemas := []nodeSchema{{node: "ch-0", engines: enginesFor("ReplacingMergeTree", "traces", "scores")}}
		drift := findSchemaDrift(schemas, false)
		if drift.empty() {
			t.Fatal("missing tables are drift")
		}
		// One node: the message should not bother naming it.
		if strings.Contains(drift.describe("default"), "on ch-0") {
			t.Errorf("single-node message should not name the node: %q", drift.describe("default"))
		}
	})
}

func enginesFor(engine string, tables ...string) map[string]string {
	out := map[string]string{}
	for _, t := range tables {
		out[t] = engine
	}
	return out
}

// The condition has to distinguish the two, because the remedies differ: missing
// tables mean migrations never ran, wrong engines mean they ran in the wrong
// mode and the database has to be recreated.
func TestSchemaDriftReason(t *testing.T) {
	all := strings.Split(allTables, ",")
	for _, tc := range []struct {
		name       string
		schemas    []nodeSchema
		wantReason string
	}{
		{"missing", []nodeSchema{{node: "ch-0", engines: map[string]string{}}}, "TablesMissing"},
		{"unreplicated", []nodeSchema{
			{node: "ch-0", engines: enginesFor("ReplacingMergeTree", all...)},
		}, "TablesNotReplicated"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			drift := findSchemaDrift(tc.schemas, true)
			reason := "TablesMissing"
			if len(drift.missing) == 0 {
				reason = "TablesNotReplicated"
			}
			if reason != tc.wantReason {
				t.Errorf("reason = %q, want %q", reason, tc.wantReason)
			}
		})
	}
}
