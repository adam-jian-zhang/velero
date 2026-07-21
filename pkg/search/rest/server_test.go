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

package rest

import (
	"testing"
	"encoding/json"
	"net/http"
	"net/http/httptest"

	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	authv1 "k8s.io/api/authentication/v1"
	authzv1 "k8s.io/api/authorization/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/vmware-tanzu/velero/pkg/plugin/velero"
	veleromocks "github.com/vmware-tanzu/velero/pkg/plugin/velero/mocks"
)

func TestParseSearchParams(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/apis/search.velero.io/v2alpha1/resources?name=ng*&namespace=prod&kind=Deployment&apiVersion=apps/v1&label=app=nginx&label=tier=front&limit=50&offset=10", nil)
	params, err := parseSearchParams(req, "b1")
	require.NoError(t, err)
	assert.Equal(t, "ng*", params.Name)
	assert.Equal(t, "prod", params.Namespace)
	assert.Equal(t, "Deployment", params.Kind)
	assert.Equal(t, "apps/v1", params.APIVersion)
	assert.Equal(t, "b1", params.BackupName)
	assert.Equal(t, map[string]string{"app": "nginx", "tier": "front"}, params.Labels)
	assert.Equal(t, 50, params.Limit)
	assert.Equal(t, 10, params.Offset)
}

func TestAuthAndSearch(t *testing.T) {
	kube := fake.NewSimpleClientset()
	kube.Fake.PrependReactor("create", "tokenreviews", func(action k8stesting.Action) (bool, runtime.Object, error) {
		return true, &authv1.TokenReview{
			Status: authv1.TokenReviewStatus{
				Authenticated: true,
				User:          authv1.UserInfo{Username: "user", Groups: []string{"g"}},
			},
		}, nil
	})
	kube.Fake.PrependReactor("create", "subjectaccessreviews", func(action k8stesting.Action) (bool, runtime.Object, error) {
		return true, &authzv1.SubjectAccessReview{Status: authzv1.SubjectAccessReviewStatus{Allowed: true}}, nil
	})

	sp := veleromocks.NewSearchProvider(t)
	sp.On("Search", mock.Anything, velero.SearchParams{
		Kind: "Pod", Labels: map[string]string{}, Limit: 100,
	}).Return(velero.SearchResult{
		Records:    []velero.ResourceRecord{{BackupName: "b", ResourceName: "p", Kind: "Pod"}},
		TotalCount: 1,
	}, nil)

	srv := New(":0", sp, kube, logrus.New()).WithNamespace("velero")

	req := httptest.NewRequest(http.MethodGet, "/apis/search.velero.io/v2alpha1/resources?kind=Pod", nil)
	req.Header.Set("Authorization", "Bearer tok")
	rr := httptest.NewRecorder()
	srv.auth(srv.searchAll)(rr, req)
	require.Equal(t, http.StatusOK, rr.Code)

	var body searchResponse
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &body))
	assert.Equal(t, 1, body.TotalCount)
	require.Len(t, body.Items, 1)
}

func TestAuthRejectsMissingToken(t *testing.T) {
	srv := New(":0", nil, fake.NewSimpleClientset(), logrus.New())
	req := httptest.NewRequest(http.MethodGet, "/apis/search.velero.io/v2alpha1/resources", nil)
	rr := httptest.NewRecorder()
	srv.auth(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})(rr, req)
	assert.Equal(t, http.StatusUnauthorized, rr.Code)
}
