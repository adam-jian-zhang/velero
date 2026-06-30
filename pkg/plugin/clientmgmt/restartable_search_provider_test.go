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

package clientmgmt

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/vmware-tanzu/velero/pkg/plugin/clientmgmt/process"
	processmocks "github.com/vmware-tanzu/velero/pkg/plugin/clientmgmt/process/mocks"
	"github.com/vmware-tanzu/velero/pkg/plugin/framework/common"
	veleromocks "github.com/vmware-tanzu/velero/pkg/plugin/velero/mocks"
)

func TestRestartableSearchProviderGetDelegate(t *testing.T) {
	p := new(processmocks.RestartableProcess)
	defer p.AssertExpectations(t)

	name := "velero.io/search-provider"
	key := process.KindAndName{Kind: common.PluginKindSearchProvider, Name: name}

	p.On("AddReinitializer", key, mock.Anything).Return()

	r := NewRestartableSearchProvider(name, p)

	// first call to getDelegate()
	p.On("ResetIfNeeded").Return(nil).Once()
	p.On("GetByKindAndName", key).Return(new(veleromocks.SearchProvider), nil).Once()

	delegate1, err := r.(*restartableSearchProvider).getDelegate()
	require.NoError(t, err)

	// second call to getDelegate()
	p.On("ResetIfNeeded").Return(nil).Once()
	p.On("GetByKindAndName", key).Return(new(veleromocks.SearchProvider), nil).Once()

	delegate2, err := r.(*restartableSearchProvider).getDelegate()
	require.NoError(t, err)

	assert.NotSame(t, delegate1, delegate2)
}
