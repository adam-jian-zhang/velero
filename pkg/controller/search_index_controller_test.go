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
	"testing"
	"time"

	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/assert"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/vmware-tanzu/velero/pkg/metrics"
)

func TestSearchIndexReconciler(t *testing.T) {
	client := fake.NewClientBuilder().Build()
	logger := logrus.New()

	r := NewSearchIndexReconciler(
		client,
		logger,
		nil,
		nil,
		nil,
		"velero",
		10,
		time.Hour,
		metrics.NewServerMetrics(),
	)

	assert.NotNil(t, r)
}
