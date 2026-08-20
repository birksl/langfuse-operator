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
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1alpha1 "github.com/PalenaAI/langfuse-operator/api/v1alpha1"
)

const gib = 1024 * 1024 * 1024

// fakeClickHouse serves a canned response and records the last query it saw.
func fakeClickHouse(t *testing.T, response string) (*httptest.Server, *string) {
	t.Helper()
	var query string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body := make([]byte, r.ContentLength)
		_, _ = r.Body.Read(body)
		query = string(body)
		_, _ = w.Write([]byte(response))
	}))
	t.Cleanup(srv.Close)
	return srv, &query
}

func clusteredInstance() *v1alpha1.LangfuseInstance {
	instance := chInstance()
	instance.Spec.ClickHouse.Cluster = &v1alpha1.ClickHouseClusterSpec{Enabled: true}
	return instance
}

// system.disks is local to the node that answers, and a Service in front of a
// multi-node cluster picks one arbitrarily. A clustered instance must be read
// through clusterAllReplicas or the operator sees a fraction of the cluster.
func TestQueryDiskUsage_ReadsEveryNodeWhenClustered(t *testing.T) {
	t.Run("clustered", func(t *testing.T) {
		srv, query := fakeClickHouse(t, "ch-0\t100\t400\nch-1\t300\t400\n")
		r := &RetentionController{Client: newFakeClient(t, chSecret(srv.URL))}

		nodes, err := r.queryDiskUsage(context.Background(), clusteredInstance())
		if err != nil {
			t.Fatalf("queryDiskUsage() error: %v", err)
		}
		if !strings.Contains(*query, "clusterAllReplicas('default', system.disks)") {
			t.Errorf("query = %q, want it to read every replica", *query)
		}
		if len(nodes) != 2 {
			t.Fatalf("nodes = %v, want one entry per node", nodes)
		}
		if nodes[0] != (nodeDiskUsage{node: "ch-0", used: 100, total: 400}) {
			t.Errorf("nodes[0] = %+v, want ch-0 100/400", nodes[0])
		}
		if nodes[1].node != "ch-1" || nodes[1].used != 300 {
			t.Errorf("nodes[1] = %+v, want ch-1 300/400", nodes[1])
		}
	})

	t.Run("unclustered", func(t *testing.T) {
		// clusterAllReplicas needs a cluster in remote_servers, which a plain
		// single-node endpoint has no reason to define.
		srv, query := fakeClickHouse(t, "ch-0\t100\t400\n")
		r := &RetentionController{Client: newFakeClient(t, chSecret(srv.URL))}

		if _, err := r.queryDiskUsage(context.Background(), chInstance()); err != nil {
			t.Fatalf("queryDiskUsage() error: %v", err)
		}
		if strings.Contains(*query, "clusterAllReplicas") {
			t.Errorf("query = %q, should read the local node only", *query)
		}
		if !strings.Contains(*query, "FROM system.disks") {
			t.Errorf("query = %q, want a plain system.disks read", *query)
		}
	})

	t.Run("a short row is an error, not a zero", func(t *testing.T) {
		srv, _ := fakeClickHouse(t, "100\t400\n")
		r := &RetentionController{Client: newFakeClient(t, chSecret(srv.URL))}

		if _, err := r.queryDiskUsage(context.Background(), clusteredInstance()); err == nil {
			t.Error("expected an error for a response without the node column")
		}
	})
}

