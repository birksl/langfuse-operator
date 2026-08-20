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

	v1alpha1 "github.com/PalenaAI/langfuse-operator/api/v1alpha1"
)

// A field the operator silently ignores is worse than one that fails, so every
// inert field has to announce itself — with the release it disappears in, which
// is not the same for all of them.
func TestSetDeprecationCondition_NamesEveryIgnoredField(t *testing.T) {
	cases := []struct {
		name        string
		mutate      func(*v1alpha1.LangfuseInstance)
		wantField   string
		wantRemoval string
	}{
		{"upgrade block", func(i *v1alpha1.LangfuseInstance) {
			i.Spec.Upgrade = &v1alpha1.UpgradeSpec{Strategy: "rolling"}
		}, "spec.upgrade", "0.12.0"},

		{"clickhouse encryption", func(i *v1alpha1.LangfuseInstance) {
			i.Spec.ClickHouse.Encryption = &v1alpha1.ClickHouseEncryptionSpec{Enabled: true}
		}, "spec.clickhouse.encryption", "0.12.0"},

		{"topology spread", func(i *v1alpha1.LangfuseInstance) {
			i.Spec.Web.TopologySpreadConstraints = &v1alpha1.TopologySpreadSpec{Enabled: true}
		}, "spec.web.topologySpreadConstraints", "0.12.0"},

		{"prune oldest partitions", func(i *v1alpha1.LangfuseInstance) {
			i.Spec.ClickHouse.Retention = &v1alpha1.RetentionSpec{
				StoragePressure: &v1alpha1.StoragePressureSpec{
					Enabled: true, PruneOldestPartitions: true,
				},
			}
		}, "pruneOldestPartitions", "0.12.0"},

		{"schema drift autoRepair", func(i *v1alpha1.LangfuseInstance) {
			i.Spec.ClickHouse.SchemaDrift = &v1alpha1.SchemaDriftSpec{Enabled: true, AutoRepair: true}
		}, "autoRepair", "0.12.0"},

		{"background migrations", func(i *v1alpha1.LangfuseInstance) {
			i.Spec.Database = &v1alpha1.DatabaseSpec{
				Migration: &v1alpha1.MigrationSpec{
					BackgroundMigrations: &v1alpha1.BackgroundMigrationSpec{},
				},
			}
		}, "spec.database.migration.backgroundMigrations", "0.12.0"},

		{"managed clickhouse still goes in 0.11.0", func(i *v1alpha1.LangfuseInstance) {
			i.Spec.ClickHouse = &v1alpha1.ClickHouseSpec{Managed: &v1alpha1.ManagedClickHouseSpec{}}
		}, "spec.clickhouse.managed", "0.11.0"},

		{"serviceMonitor still goes in 0.11.0", func(i *v1alpha1.LangfuseInstance) {
			i.Spec.Observability = &v1alpha1.ObservabilitySpec{
				ServiceMonitor: &v1alpha1.ServiceMonitorSpec{Enabled: true},
			}
		}, "spec.observability.serviceMonitor", "0.11.0"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			instance := chInstance()
			tc.mutate(instance)

			setDeprecationCondition(context.Background(), instance)

			c := findCondition(instance)
			if c == nil {
				t.Fatalf("%s is ignored by the operator, so it must raise the Deprecated condition", tc.wantField)
			}
			if !strings.Contains(c.Message, tc.wantField) {
				t.Errorf("message %q should name %q", c.Message, tc.wantField)
			}
			if !strings.Contains(c.Message, tc.wantRemoval) {
				t.Errorf("message %q should give the removal release %q", c.Message, tc.wantRemoval)
			}
		})
	}

	t.Run("a clean spec raises nothing", func(t *testing.T) {
		if c := findCondition(chInstance()); c != nil {
			t.Errorf("unexpected condition on a clean spec: %+v", c)
		}
	})

	t.Run("removal releases are not conflated", func(t *testing.T) {
		// Two waves of deprecation in one spec: a single "removal in X" line for
		// the whole message was wrong the moment the second wave was added.
		instance := chInstance()
		instance.Spec.ClickHouse = &v1alpha1.ClickHouseSpec{Managed: &v1alpha1.ManagedClickHouseSpec{}}
		instance.Spec.Upgrade = &v1alpha1.UpgradeSpec{}

		setDeprecationCondition(context.Background(), instance)

		c := findCondition(instance)
		if c == nil {
			t.Fatal("expected a Deprecated condition")
		}
		for _, want := range []string{
			"spec.clickhouse.managed (removal in 0.11.0",
			"spec.upgrade (removal in 0.12.0",
		} {
			if !strings.Contains(c.Message, want) {
				t.Errorf("message %q should contain %q", c.Message, want)
			}
		}
	})
}
