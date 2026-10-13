/*
Copyright The Velero Contributors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at
    http://www.apache.org/licenses/LICENSE-2.0
Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package restore

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/velero-io/velero/internal/ownerref"
)

func newOwnerRefRequest() ownerref.OwnerPatchRequest {
	return ownerref.OwnerPatchRequest{
		Group: "example.io", Version: "v1", Kind: "Child", Namespace: "work", Name: "child-0",
		RestoredUID: "child-uid", Scope: ownerref.ScopeNamespaced,
		OwnerReferences: []metav1.OwnerReference{newOwnerRef("parent-old", "parent")},
	}
}

func newOwnerRefState(requests ...ownerref.OwnerPatchRequest) *ownerref.OwnerRefRelinkState {
	state := ownerref.NewState(ownerref.NewScope())
	state.Register("parent-old", ownerref.UIDMappingRecord{
		LiveUID: "parent-new", GroupKind: schema.GroupKind{Group: "example.io", Kind: "Parent"},
		APIVersion: "example.io/v1", ServedAPIVersions: []string{"example.io/v1"},
		Name: "parent", Namespace: "work", Scope: ownerref.ScopeNamespaced,
	})
	for _, req := range requests {
		state.Enqueue(req)
	}
	return state
}

func TestOwnerRefMergeFieldPrecedence(t *testing.T) {
	for _, tc := range []struct {
		name                                               string
		sameUID                                            bool
		liveController, incomingController, wantController *bool
		liveBlocking, incomingBlocking, wantBlocking       *bool
	}{
		{name: "same UID keeps live true", sameUID: true, liveController: ptr.To(true), incomingController: ptr.To(false), wantController: ptr.To(true), liveBlocking: ptr.To(true), incomingBlocking: ptr.To(false), wantBlocking: ptr.To(true)},
		{name: "same UID keeps explicit live false", sameUID: true, liveController: ptr.To(false), incomingController: ptr.To(true), wantController: ptr.To(false), liveBlocking: ptr.To(false), incomingBlocking: ptr.To(true), wantBlocking: ptr.To(false)},
		{name: "same UID fills unset flags", sameUID: true, incomingController: ptr.To(true), wantController: ptr.To(true), incomingBlocking: ptr.To(false), wantBlocking: ptr.To(false)},
		{name: "same UID preserves unset", sameUID: true},
		{name: "replacement UID uses incoming flags", liveController: ptr.To(false), incomingController: ptr.To(true), wantController: ptr.To(true), liveBlocking: ptr.To(true), incomingBlocking: ptr.To(false), wantBlocking: ptr.To(false)},
		{name: "replacement UID retains live controller", liveController: ptr.To(true), incomingController: ptr.To(false), wantController: ptr.To(true), liveBlocking: ptr.To(false), incomingBlocking: ptr.To(true), wantBlocking: ptr.To(true)},
		{name: "replacement UID does not inherit stale blocking", liveBlocking: ptr.To(true)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			live := newOwnerRef("stale-parent", "parent")
			incoming := newOwnerRef("parent-new", "parent")
			if tc.sameUID {
				live.UID = incoming.UID
			}
			live.Controller, incoming.Controller = tc.liveController, tc.incomingController
			live.BlockOwnerDeletion, incoming.BlockOwnerDeletion = tc.liveBlocking, tc.incomingBlocking
			unrelated := newOwnerRef("other", "other")
			unrelated.APIVersion = "example.io/removed"
			originalLive, originalIncoming := live.DeepCopy(), incoming.DeepCopy()
			versions := ownerRefServedVersions{schema.GroupVersionKind{Group: "example.io", Version: "v1", Kind: "Parent"}: true}
			merged := mergeOwnerReferences([]metav1.OwnerReference{unrelated, live}, []metav1.OwnerReference{incoming}, func(string) {}, versions)
			require.Len(t, merged, 2)
			assert.Equal(t, unrelated, merged[0])
			assert.Equal(t, types.UID("parent-new"), merged[1].UID)
			assert.Equal(t, tc.wantController, merged[1].Controller)
			assert.Equal(t, tc.wantBlocking, merged[1].BlockOwnerDeletion)
			assert.Equal(t, *originalLive, live)
			assert.Equal(t, *originalIncoming, incoming)
			assert.Equal(t, merged, mergeOwnerReferences(merged, []metav1.OwnerReference{incoming}, func(string) {}, versions), "retry merge must be idempotent")
		})
	}
}

func TestOwnerRefMergeDemotesWithoutLosingLinksAndDeduplicates(t *testing.T) {
	first, second := newOwnerRef("a", "a"), newOwnerRef("b", "b")
	first.Controller, second.Controller = ptr.To(true), ptr.To(true)
	warnings := 0
	merged := mergeOwnerReferences(nil, []metav1.OwnerReference{first, second, second}, func(string) { warnings++ }, nil)
	require.Len(t, merged, 2)
	assert.Equal(t, ptr.To(true), merged[0].Controller)
	assert.Equal(t, ptr.To(false), merged[1].Controller)
	assert.Equal(t, second.UID, merged[1].UID)
	assert.Equal(t, 2, warnings)
}

func TestOwnerRefSinglePassConflictPreservesConcurrentChanges(t *testing.T) {
	child := newOwnerRefChild()
	base := newOwnerRefRestoreClient(t, child)
	calls, gets := 0, 0
	c := &ownerRefInterceptClient{Client: base}
	c.get = func(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
		assert.Equal(t, "Child", obj.GetObjectKind().GroupVersionKind().Kind, "relinking must never GET parents")
		gets++
		return base.Get(ctx, key, obj, opts...)
	}
	c.patch = func(ctx context.Context, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
		calls++
		if calls == 1 {
			current := child.DeepCopy()
			require.NoError(t, base.Get(ctx, client.ObjectKeyFromObject(child), current))
			owner := newOwnerRef("live-owner", "live")
			owner.Controller = ptr.To(true)
			current.SetOwnerReferences([]metav1.OwnerReference{owner})
			current.SetAnnotations(map[string]string{"concurrent": "preserve"})
			require.NoError(t, base.Update(ctx, current))
			return apierrors.NewConflict(schema.GroupResource{Resource: "children"}, child.GetName(), fmt.Errorf("concurrent write"))
		}
		return base.Patch(ctx, obj, patch, opts...)
	}
	req := newOwnerRefRequest()
	req.OwnerReferences[0].Controller = ptr.To(true)
	restore := newOwnerRefRestore()
	warnings, errs := ApplyOwnerRefRelinking(t.Context(), c, restore, newOwnerRefState(req))
	require.True(t, errs.IsEmpty(), "%+v", errs)
	assert.False(t, warnings.IsEmpty())
	assert.Equal(t, 2, calls)
	assert.Equal(t, 2, gets)
	assert.Equal(t, 1, restore.Status.OwnerRefsRelinked)
	require.NoError(t, base.Get(t.Context(), client.ObjectKeyFromObject(child), child))
	require.Len(t, child.GetOwnerReferences(), 2)
	assert.Equal(t, ptr.To(true), child.GetOwnerReferences()[0].Controller)
	assert.Equal(t, ptr.To(false), child.GetOwnerReferences()[1].Controller)
	assert.Equal(t, types.UID("parent-new"), child.GetOwnerReferences()[1].UID)
	assert.Equal(t, "preserve", child.GetAnnotations()["concurrent"])
	assert.Equal(t, "parent-old", child.Object["spec"].(map[string]any)["parentUID"])
}

func TestOwnerRefSinglePassLostPatchResponse(t *testing.T) {
	child := newOwnerRefChild()
	base := newOwnerRefRestoreClient(t, child)
	calls := 0
	c := &ownerRefInterceptClient{Client: base, patch: func(ctx context.Context, obj client.Object, p client.Patch, opts ...client.PatchOption) error {
		calls++
		if err := base.Patch(ctx, obj, p, opts...); err != nil {
			return err
		}
		if calls == 1 {
			return io.ErrUnexpectedEOF
		}
		return nil
	}}
	req := newOwnerRefRequest()
	req.OwnerReferences[0].Controller, req.OwnerReferences[0].BlockOwnerDeletion = ptr.To(true), ptr.To(true)
	restore := newOwnerRefRestore()
	state := newOwnerRefState(req)
	_, errs := ApplyOwnerRefRelinking(t.Context(), c, restore, state)
	require.True(t, errs.IsEmpty(), "%+v", errs)
	assert.Equal(t, 2, calls)
	assert.Equal(t, 1, restore.Status.OwnerRefsRelinked)
	require.NoError(t, base.Get(t.Context(), client.ObjectKeyFromObject(child), child))
	require.Len(t, child.GetOwnerReferences(), 1)
	assert.Equal(t, types.UID("parent-new"), child.GetOwnerReferences()[0].UID)
	_, errs = ApplyOwnerRefRelinking(t.Context(), c, restore, state)
	assert.True(t, errs.IsEmpty())
	assert.Equal(t, 2, calls, "completed in-memory state cannot replay requests")
}

func TestOwnerRefSinglePassFailures(t *testing.T) {
	for _, tc := range []struct {
		name     string
		failure  error
		attempts int
	}{
		{"throttled", apierrors.NewTooManyRequests("busy", 0), 4},
		{"internal", apierrors.NewInternalError(fmt.Errorf("transient")), 4},
		{"unavailable", apierrors.NewServiceUnavailable("transient"), 4},
		{"timeout", apierrors.NewTimeoutError("transient", 0), 4},
		{"forbidden", apierrors.NewForbidden(schema.GroupResource{}, "child", fmt.Errorf("cannot set blockOwnerDeletion")), 1},
		{"proxy forbidden", apierrors.NewGenericServerResponse(http.StatusForbidden, http.MethodPatch, schema.GroupResource{}, "child", "proxy deny", 0, true), 1},
		{"proxy unauthorized", apierrors.NewGenericServerResponse(http.StatusUnauthorized, http.MethodPatch, schema.GroupResource{}, "child", "unauthorized", 0, true), 1},
		{"proxy invalid", apierrors.NewGenericServerResponse(http.StatusUnprocessableEntity, http.MethodPatch, schema.GroupResource{}, "child", "invalid", 0, true), 1},
		{"invalid", apierrors.NewBadRequest("invalid"), 1},
		{"missing child", apierrors.NewNotFound(schema.GroupResource{}, "child"), 1},
		{"reset", &url.Error{Op: "Patch", URL: "https://apiserver", Err: &net.OpError{Op: "read", Net: "tcp", Err: syscall.ECONNRESET}}, 4},
		{"refused", &net.OpError{Op: "dial", Net: "tcp", Err: syscall.ECONNREFUSED}, 4},
	} {
		t.Run(tc.name, func(t *testing.T) {
			child := newOwnerRefChild()
			sibling := newOwnerRefChild()
			sibling.SetName("sibling")
			base := newOwnerRefRestoreClient(t, child, sibling)
			calls := 0
			c := &ownerRefInterceptClient{Client: base, patch: func(ctx context.Context, obj client.Object, p client.Patch, opts ...client.PatchOption) error {
				if obj.GetName() == "sibling" {
					return base.Patch(ctx, obj, p, opts...)
				}
				calls++
				return tc.failure
			}}
			first, second := newOwnerRefRequest(), newOwnerRefRequest()
			first.OwnerReferences[0].BlockOwnerDeletion = ptr.To(true)
			second.Name = "sibling"
			restore := newOwnerRefRestore()
			_, errs := ApplyOwnerRefRelinking(t.Context(), c, restore, newOwnerRefState(first, second))
			assert.Equal(t, tc.attempts, calls)
			assert.Len(t, errs.Namespaces["work"], 1)
			assert.Contains(t, errs.Namespaces["work"][0], "child-0")
			assert.Contains(t, errs.Velero[0], "1 child requests unfinished")
			assert.Equal(t, 1, restore.Status.OwnerRefsRelinked, "other children must still be attempted")
			require.NoError(t, base.Get(t.Context(), client.ObjectKeyFromObject(child), child))
			assert.Empty(t, child.GetOwnerReferences())
		})
	}
	assert.Equal(t, 4, retry.DefaultBackoff.Steps)
}

func TestOwnerRefDeadlineAccountsForUnattemptedAndOmittedChildren(t *testing.T) {
	req := newOwnerRefRequest()
	sibling, omission := req, req
	sibling.Name = "unattempted"
	omission.Name = "omitted"
	omission.OwnerReferences = []metav1.OwnerReference{newOwnerRef("missing", "missing")}
	gets, patches := 0, 0
	c := &ownerRefInterceptClient{Client: newOwnerRefRestoreClient(t, newOwnerRefChild())}
	c.get = func(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
		gets++
		return c.Client.Get(ctx, key, obj, opts...)
	}
	c.patch = func(ctx context.Context, obj client.Object, p client.Patch, opts ...client.PatchOption) error {
		patches++
		<-ctx.Done()
		return ctx.Err()
	}
	restore := newOwnerRefRestore()
	state := newOwnerRefState(req, sibling, omission)
	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	warnings, errs := ApplyOwnerRefRelinking(ctx, c, restore, state)
	require.Len(t, errs.Namespaces["work"], 2)
	assert.Contains(t, errs.Namespaces["work"][1], "unattempted")
	assert.Contains(t, errs.Velero[0], "2 child requests unfinished")
	assert.False(t, warnings.IsEmpty(), "missing owners remain omissions even after the deadline")
	assert.Equal(t, 1, gets)
	assert.Equal(t, 1, patches)
	assert.Zero(t, restore.Status.OwnerRefsRelinked)
}

func TestOwnerRefRetryAfterHonorsStageCancellation(t *testing.T) {
	calls := 0
	c := &ownerRefInterceptClient{Client: newOwnerRefRestoreClient(t, newOwnerRefChild()), patch: func(context.Context, client.Object, client.Patch, ...client.PatchOption) error {
		calls++
		return apierrors.NewTooManyRequests("back off", 60)
	}}
	state := newOwnerRefState(newOwnerRefRequest())
	restore := newOwnerRefRestore()
	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	_, errs := ApplyOwnerRefRelinking(ctx, c, restore, state)
	assert.False(t, errs.IsEmpty())
	assert.Equal(t, 1, calls, "Retry-After must not be bypassed or extend the stage deadline")
}

func TestOwnerRefReplacementChildAfterConflict(t *testing.T) {
	child := newOwnerRefChild()
	base := newOwnerRefRestoreClient(t, child)
	calls := 0
	c := &ownerRefInterceptClient{Client: base, patch: func(ctx context.Context, obj client.Object, p client.Patch, opts ...client.PatchOption) error {
		calls++
		if calls == 1 {
			require.NoError(t, base.Delete(ctx, child))
			replacement := newOwnerRefChild()
			replacement.SetUID("replacement")
			require.NoError(t, base.Create(ctx, replacement))
			return apierrors.NewConflict(schema.GroupResource{}, child.GetName(), fmt.Errorf("replaced"))
		}
		return base.Patch(ctx, obj, p, opts...)
	}}
	warnings, errs := ApplyOwnerRefRelinking(t.Context(), c, newOwnerRefRestore(), newOwnerRefState(newOwnerRefRequest()))
	require.True(t, errs.IsEmpty(), "%+v", errs)
	assert.Contains(t, fmt.Sprint(warnings), "replacement child")
	require.NoError(t, base.Get(t.Context(), client.ObjectKeyFromObject(child), child))
	assert.Equal(t, types.UID("replacement"), child.GetUID())
	require.Len(t, child.GetOwnerReferences(), 1)
}

func TestOwnerRefGetFailuresConsumeAnAttemptWithoutPatching(t *testing.T) {
	for _, transient := range []bool{false, true} {
		t.Run(strconv.FormatBool(transient), func(t *testing.T) {
			base := newOwnerRefRestoreClient(t, newOwnerRefChild())
			gets, patches := 0, 0
			c := &ownerRefInterceptClient{Client: base}
			c.get = func(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
				gets++
				if gets == 1 {
					if transient {
						return apierrors.NewServiceUnavailable("retry GET")
					}
					return apierrors.NewForbidden(schema.GroupResource{}, "child", fmt.Errorf("GET denied"))
				}
				return base.Get(ctx, key, obj, opts...)
			}
			c.patch = func(ctx context.Context, obj client.Object, p client.Patch, opts ...client.PatchOption) error {
				patches++
				return base.Patch(ctx, obj, p, opts...)
			}
			restore := newOwnerRefRestore()
			_, errs := ApplyOwnerRefRelinking(t.Context(), c, restore, newOwnerRefState(newOwnerRefRequest()))
			if transient {
				require.True(t, errs.IsEmpty(), "%+v", errs)
				assert.Equal(t, 2, gets)
				assert.Equal(t, 1, patches)
				assert.Equal(t, 1, restore.Status.OwnerRefsRelinked)
			} else {
				assert.False(t, errs.IsEmpty())
				assert.Equal(t, 1, gets)
				assert.Zero(t, patches)
				assert.Zero(t, restore.Status.OwnerRefsRelinked)
			}
		})
	}
}
