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

package plugin

import (
	"time"

	"github.com/sirupsen/logrus"

	"github.com/vmware-tanzu/velero/pkg/cmd/server/plugin/search"
)

func newBuiltinSearchProvider(logger logrus.FieldLogger) (any, error) {
	return search.NewBuiltInSearchProvider(search.Options{
		Driver:       "sqlite", // overridden by config at Init
		DSN:          "/var/lib/velero/search.db",
		MaxWorkers:   10,
		QueryTimeout: 5 * time.Second,
		Logger:       logger,
	}), nil
}
