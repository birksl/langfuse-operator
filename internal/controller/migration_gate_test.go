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
