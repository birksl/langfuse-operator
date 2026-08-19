/*
Copyright 2026 bitkaio LLC.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0
*/

package controller

import (
	"strings"
	"testing"

	v1alpha1 "github.com/PalenaAI/langfuse-operator/api/v1alpha1"
)

// A plain ALTER reaches only the shard that served it. Replication carries it to
// the other replicas of that shard through Keeper, so a single-shard cluster
// would survive without ON CLUSTER — but shards are independent, and the operator
// cannot see the topology, so it distributes whenever clustering is on. Without
// this, RetentionApplied would report success while other shards kept their data
// indefinitely.
func TestBuildRetentionStatements_DistributesWhenClustered(t *testing.T) {
	retention := &v1alpha1.RetentionSpec{
		Traces:       &v1alpha1.TableRetentionSpec{TTLDays: 30},
		Observations: &v1alpha1.TableRetentionSpec{TTLDays: 60},
	}

	instance := func(clustered bool) *v1alpha1.LangfuseInstance {
		spec := &v1alpha1.ClickHouseSpec{}
		if clustered {
			spec.Cluster = &v1alpha1.ClickHouseClusterSpec{Enabled: true}
		}
		return &v1alpha1.LangfuseInstance{
			Spec: v1alpha1.LangfuseInstanceSpec{ClickHouse: spec},
		}
	}

	r := &RetentionController{}

	t.Run("unclustered stays local", func(t *testing.T) {
		for _, stmt := range r.buildRetentionStatements(instance(false), retention) {
			if strings.Contains(stmt, "ON CLUSTER") {
				t.Errorf("unclustered statement should not distribute: %q", stmt)
			}
		}
	})

	t.Run("clustered distributes to every shard", func(t *testing.T) {
		statements := r.buildRetentionStatements(instance(true), retention)
		if len(statements) != 2 {
			t.Fatalf("got %d statements, want 2: %v", len(statements), statements)
		}
		for _, stmt := range statements {
			// ON CLUSTER binds to the table, so it must sit between the table name
			// and MODIFY TTL — not appended, which would not parse.
			if !strings.Contains(stmt, " ON CLUSTER "+langfuseClickHouseClusterName+" MODIFY TTL ") {
				t.Errorf("statement is not a valid distributed ALTER: %q", stmt)
			}
		}
	})

	t.Run("the TTL expression is unaffected", func(t *testing.T) {
		clustered := r.buildRetentionStatements(instance(true), retention)
		unclustered := r.buildRetentionStatements(instance(false), retention)
		for i := range clustered {
			stripped := strings.Replace(clustered[i],
				" ON CLUSTER "+langfuseClickHouseClusterName, "", 1)
			if stripped != unclustered[i] {
				t.Errorf("clustered statement differs beyond ON CLUSTER:\n  %q\n  %q",
					stripped, unclustered[i])
			}
		}
	})
}
