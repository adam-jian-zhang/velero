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

package grpc

import (
	"context"
	"testing"

	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/types/known/emptypb"

	"github.com/vmware-tanzu/velero/pkg/plugin/velero"
	veleromocks "github.com/vmware-tanzu/velero/pkg/plugin/velero/mocks"
	searchv1 "github.com/vmware-tanzu/velero/pkg/search/generated"
)

func TestClampLimit(t *testing.T) {
	assert.Equal(t, 100, clampLimit(0))
	assert.Equal(t, 500, clampLimit(999))
	assert.Equal(t, 25, clampLimit(25))
}

func TestSearchAndReady(t *testing.T) {
	sp := veleromocks.NewSearchProvider(t)
	sp.On("Search", mock.Anything, velero.SearchParams{
		Kind: "Pod", Limit: 100,
	}).Return(velero.SearchResult{
		Records:    []velero.ResourceRecord{{BackupName: "b", ResourceName: "p", Kind: "Pod"}},
		TotalCount: 1,
	}, nil)
	sp.On("Ready", mock.Anything).Return(true, nil)

	srv := New(":0", sp, nil, logrus.New())
	res, err := srv.Search(context.Background(), &searchv1.SearchRequest{Kind: "Pod"})
	require.NoError(t, err)
	assert.Equal(t, int32(1), res.TotalCount)

	ready, err := srv.Ready(context.Background(), &emptypb.Empty{})
	require.NoError(t, err)
	assert.True(t, ready.Ready)
}
