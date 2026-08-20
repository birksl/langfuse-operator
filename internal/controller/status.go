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
	"time"

	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	v1alpha1 "github.com/PalenaAI/langfuse-operator/api/v1alpha1"
)

// statusConflictRetryDelay paces the retry after a lost status write. The
// sibling that won the race has already finished by then, so one short wait is
// enough — and it keeps a collision from turning into two controllers
// rewriting each other as fast as the API server will take it.
const statusConflictRetryDelay = time.Second

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

// statusWriteFailed turns a failed status write into the reconcile outcome it
// deserves.
//
// A conflict is not a failure. Seven controllers write this one subresource,
// each from its own read, so any of them can lose the race — and while a
// migration Job runs, two of them are writing every few seconds. Each
// recomputes its whole contribution from scratch on the next pass, so the
// losing write only needs redoing. Returning it as an error instead logs a
// stacktrace and hands the controller to the rate limiter, which backs off to
// ~16 minutes: health probes stop running because a status write collided.
func statusWriteFailed(err error, what string) (ctrl.Result, error) {
	if apierrors.IsConflict(err) {
		return ctrl.Result{RequeueAfter: statusConflictRetryDelay}, nil
	}
	return ctrl.Result{}, fmt.Errorf("%s: %w", what, err)
}

// requeueIf converts the retry flag from noteStatusWriteFailure into a result
// for a path that would otherwise return without one.
func requeueIf(retry bool) ctrl.Result {
	if retry {
		return ctrl.Result{RequeueAfter: statusConflictRetryDelay}
	}
	return ctrl.Result{}
}

// noteStatusWriteFailure records a best-effort status write that did not land,
// and reports whether the pass needs repeating. Conflicts are self-correcting
// only if something retries, so callers that would otherwise return without a
// requeue must use the result — status the operator computed but never
// published is status that silently lies.
func noteStatusWriteFailure(ctx context.Context, err error) (retry bool) {
	if err == nil {
		return false
	}
	if apierrors.IsConflict(err) {
		logf.FromContext(ctx).V(1).Info("status write lost a race with a sibling controller; retrying")
		return true
	}
	logf.FromContext(ctx).Error(err, "failed to update status")
	return false
}
