/*
Copyright 2026 bitkaio LLC.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0
*/

package controller

import (
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1alpha1 "github.com/PalenaAI/langfuse-operator/api/v1alpha1"
	"github.com/PalenaAI/langfuse-operator/internal/langfuse"
)

// The gate must read the version this controller migrated, not the one the
// instance controller deployed. Status.Version is stamped on every instance
// reconcile, so gating on it meant whichever controller won the race on a fresh
// CR decided whether migrations ever ran — and it never reopened afterwards.
func TestMigrationGate_UsesMigratedVersionNotDeployedVersion(t *testing.T) {
	cases := []struct {
		name              string
		deployedVersion   string // status.version, written by the instance controller
		migratedVersion   string // status.database.migrationVersion, written here
		tag               string
		wantShouldMigrate bool
	}{
		{
			name:            "fresh instance the instance controller already stamped",
			deployedVersion: "3.126.0", migratedVersion: "", tag: "3.126.0",
			wantShouldMigrate: true, // the bug: this used to be skipped forever
		},
		{"never migrated", "", "", "3.126.0", true},
		{"already migrated same version", "3.126.0", "3.126.0", "3.126.0", false},
		{"upgrade", "3.126.0", "3.100.0", "3.126.0", true},
		{"normalised v-prefix counts as migrated", "3.126.0", "3.126.0", "v3.126.0", false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			instance := &v1alpha1.LangfuseInstance{
				ObjectMeta: metav1.ObjectMeta{Name: "lf", Namespace: "ns"},
				Spec:       v1alpha1.LangfuseInstanceSpec{Image: v1alpha1.ImageSpec{Tag: tc.tag}},
				Status:     v1alpha1.LangfuseInstanceStatus{Version: tc.deployedVersion},
			}
			if tc.migratedVersion != "" {
				instance.Status.Database = &v1alpha1.DatabaseStatus{MigrationVersion: tc.migratedVersion}
			}

			// Mirrors the gate in Reconcile.
			current := ""
			if instance.Status.Database != nil {
				current = instance.Status.Database.MigrationVersion
			}
			shouldMigrate := langfuse.VersionChanged(instance.Spec.Image.Tag, current) || current == ""

			if shouldMigrate != tc.wantShouldMigrate {
				t.Errorf("shouldMigrate = %v, want %v (deployed=%q migrated=%q tag=%q)",
					shouldMigrate, tc.wantShouldMigrate, tc.deployedVersion, tc.migratedVersion, tc.tag)
			}
		})
	}
}

// The gate must reopen when the migration target changes, not only the tag —
// otherwise repointing at a different (empty) ClickHouse database gives the
// workload a new CLICKHOUSE_DB against a schema that was never created.
func TestMigrationGate_ReopensWhenTheTargetChanges(t *testing.T) {
	withStatus := func(tag, appliedIdentity, legacyVersion, database string) *v1alpha1.LangfuseInstance {
		instance := &v1alpha1.LangfuseInstance{
			Spec: v1alpha1.LangfuseInstanceSpec{
				Image:      v1alpha1.ImageSpec{Tag: tag},
				ClickHouse: &v1alpha1.ClickHouseSpec{Database: database},
			},
		}
		if appliedIdentity != "" {
			instance.Status.Migration = &v1alpha1.MigrationStatus{AppliedIdentity: appliedIdentity}
		}
		if legacyVersion != "" {
			instance.Status.Database = &v1alpha1.DatabaseStatus{MigrationVersion: legacyVersion}
		}
		return instance
	}

	cases := []struct {
		name              string
		instance          *v1alpha1.LangfuseInstance
		wantShouldMigrate bool
	}{
		{
			name:              "same tag and database — nothing to do",
			instance:          withStatus("3.126.0", buildMigrationIdentity("3.126.0", "langfuse", false), "", "langfuse"),
			wantShouldMigrate: false,
		},
		{
			name:              "database retargeted on an unchanged tag",
			instance:          withStatus("3.126.0", buildMigrationIdentity("3.126.0", "old", false), "", "new"),
			wantShouldMigrate: true,
		},
		{
			name:              "tag upgraded on an unchanged database",
			instance:          withStatus("3.126.0", buildMigrationIdentity("3.100.0", "langfuse", false), "", "langfuse"),
			wantShouldMigrate: true,
		},
		{
			// Migrated by an operator predating appliedIdentity: it necessarily
			// ran against "default", so back-filling avoids a spurious re-run.
			name:              "legacy status, still on default",
			instance:          withStatus("3.126.0", "", "3.126.0", ""),
			wantShouldMigrate: false,
		},
		{
			name:              "legacy status, now retargeted off default",
			instance:          withStatus("3.126.0", "", "3.126.0", "langfuse"),
			wantShouldMigrate: true,
		},
		{
			name:              "never migrated",
			instance:          withStatus("3.126.0", "", "", "langfuse"),
			wantShouldMigrate: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			shouldMigrate := appliedMigrationIdentity(tc.instance) != migrationIdentity(tc.instance)
			if shouldMigrate != tc.wantShouldMigrate {
				t.Errorf("shouldMigrate = %v, want %v (applied=%q desired=%q)",
					shouldMigrate, tc.wantShouldMigrate,
					appliedMigrationIdentity(tc.instance), migrationIdentity(tc.instance))
			}
		})
	}
}

