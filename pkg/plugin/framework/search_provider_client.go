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

package framework

import (
	"context"

	"google.golang.org/grpc"

	"github.com/vmware-tanzu/velero/pkg/plugin/framework/common"
	"github.com/vmware-tanzu/velero/pkg/plugin/generated"
	"github.com/vmware-tanzu/velero/pkg/plugin/velero"
)

type SearchProviderGRPCClient struct {
	*common.ClientBase
	grpcClient generated.SearchProviderClient
}

func newSearchProviderGRPCClient(base *common.ClientBase, conn *grpc.ClientConn) any {
	return &SearchProviderGRPCClient{
		ClientBase: base,
		grpcClient: generated.NewSearchProviderClient(conn),
	}
}

func (c *SearchProviderGRPCClient) Init(config map[string]string) error {
	_, err := c.grpcClient.Init(context.Background(), &generated.SearchProviderInitRequest{
		Plugin: c.Plugin, Config: config,
	})
	return common.FromGRPCError(err)
}

func (c *SearchProviderGRPCClient) IndexBackup(ctx context.Context, backupName, tarballURL string) error {
	_, err := c.grpcClient.IndexBackup(ctx, &generated.IndexBackupRequest{
		Plugin: c.Plugin, BackupName: backupName, TarballUrl: tarballURL,
	})
	return common.FromGRPCError(err)
}

func (c *SearchProviderGRPCClient) DeleteBackup(ctx context.Context, backupName string) error {
	_, err := c.grpcClient.DeleteBackup(ctx, &generated.DeleteBackupRequest{
		Plugin: c.Plugin, BackupName: backupName,
	})
	return common.FromGRPCError(err)
}

func (c *SearchProviderGRPCClient) Search(ctx context.Context, params velero.SearchParams) (velero.SearchResult, error) {
	resp, err := c.grpcClient.Search(ctx, &generated.SearchRequest{
		Plugin:     c.Plugin,
		Name:       params.Name,
		Namespace:  params.Namespace,
		Kind:       params.Kind,
		ApiVersion: params.APIVersion,
		Labels:     params.Labels,
		BackupName: params.BackupName,
		Limit:      int32(params.Limit),
		Offset:     int32(params.Offset),
	})
	if err != nil {
		return velero.SearchResult{}, common.FromGRPCError(err)
	}
	return velero.SearchResult{
		Records:    fromSearchProviderProtoRecords(resp.Items),
		TotalCount: int(resp.TotalCount),
	}, nil
}

func (c *SearchProviderGRPCClient) Ready(ctx context.Context) (bool, error) {
	resp, err := c.grpcClient.Ready(ctx, &generated.ReadyRequest{
		Plugin: c.Plugin,
	})
	if err != nil {
		return false, common.FromGRPCError(err)
	}
	return resp.Ready, nil
}

func (c *SearchProviderGRPCClient) ListIndexedBackups(ctx context.Context) ([]string, error) {
	resp, err := c.grpcClient.ListIndexedBackups(ctx, &generated.ListIndexedBackupsRequest{
		Plugin: c.Plugin,
	})
	if err != nil {
		return nil, common.FromGRPCError(err)
	}
	return resp.BackupNames, nil
}

func (c *SearchProviderGRPCClient) MarkReady(ctx context.Context) error {
	_, err := c.grpcClient.MarkReady(ctx, &generated.MarkReadyRequest{
		Plugin: c.Plugin,
	})
	return common.FromGRPCError(err)
}

func fromSearchProviderProtoRecords(protoRecords []*generated.ResourceRecord) []velero.ResourceRecord {
	if protoRecords == nil {
		return nil
	}
	records := make([]velero.ResourceRecord, 0, len(protoRecords))
	for _, pr := range protoRecords {
		records = append(records, velero.ResourceRecord{
			BackupName:   pr.BackupName,
			ResourceName: pr.ResourceName,
			APIVersion:   pr.ApiVersion,
			Kind:         pr.Kind,
			Namespace:    pr.Namespace,
			Labels:       pr.Labels,
		})
	}
	return records
}

func toSearchProviderProtoRecords(records []velero.ResourceRecord) []*generated.ResourceRecord {
	if records == nil {
		return nil
	}
	protoRecords := make([]*generated.ResourceRecord, 0, len(records))
	for _, r := range records {
		protoRecords = append(protoRecords, &generated.ResourceRecord{
			BackupName:   r.BackupName,
			ResourceName: r.ResourceName,
			ApiVersion:   r.APIVersion,
			Kind:         r.Kind,
			Namespace:    r.Namespace,
			Labels:       r.Labels,
		})
	}
	return protoRecords
}
