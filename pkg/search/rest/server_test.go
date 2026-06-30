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
	"context"
	"testing"
	"time"

	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/assert"
)

func TestRESTServer(t *testing.T) {
	srv := New(":0", nil, nil, logrus.New())
	assert.NotNil(t, srv)

	// Since we are mocking/stubbing this, we will just start and stop it
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go func() {
		// Mock start
		err := srv.Start(ctx)
		assert.NoError(t, err)
	}()

	time.Sleep(100 * time.Millisecond)
	cancel()
}
