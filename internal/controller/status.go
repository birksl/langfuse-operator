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
	"fmt"

	"k8s.io/apimachinery/pkg/api/equality"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/PalenaAI/langfuse-operator/api/v1alpha1"
)

// ptrTo returns a pointer to v. Status booleans are pointers so that false is
// distinguishable from "not yet determined" once serialised.
func ptrTo[T any](v T) *T { return &v }

// updateInstanceStatus writes status only when it differs from what was read.
// Seven controllers watch this CR, so an unnecessary write costs seven
// reconciles; original must be the DeepCopy taken right after the Get.
func updateInstanceStatus(ctx context.Context, c client.Client, instance, original *v1alpha1.LangfuseInstance) error {
	if equality.Semantic.DeepEqual(original.Status, instance.Status) {
		return nil
	}
	if err := c.Status().Update(ctx, instance); err != nil {
		return fmt.Errorf("updating status: %w", err)
	}
	return nil
}
