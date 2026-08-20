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
	"errors"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// conflictErr is what the API server returns when another controller wrote the
// status subresource between this one's Get and its Update.
func conflictErr() error {
	return apierrors.NewConflict(
		schema.GroupResource{Group: "langfuse.palena.ai", Resource: "langfuseinstances"},
		"langfuse",
		errors.New("the object has been modified; please apply your changes to the latest version and try again"),
	)
}

// Seven controllers write one status subresource, so losing the race is routine
// and self-correcting. Returning it as a reconcile error hands the controller to
// the rate limiter, which backs off to ~16 minutes — health probes stop running
// because a status write collided.
func TestStatusWriteFailed(t *testing.T) {
	t.Run("a conflict requeues instead of erroring", func(t *testing.T) {
		res, err := statusWriteFailed(conflictErr(), "updating health status")
		if err != nil {
			t.Fatalf("a conflict must not surface as a reconcile error: %v", err)
		}
		if res.RequeueAfter <= 0 {
			t.Error("a lost status write must be retried, or the status never lands")
		}
	})

	t.Run("a conflict wrapped by updateInstanceStatus is still recognised", func(t *testing.T) {
		// updateInstanceStatus wraps with %w; detection must survive that.
		wrapped := errors.Join(errors.New("updating status"), conflictErr())
		if !apierrors.IsConflict(wrapped) {
			t.Fatal("IsConflict should see through the wrap")
		}
		if _, err := statusWriteFailed(wrapped, "updating health status"); err != nil {
			t.Errorf("wrapped conflict should requeue, got %v", err)
		}
	})

	t.Run("any other failure is still an error", func(t *testing.T) {
		res, err := statusWriteFailed(errors.New("boom"), "updating health status")
		if err == nil {
			t.Fatal("a real failure must still be reported")
		}
		if res.RequeueAfter != 0 {
			t.Error("a real failure should leave requeueing to the rate limiter")
		}
	})
}

func TestNoteStatusWriteFailure(t *testing.T) {
	ctx := context.Background()

	if noteStatusWriteFailure(ctx, nil) {
		t.Error("a successful write needs no retry")
	}
	if !noteStatusWriteFailure(ctx, conflictErr()) {
		t.Error("a conflict must ask the caller to retry")
	}
	if noteStatusWriteFailure(ctx, errors.New("boom")) {
		t.Error("a real failure is logged, not retried by this path")
	}

	if requeueIf(true).RequeueAfter != statusConflictRetryDelay {
		t.Error("requeueIf(true) should schedule the retry")
	}
	if requeueIf(false).RequeueAfter != 0 {
		t.Error("requeueIf(false) should not schedule anything")
	}
}
