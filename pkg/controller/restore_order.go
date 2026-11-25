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
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/velero-io/velero/internal/restoreorder"
	api "github.com/velero-io/velero/pkg/apis/velero/v1"
)

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
	if err := r.kbClient.Get(ctx, key, cm); err != nil {
		return nil, fmt.Errorf("spec.resourceRestoreOrderRef: failed to get ConfigMap %s: %w", key, err)
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
