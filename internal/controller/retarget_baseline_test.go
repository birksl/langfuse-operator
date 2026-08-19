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

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	v1alpha1 "github.com/PalenaAI/langfuse-operator/api/v1alpha1"
)

// newFakeClientWithStatus builds a fake client that serves the status
// subresource, which updateStatus writes through.
func newFakeClientWithStatus(t *testing.T, objs ...client.Object) client.Client {
	t.Helper()
	scheme := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{
		corev1.AddToScheme, appsv1.AddToScheme, v1alpha1.AddToScheme,
	} {
		if err := add(scheme); err != nil {
			t.Fatalf("scheme: %v", err)
		}
	}
	return fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(objs...).
		WithStatusSubresource(&v1alpha1.LangfuseInstance{}).
		Build()
}

// externalInstance is an instance whose datastores are both external Secrets, so
// the reference and endpoint-key components of the identity are populated.
func externalInstance(tag string) *v1alpha1.LangfuseInstance {
	return &v1alpha1.LangfuseInstance{
		ObjectMeta: metav1.ObjectMeta{Name: "lf", Namespace: "ns"},
		Spec: v1alpha1.LangfuseInstanceSpec{
			Image: v1alpha1.ImageSpec{Tag: tag},
			Auth:  v1alpha1.AuthSpec{NextAuthUrl: "https://langfuse.example.com"},
			Database: &v1alpha1.DatabaseSpec{
				External: &v1alpha1.ExternalDatabaseSpec{
					SecretRef: v1alpha1.SecretKeysRef{
						Name: "pg", Keys: map[string]string{"url": "database_url"},
					},
				},
			},
			ClickHouse: &v1alpha1.ClickHouseSpec{
				External: &v1alpha1.ExternalClickHouseSpec{
					SecretRef: v1alpha1.SecretKeysRef{
						Name: "ch",
						Keys: map[string]string{"url": "url", "migrationUrl": "migration_url"},
					},
				},
			},
		},
	}
}

// An instance migrated before appliedIdentity existed records only a version, so
// the components added since are skipped — correct for one pass, but permanent
// unless the full identity is written down. Until it is, repointing a Secret goes
// unnoticed.
func TestLegacyBaselineIdentity(t *testing.T) {
	t.Run("legacy instance baselines its current references", func(t *testing.T) {
		instance := externalInstance("3.126.0")
		instance.Status.Database = &v1alpha1.DatabaseStatus{MigrationVersion: "3.126.0"}

		baseline, ok := legacyBaselineIdentity(instance, migrationIdentity(instance))
		if !ok {
			t.Fatal("a legacy instance whose recorded components still match should baseline")
		}
		if baseline != migrationIdentity(instance) {
			t.Errorf("baseline = %q, want the full current identity %q",
				baseline, migrationIdentity(instance))
		}
		// The point of the exercise: the references are now recorded.
		for _, key := range []string{migrationIdentityPostgresRefKey, migrationIdentityClickHouseRefKey} {
			if _, present := identityComponent(baseline, key); !present {
				t.Errorf("baseline %q is missing %q", baseline, key)
			}
		}
	})

	t.Run("a baselined instance detects a later repoint", func(t *testing.T) {
		instance := externalInstance("3.126.0")
		instance.Status.Database = &v1alpha1.DatabaseStatus{MigrationVersion: "3.126.0"}
		baseline, _ := legacyBaselineIdentity(instance, migrationIdentity(instance))
		instance.Status.Migration = &v1alpha1.MigrationStatus{AppliedIdentity: baseline}

		// Without the baseline this repoint would have been invisible.
		instance.Spec.ClickHouse.External.SecretRef.Name = "ch-elsewhere"
		changed := retargetedComponents(appliedMigrationIdentity(instance), migrationIdentity(instance))
		if len(changed) != 1 || !strings.HasPrefix(changed[0], "spec.clickhouse (connection)") {
			t.Errorf("changed = %v, want the ClickHouse connection", changed)
		}
	})

	t.Run("already baselined is not rewritten", func(t *testing.T) {
		instance := externalInstance("3.126.0")
		instance.Status.Migration = &v1alpha1.MigrationStatus{AppliedIdentity: "something"}
		if _, ok := legacyBaselineIdentity(instance, migrationIdentity(instance)); ok {
			t.Error("an instance with an applied identity must not be re-baselined")
		}
	})

	t.Run("never migrated does not baseline", func(t *testing.T) {
		instance := externalInstance("3.126.0")
		if _, ok := legacyBaselineIdentity(instance, migrationIdentity(instance)); ok {
			t.Error("an instance that never migrated has nothing to baseline")
		}
	})

	t.Run("a legacy instance already retargeted does not baseline", func(t *testing.T) {
		// Recorded components disagree with the spec, so the target already moved.
		// Baselining here would launder the retarget into an applied state.
		instance := externalInstance("3.126.0")
		instance.Status.Database = &v1alpha1.DatabaseStatus{MigrationVersion: "3.126.0"}
		instance.Spec.ClickHouse.Database = "somewhere-else"

		if _, ok := legacyBaselineIdentity(instance, migrationIdentity(instance)); ok {
			t.Error("must not baseline over a target change")
		}
	})
}

