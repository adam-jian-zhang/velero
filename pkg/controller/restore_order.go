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

package controller

import (
	"context"
	"fmt"

	corev1api "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/velero-io/velero/internal/restoreorder"
	api "github.com/velero-io/velero/pkg/apis/velero/v1"
)

// transientRestoreOrderError marks a failure to read the rule ConfigMap that is
// not a property of the submitted Restore. The Restore is requeued, not failed.
type transientRestoreOrderError struct{ error }

func (e *transientRestoreOrderError) Unwrap() error { return e.error }

// resolveRestoreOrder snapshots the reference once per preflight attempt. The
// returned rules, rather than the mutable ConfigMap, are passed to preparation.
func (r *restoreReconciler) resolveRestoreOrder(ctx context.Context, restore *api.Restore) ([]restoreorder.Rule, error) {
	rules := restoreorder.InlineRules(restore.Spec.ResourceRestoreOrder)
	if err := restoreorder.Validate(rules); err != nil {
		return nil, err
	}
	ref := restore.Spec.ResourceRestoreOrderRef
	if err := restoreorder.ValidateReference(ref); err != nil {
		return nil, err
	}
	if ref == nil {
		return rules, nil
	}
	cm := new(corev1api.ConfigMap)
	key := client.ObjectKey{Namespace: restore.Namespace, Name: ref.Name}
	// Read directly from the API server so informer cache lag cannot turn a
	// newly created ConfigMap into a permanent validation failure.
	if err := r.globalCrClient.Get(ctx, key, cm); err != nil {
		lookupErr := fmt.Errorf("spec.resourceRestoreOrderRef: failed to get ConfigMap %s: %w", key, err)
		if apierrors.IsNotFound(err) || apierrors.IsForbidden(err) {
			// The reference is unusable until the user fixes it.
			return nil, lookupErr
		}
		// Other failures, such as API server errors, may succeed on retry.
		return nil, &transientRestoreOrderError{lookupErr}
	}
	external, err := restoreorder.ConfigMapRules(cm)
	if err != nil {
		return nil, err
	}
	r.logger.WithField("restore", client.ObjectKeyFromObject(restore).String()).
		WithField("configMap", key.String()).WithField("uid", cm.UID).
		WithField("resourceVersion", cm.ResourceVersion).Info("Resolved restore-order ConfigMap snapshot")
	return append(rules, external...), nil
}