// A succeeded Job outlives its migration by TTLSecondsAfterFinished (1h), and
// its name carries no version — so an upgrade inside that window must not read
// the old Job's success as its own.
func TestMigrationIdentity_DistinguishesStaleJobs(t *testing.T) {
	instance := &v1alpha1.LangfuseInstance{
		Spec: v1alpha1.LangfuseInstanceSpec{
			Image:      v1alpha1.ImageSpec{Tag: "3.126.0"},
			ClickHouse: &v1alpha1.ClickHouseSpec{Database: "langfuse"},
		},
	}
	desired := migrationIdentity(instance)

	previousVersion := buildMigrationIdentity("3.100.0", "langfuse", false)
	if previousVersion == desired {
		t.Error("a Job from the previous version must not match the desired identity")
	}

	otherDatabase := buildMigrationIdentity("3.126.0", "other", false)
	if otherDatabase == desired {
		t.Error("a Job for another database must not match the desired identity")
	}

	// A Job predating the annotation has no identity at all, so it is replaced
	// rather than trusted.
	if desired == "" {
		t.Error("desired identity must never be empty, or an unannotated Job would match it")
	}

	// The v-prefix is normalised away, so v3.126.0 and 3.126.0 are one target.
	if got := buildMigrationIdentity("v3.126.0", "langfuse", false); got != desired {
		t.Errorf("normalised identity = %q, want %q", got, desired)
	}
}

// Clustering fixes the table engine at CREATE time, so it must be recoverable
// from a recorded identity — that is how the controller tells a switch it cannot
// perform from a re-migration it can.
func TestIdentityClusterMode(t *testing.T) {
	clustered := buildMigrationIdentity("3.126.0", "langfuse", true)
	unclustered := buildMigrationIdentity("3.126.0", "langfuse", false)

	if clustered == unclustered {
		t.Fatal("clustered and unclustered identities must differ")
	}

	mode, ok := identityClusterMode(clustered)
	if !ok || mode != "true" {
		t.Errorf("clustered mode = %q (found=%v), want true", mode, ok)
	}
	mode, ok = identityClusterMode(unclustered)
	if !ok || mode != "false" {
		t.Errorf("unclustered mode = %q (found=%v), want false", mode, ok)
	}

	// A database name containing the separator must not confuse extraction.
	if mode, _ := identityClusterMode(buildMigrationIdentity("3.126.0", "db-clickhouse-cluster=x", true)); mode != "true" {
		t.Errorf("mode = %q, want true", mode)
	}

	// An identity from before clustering was tracked has no component, and the
	// controller must treat that as "unknown" rather than "false".
	if _, ok := identityClusterMode("3.126.0|clickhouse-db=langfuse"); ok {
		t.Error("a pre-clustering identity should report no recorded mode")
	}
}