func TestSummarizeDiskUsage(t *testing.T) {
	t.Run("sums capacity and follows the fullest node", func(t *testing.T) {
		usage := summarizeDiskUsage([]nodeDiskUsage{
			{node: "ch-0", used: 10 * gib, total: 100 * gib},
			{node: "ch-1", used: 95 * gib, total: 100 * gib},
			{node: "ch-2", used: 20 * gib, total: 100 * gib},
		})

		if usage.total != 300*gib || usage.used != 125*gib {
			t.Errorf("cluster = %d/%d, want 125/300 GiB", usage.used, usage.total)
		}
		if usage.fullest.node != "ch-1" || usage.fullestPercent != 95 {
			t.Errorf("fullest = %+v at %d%%, want ch-1 at 95%%", usage.fullest, usage.fullestPercent)
		}
	})

	t.Run("a node reporting no capacity does not become the fullest", func(t *testing.T) {
		usage := summarizeDiskUsage([]nodeDiskUsage{
			{node: "ch-0", used: 0, total: 0},
			{node: "ch-1", used: 50 * gib, total: 100 * gib},
		})
		if usage.fullest.node != "ch-1" || usage.fullestPercent != 50 {
			t.Errorf("fullest = %+v at %d%%, want ch-1 at 50%%", usage.fullest, usage.fullestPercent)
		}
	})

	t.Run("single node keeps the plain message", func(t *testing.T) {
		usage := summarizeDiskUsage([]nodeDiskUsage{{node: "ch-0", used: 50 * gib, total: 100 * gib}})
		summary := usage.summary(75, 90)
		if strings.Contains(summary, "fullest") || strings.Contains(summary, "cluster total") {
			t.Errorf("summary = %q, want no cluster wording for one node", summary)
		}
		if !strings.Contains(summary, "50% used (50.0Gi of 100.0Gi") {
			t.Errorf("summary = %q, want the usage and both byte counts", summary)
		}
	})

	t.Run("clustered message names the node the percentage came from", func(t *testing.T) {
		usage := summarizeDiskUsage([]nodeDiskUsage{
			{node: "ch-0", used: 10 * gib, total: 100 * gib},
			{node: "ch-1", used: 95 * gib, total: 100 * gib},
		})
		summary := usage.summary(75, 90)
		for _, want := range []string{"95% used on ch-1", "the fullest of 2 nodes",
			"95.0Gi of 100.0Gi", "cluster total 105.0Gi of 200.0Gi", "critical=90%"} {
			if !strings.Contains(summary, want) {
				t.Errorf("summary = %q, want it to contain %q", summary, want)
			}
		}
	})
}

// The regression: one node about to fill up, the rest nearly empty. Reading a
// single node misses it whenever the Service routes elsewhere, and a cluster
// average (35%) stays below the warning threshold while writes on ch-2 are about
// to start failing.
func TestEvaluateStoragePressure_CatchesASingleFullNode(t *testing.T) {
	srv, _ := fakeClickHouse(t, strings.Join([]string{
		"ch-0\t10737418240\t107374182400",  // 10Gi of 100Gi
		"ch-1\t10737418240\t107374182400",  // 10Gi of 100Gi
		"ch-2\t101185884160\t107374182400", // 94.2Gi of 100Gi
	}, "\n"))

	instance := clusteredInstance()
	r := &RetentionController{Client: newFakeClient(t, chSecret(srv.URL))}

	r.evaluateStoragePressure(context.Background(), instance,
		&v1alpha1.StoragePressureSpec{Enabled: true, WarningThresholdPercent: 75, CriticalThresholdPercent: 90})

	condition := meta.FindStatusCondition(instance.Status.Conditions, "StoragePressure")
	if condition == nil {
		t.Fatal("StoragePressure condition should be set")
	}
	if condition.Status != metav1.ConditionTrue || condition.Reason != "CriticalThresholdExceeded" {
		t.Errorf("condition = %v/%s, want True/CriticalThresholdExceeded (cluster average is 35%%)",
			condition.Status, condition.Reason)
	}
	if !strings.Contains(condition.Message, "ch-2") {
		t.Errorf("message = %q, should name the node to act on", condition.Message)
	}
	// The totals describe the cluster, not whichever node answered.
	if got := instance.Status.ClickHouse.StorageTotal; got != "300.0Gi" {
		t.Errorf("status.storageTotal = %q, want the sum across nodes (300.0Gi)", got)
	}
}
