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
	"fmt"
	"time"

	"github.com/spf13/cobra"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/vmware-tanzu/velero/pkg/apis/velero/v2alpha1"
	"github.com/vmware-tanzu/velero/pkg/client"
	"github.com/vmware-tanzu/velero/pkg/cmd"
)

func NewCommand(f client.Factory) *cobra.Command {
	var (
		name       string
		namespace  string
		kind       string
		apiVersion string
		labels     map[string]string
		backupName string
		limit      int
		ttl        time.Duration
		timeout    time.Duration
	)

	c := &cobra.Command{
		Use:   "search",
		Short: "Search for resources in backups",
		Run: func(c *cobra.Command, args []string) {
			kbClient, err := f.KubebuilderClient()
			cmd.CheckError(err)

			veleroNs := f.Namespace()

			req := &v2alpha1.SearchRequest{
				ObjectMeta: metav1.ObjectMeta{
					GenerateName: "search-",
					Namespace:    veleroNs,
				},
				Spec: v2alpha1.SearchRequestSpec{
					Query: v2alpha1.SearchQuery{
						Name:       name,
						Namespace:  namespace,
						Kind:       kind,
						APIVersion: apiVersion,
						Labels:     labels,
						BackupName: backupName,
					},
					Limit: limit,
					TTL:   metav1.Duration{Duration: ttl},
				},
			}

			err = kbClient.Create(context.Background(), req)
			cmd.CheckError(err)

			fmt.Printf("SearchRequest %s created, waiting for results...\n", req.Name)

			var result *v2alpha1.SearchRequest
			err = wait.PollUntilContextTimeout(context.Background(), 500*time.Millisecond, timeout, true, func(ctx context.Context) (bool, error) {
				var sr v2alpha1.SearchRequest
				if err := kbClient.Get(ctx, ctrlclient.ObjectKeyFromObject(req), &sr); err != nil {
					return false, err
				}
				if sr.Status.Phase == v2alpha1.SearchRequestPhaseProcessed || sr.Status.Phase == v2alpha1.SearchRequestPhaseFailed {
					result = &sr
					return true, nil
				}
				return false, nil
			})
			cmd.CheckError(err)

			if result.Status.Phase == v2alpha1.SearchRequestPhaseFailed {
				cmd.CheckError(fmt.Errorf("search failed: %s", result.Status.Message))
			}

			fmt.Printf("\nBACKUP\tNAMESPACE\tKIND\tNAME\tAPIVERSION\n")
			for _, r := range result.Status.Results {
				fmt.Printf("%s\t%s\t%s\t%s\t%s\n", r.BackupName, r.Namespace, r.Kind, r.ResourceName, r.APIVersion)
			}
			if result.Status.TotalCount > len(result.Status.Results) {
				fmt.Printf("\n(Showing %d of %d matches)\n", len(result.Status.Results), result.Status.TotalCount)
			}
			if result.Status.Message != "" {
				fmt.Printf("\nNote: %s\n", result.Status.Message)
			}
		},
	}

	c.Flags().StringVar(&name, "name", "", "Resource name (supports glob * ?)")
	c.Flags().StringVar(&namespace, "namespace", "", "Resource namespace")
	c.Flags().StringVar(&kind, "kind", "", "Resource kind")
	c.Flags().StringVar(&apiVersion, "api-version", "", "Resource API version")
	c.Flags().StringToStringVar(&labels, "label", nil, "Resource labels (key=value)")
	c.Flags().StringVar(&backupName, "backup-name", "", "Backup name")
	c.Flags().IntVar(&limit, "limit", 100, "Max results")
	c.Flags().DurationVar(&ttl, "ttl", 10*time.Minute, "Time to keep the SearchRequest")
	c.Flags().DurationVar(&timeout, "timeout", 30*time.Second, "Timeout waiting for results")

	return c
}
