/*
Copyright 2026 bitkaio LLC.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0
*/

package controller

import (
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1alpha1 "github.com/PalenaAI/langfuse-operator/api/v1alpha1"
)

// conditionMigrationsComplete tracks migration progress. Its reason is a stable
// token — detail belongs in the message — because derivePhase branches on it.
const conditionMigrationsComplete = "MigrationsComplete"

const (
	reasonMigrationStarted    = "MigrationStarted"
	reasonMigrationInProgress = "MigrationInProgress"
	reasonMigrationSucceeded  = "MigrationSucceeded"
	reasonMigrationFailed     = "MigrationFailed"
)

// dependencyConditions are the datastore probes published by the health
// monitor. Web/Worker readiness is deliberately absent: it is derived from
// Deployment status instead, which does not depend on the operator's own
// network path to the workload.
var dependencyConditions = []string{
	conditionDatabaseReady,
	conditionClickHouseReady,
	conditionRedisReady,
	conditionBlobStorageReady,
}

// derivePhase computes phase and readiness from the conditions the sibling
// controllers publish. It is the only place either value is decided — two
// controllers computing phase from different inputs is what produced the
// Pending↔Degraded write loop.
func derivePhase(instance *v1alpha1.LangfuseInstance) (phase string, ready bool) {
	anyUnhealthy, allHealthy := dependencyHealth(instance)

	switch {
	case datastoreTargetChanged(instance):
		// The workload is frozen on its previous config, so nothing about the
		// current spec is running. Error rather than Degraded: it needs the spec
		// reverted, or a separate instance for the new target.
		return phaseError, false
	case migrationFailed(instance) || instanceHasFatalPodIssue(instance):
		return phaseError, false
	case migrationRunning(instance):
		return phaseMigrating, false
	case !componentsReady(instance):
		return phasePending, false
	case anyUnhealthy:
		return phaseDegraded, false
	case allHealthy:
		return phaseRunning, true
	default:
		// Probes have not reported yet.
		return phasePending, false
	}
}

func componentsReady(instance *v1alpha1.LangfuseInstance) bool {
	return instance.Status.Web != nil && instance.Status.Web.ReadyReplicas > 0 &&
		instance.Status.Worker != nil && instance.Status.Worker.ReadyReplicas > 0
}

// dependencyHealth reports whether any dependency is known-unhealthy and
// whether all of them are known-healthy. Both are false while probes are
// missing, which keeps a fresh instance in Pending rather than Degraded.
func dependencyHealth(instance *v1alpha1.LangfuseInstance) (anyUnhealthy, allHealthy bool) {
	allHealthy = true
	for _, conditionType := range dependencyConditions {
		condition := meta.FindStatusCondition(instance.Status.Conditions, conditionType)
		switch {
		case condition == nil:
			allHealthy = false
		case condition.Status != metav1.ConditionTrue:
			return true, false
		}
	}
	return false, allHealthy
}

// datastoreTargetChanged reports whether the instance controller has frozen
// workload reconciliation because the spec points at a datastore the current
// schema does not live in.
func datastoreTargetChanged(instance *v1alpha1.LangfuseInstance) bool {
	condition := meta.FindStatusCondition(instance.Status.Conditions, conditionTypeDatastoreTarget)
	return condition != nil && condition.Status == metav1.ConditionFalse
}

func migrationRunning(instance *v1alpha1.LangfuseInstance) bool {
	condition := meta.FindStatusCondition(instance.Status.Conditions, conditionMigrationsComplete)
	return condition != nil &&
		condition.Status == metav1.ConditionFalse &&
		condition.Reason != reasonMigrationFailed
}

func migrationFailed(instance *v1alpha1.LangfuseInstance) bool {
	condition := meta.FindStatusCondition(instance.Status.Conditions, conditionMigrationsComplete)
	return condition != nil &&
		condition.Status == metav1.ConditionFalse &&
		condition.Reason == reasonMigrationFailed
}
