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

package search

import (
	"context"
	"testing"
	"time"

	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNewBuiltInSearchProvider(t *testing.T) {
	opts := Options{
		Driver:       "sqlite",
		DSN:          ":memory:",
		MaxWorkers:   1,
		QueryTimeout: time.Second,
		Logger:       logrus.New(),
	}
	p := NewBuiltInSearchProvider(opts)
	assert.NotNil(t, p)

	err := p.Init(map[string]string{
		"driver": "sqlite",
		"dsn":    ":memory:",
	})
	require.NoError(t, err)

	ready, err := p.Ready(context.Background())
	require.NoError(t, err)
	assert.True(t, ready) // Since we updated Init to immediately mark ready
}