// Instances migrated by an operator that forced CLICKHOUSE_CLUSTER_ENABLED=false
// really did run unclustered, so back-filling false is accurate — and enabling
// clustering on them must therefore be detected as a switch.
func TestAppliedMigrationIdentity_BackfillsUnclustered(t *testing.T) {
	instance := &v1alpha1.LangfuseInstance{
		Spec: v1alpha1.LangfuseInstanceSpec{
			Image:      v1alpha1.ImageSpec{Tag: "3.126.0"},
			ClickHouse: &v1alpha1.ClickHouseSpec{Cluster: &v1alpha1.ClickHouseClusterSpec{Enabled: true}},
		},
		Status: v1alpha1.LangfuseInstanceStatus{
			Database: &v1alpha1.DatabaseStatus{MigrationVersion: "3.126.0"},
		},
	}

	was, ok := identityClusterMode(appliedMigrationIdentity(instance))
	if !ok || was != "false" {
		t.Fatalf("back-filled mode = %q (found=%v), want false", was, ok)
	}
	want, _ := identityClusterMode(migrationIdentity(instance))
	if want != "true" {
		t.Fatalf("desired mode = %q, want true", want)
	}
	if was == want {
		t.Error("enabling clustering on a legacy instance must register as a switch")
	}
}

// The operator's own ClickHouse queries must target whatever database the
// workload uses, or schema-drift detection reports phantom drift by looking in
// the wrong place.
func TestClickHouseDatabase(t *testing.T) {
	cases := []struct {
		name     string
		instance *v1alpha1.LangfuseInstance
		want     string
	}{
		{"no clickhouse block", &v1alpha1.LangfuseInstance{}, "default"},
		{
			name: "unset falls back to Langfuse's own default",
			instance: &v1alpha1.LangfuseInstance{Spec: v1alpha1.LangfuseInstanceSpec{
				ClickHouse: &v1alpha1.ClickHouseSpec{},
			}},
			want: "default",
		},
		{
			name: "explicit database is honoured",
			instance: &v1alpha1.LangfuseInstance{Spec: v1alpha1.LangfuseInstanceSpec{
				ClickHouse: &v1alpha1.ClickHouseSpec{Database: "langfuse"},
			}},
			want: "langfuse",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := clickHouseDatabase(tc.instance); got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

// up.sh exits 1 without CLICKHOUSE_MIGRATION_URL, CLICKHOUSE_USER or
// CLICKHOUSE_PASSWORD, and the operator only emits those for keys present in
// the spec — so a Job built without them can never succeed.
func TestMissingClickHouseMigrationKeys(t *testing.T) {
	external := func(keys map[string]string) *v1alpha1.LangfuseInstance {
		return &v1alpha1.LangfuseInstance{
			Spec: v1alpha1.LangfuseInstanceSpec{
				ClickHouse: &v1alpha1.ClickHouseSpec{
					External: &v1alpha1.ExternalClickHouseSpec{
						SecretRef: v1alpha1.SecretKeysRef{Name: "ch", Keys: keys},
					},
				},
			},
		}
	}

	cases := []struct {
		name     string
		instance *v1alpha1.LangfuseInstance
		want     []string
	}{
		{
			name:     "url only — the common mistake",
			instance: external(map[string]string{"url": "url"}),
			want:     []string{"migrationUrl", "username", "password"},
		},
		{
			name: "complete",
			instance: external(map[string]string{
				"url": "url", "migrationUrl": "migration_url",
				"username": "user", "password": "pass",
			}),
			want: nil,
		},
		{
			name:     "empty value counts as missing",
			instance: external(map[string]string{"url": "url", "migrationUrl": "", "username": "u", "password": "p"}),
			want:     []string{"migrationUrl"},
		},
		{
			name:     "no clickhouse configured",
			instance: &v1alpha1.LangfuseInstance{},
			want:     nil,
		},
		{
			name: "managed mode derives all three itself",
			instance: &v1alpha1.LangfuseInstance{
				Spec: v1alpha1.LangfuseInstanceSpec{
					ClickHouse: &v1alpha1.ClickHouseSpec{Managed: &v1alpha1.ManagedClickHouseSpec{}},
				},
			},
			want: nil,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := missingClickHouseMigrationKeys(tc.instance)
			if len(got) != len(tc.want) {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Errorf("got %v, want %v", got, tc.want)
					break
				}
			}
		})
	}
}
