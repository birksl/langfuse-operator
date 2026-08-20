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

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	v1alpha1 "github.com/PalenaAI/langfuse-operator/api/v1alpha1"
	"github.com/PalenaAI/langfuse-operator/internal/resources"
)

var serviceMonitorGVK = schema.GroupVersionKind{
	Group: "monitoring.coreos.com", Version: "v1", Kind: "ServiceMonitor",
}

func serviceMonitorFor(instance *v1alpha1.LangfuseInstance) *unstructured.Unstructured {
	sm := &unstructured.Unstructured{}
	sm.SetGroupVersionKind(serviceMonitorGVK)
	sm.SetNamespace(instance.Namespace)
	sm.SetName(resources.ServiceMonitorName(instance))
	return sm
}

// newClientWithServiceMonitors builds a client that knows the Prometheus
// operator's CRD, which the fake client otherwise rejects as an unknown kind.
func newClientWithServiceMonitors(t *testing.T, objs ...client.Object) client.Client {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := v1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("scheme: %v", err)
	}
	scheme.AddKnownTypeWithName(serviceMonitorGVK, &unstructured.Unstructured{})
	list := serviceMonitorGVK
	list.Kind += "List"
	scheme.AddKnownTypeWithName(list, &unstructured.UnstructuredList{})
	return fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build()
}

// The ServiceMonitor could only ever name the web pod's JSON health route, so
// Prometheus reported the target down for as long as it existed. Leaving that
// behind on upgrade would read as an unreachable instance forever, so the
// operator removes what it created.
func TestRemoveRetiredServiceMonitor(t *testing.T) {
	t.Run("deletes the one earlier versions created", func(t *testing.T) {
		instance := chInstance()
		instance.Spec.Observability = &v1alpha1.ObservabilitySpec{
			ServiceMonitor: &v1alpha1.ServiceMonitorSpec{Enabled: true},
		}
		existing := serviceMonitorFor(instance)
		c := newClientWithServiceMonitors(t, existing)
		r := &LangfuseInstanceReconciler{Client: c, Scheme: c.Scheme()}

		if err := r.reconcilePlatform(context.Background(), instance); err != nil {
			t.Fatalf("reconcilePlatform() error: %v", err)
		}

		err := c.Get(context.Background(), client.ObjectKeyFromObject(existing), serviceMonitorFor(instance))
		if !apierrors.IsNotFound(err) {
			t.Errorf("ServiceMonitor should be gone, Get returned %v", err)
		}
	})

	t.Run("is a no-op when there is nothing to remove", func(t *testing.T) {
		instance := chInstance()
		instance.Spec.Observability = &v1alpha1.ObservabilitySpec{
			ServiceMonitor: &v1alpha1.ServiceMonitorSpec{Enabled: true},
		}
		c := newClientWithServiceMonitors(t)
		r := &LangfuseInstanceReconciler{Client: c, Scheme: c.Scheme()}

		if err := r.reconcilePlatform(context.Background(), instance); err != nil {
			t.Errorf("a missing ServiceMonitor must not fail the reconcile: %v", err)
		}
	})

	t.Run("no ServiceMonitor is created", func(t *testing.T) {
		instance := chInstance()
		instance.Spec.Observability = &v1alpha1.ObservabilitySpec{
			ServiceMonitor: &v1alpha1.ServiceMonitorSpec{Enabled: true, Interval: "15s"},
		}
		c := newClientWithServiceMonitors(t)
		r := &LangfuseInstanceReconciler{Client: c, Scheme: c.Scheme()}

		if err := r.reconcilePlatform(context.Background(), instance); err != nil {
			t.Fatalf("reconcilePlatform() error: %v", err)
		}

		err := c.Get(context.Background(), client.ObjectKeyFromObject(serviceMonitorFor(instance)),
			serviceMonitorFor(instance))
		if !apierrors.IsNotFound(err) {
			t.Errorf("enabled: true must no longer create anything, Get returned %v", err)
		}
	})

	t.Run("untouched when the field was never set", func(t *testing.T) {
		// Nothing to clean up and nothing to warn about, so the reconcile must not
		// spend an API call on it.
		instance := chInstance()
		c := newClientWithServiceMonitors(t, serviceMonitorFor(instance))
		r := &LangfuseInstanceReconciler{Client: c, Scheme: c.Scheme()}

		if err := r.reconcilePlatform(context.Background(), instance); err != nil {
			t.Fatalf("reconcilePlatform() error: %v", err)
		}

		if err := c.Get(context.Background(),
			client.ObjectKeyFromObject(serviceMonitorFor(instance)), serviceMonitorFor(instance)); err != nil {
			t.Errorf("an unrelated ServiceMonitor should be left alone: %v", err)
		}
	})
}

// A field that is silently ignored is worse than one that fails, so setting it
// has to say so on the CR.
func TestSetDeprecationCondition_FlagsServiceMonitor(t *testing.T) {
	instance := chInstance()
	instance.Spec.Observability = &v1alpha1.ObservabilitySpec{
		ServiceMonitor: &v1alpha1.ServiceMonitorSpec{Enabled: true},
	}

	setDeprecationCondition(context.Background(), instance)

	c := findCondition(instance)
	if c == nil {
		t.Fatal("expected a Deprecated condition")
	}
	for _, want := range []string{"spec.observability.serviceMonitor", "spec.observability.otel", "0.11.0"} {
		if !strings.Contains(c.Message, want) {
			t.Errorf("message %q missing %q", c.Message, want)
		}
	}
}