// A Secret can hold several endpoints, so the key that selects one is part of the
// target. Changing it repoints the workload without touching the Secret name.
func TestMigrationIdentity_CoversEndpointKeys(t *testing.T) {
	applied := migrationIdentity(externalInstance("3.126.0"))

	t.Run("postgres url key", func(t *testing.T) {
		instance := externalInstance("3.126.0")
		instance.Spec.Database.External.SecretRef.Keys["url"] = "replica_url"
		assertRetargeted(t, applied, migrationIdentity(instance), "spec.database")
	})

	t.Run("clickhouse url key", func(t *testing.T) {
		instance := externalInstance("3.126.0")
		instance.Spec.ClickHouse.External.SecretRef.Keys["url"] = "other_url"
		assertRetargeted(t, applied, migrationIdentity(instance), "spec.clickhouse (connection)")
	})

	t.Run("clickhouse migrationUrl key", func(t *testing.T) {
		instance := externalInstance("3.126.0")
		instance.Spec.ClickHouse.External.SecretRef.Keys["migrationUrl"] = "other_migration_url"
		assertRetargeted(t, applied, migrationIdentity(instance), "spec.clickhouse (connection)")
	})

	t.Run("credential keys are not part of the target", func(t *testing.T) {
		// Rotating credentials, or moving them to differently named keys, does not
		// change which datastore this is.
		instance := externalInstance("3.126.0")
		instance.Spec.ClickHouse.External.SecretRef.Keys["username"] = "ch_user_v2"
		instance.Spec.ClickHouse.External.SecretRef.Keys["password"] = "ch_pass_v2"
		if changed := retargetedComponents(applied, migrationIdentity(instance)); len(changed) != 0 {
			t.Errorf("changed = %v, want none", changed)
		}
	})

	t.Run("omitting a mapping matches its default", func(t *testing.T) {
		// database_url and url are the defaults the env config and probes resolve,
		// so spelling them out must not read as a move.
		instance := externalInstance("3.126.0")
		delete(instance.Spec.Database.External.SecretRef.Keys, "url")
		delete(instance.Spec.ClickHouse.External.SecretRef.Keys, "url")
		if changed := retargetedComponents(applied, migrationIdentity(instance)); len(changed) != 0 {
			t.Errorf("changed = %v, want none", changed)
		}
	})
}

// The frozen path must not publish progress: the pods still run the previous
// spec, so claiming Running, ready, or the new version would be a lie that
// persists — indefinitely when migrations are disabled.
func TestUpdateStatus_FrozenReconcileDoesNotPublishProgress(t *testing.T) {
	instance := externalInstance("3.200.0")
	instance.Status.Version = "3.126.0" // what is actually deployed
	instance.Status.PublicUrl = "https://old.example.com"
	instance.Status.Web = &v1alpha1.ComponentStatus{ReadyReplicas: 1}
	instance.Status.Worker = &v1alpha1.WorkerComponentStatus{
		ComponentStatus: v1alpha1.ComponentStatus{ReadyReplicas: 1},
	}
	instance.Status.Conditions = append(dependencyStatuses(metav1.ConditionTrue), metav1.Condition{
		Type:               conditionTypeDatastoreTarget,
		Status:             metav1.ConditionFalse,
		Reason:             "TargetChangedAfterMigration",
		LastTransitionTime: metav1.Now(),
	})

	r := &LangfuseInstanceReconciler{Client: newFakeClientWithStatus(t, instance)}
	original := instance.DeepCopy()

	if err := r.updateStatus(context.Background(), instance, original, false); err != nil {
		t.Fatalf("updateStatus error: %v", err)
	}

	// Everything else looks healthy — ready replicas and green probes — so without
	// the frozen condition derivePhase would return Running/true here.
	if instance.Status.Ready {
		t.Error("a frozen instance must not report ready")
	}
	if instance.Status.Phase != phaseError {
		t.Errorf("phase = %q, want %q", instance.Status.Phase, phaseError)
	}
	if instance.Status.Version != "3.126.0" {
		t.Errorf("status.version = %q, want the deployed 3.126.0, not the spec's tag",
			instance.Status.Version)
	}
	if instance.Status.PublicUrl != "https://old.example.com" {
		t.Errorf("status.publicUrl = %q, want the deployed value", instance.Status.PublicUrl)
	}
	if ready := meta.FindStatusCondition(instance.Status.Conditions, conditionTypeReady); ready == nil {
		t.Error("Ready condition should be set")
	} else if ready.Status != metav1.ConditionFalse {
		t.Errorf("Ready condition = %v, want False", ready.Status)
	}
}

// The same instance, once the spec is applied, does advance — otherwise the guard
// above would be indistinguishable from never updating status at all.
func TestUpdateStatus_AppliedReconcileAdvancesVersion(t *testing.T) {
	instance := externalInstance("3.200.0")
	instance.Status.Version = "3.126.0"
	instance.Status.Web = &v1alpha1.ComponentStatus{ReadyReplicas: 1}
	instance.Status.Worker = &v1alpha1.WorkerComponentStatus{
		ComponentStatus: v1alpha1.ComponentStatus{ReadyReplicas: 1},
	}
	instance.Status.Conditions = dependencyStatuses(metav1.ConditionTrue)

	r := &LangfuseInstanceReconciler{Client: newFakeClientWithStatus(t, instance)}
	original := instance.DeepCopy()

	if err := r.updateStatus(context.Background(), instance, original, true); err != nil {
		t.Fatalf("updateStatus error: %v", err)
	}

	if instance.Status.Version != "3.200.0" {
		t.Errorf("status.version = %q, want 3.200.0", instance.Status.Version)
	}
	if !instance.Status.Ready || instance.Status.Phase != phaseRunning {
		t.Errorf("phase = %q ready = %v, want Running/true",
			instance.Status.Phase, instance.Status.Ready)
	}
}
