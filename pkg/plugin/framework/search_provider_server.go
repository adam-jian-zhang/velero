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

	"github.com/pkg/errors"

	"github.com/vmware-tanzu/velero/pkg/plugin/framework/common"
	"github.com/vmware-tanzu/velero/pkg/plugin/generated"
	"github.com/vmware-tanzu/velero/pkg/plugin/velero"
)

type SearchProviderGRPCServer struct {
	mux *common.ServerMux
}

func (s *SearchProviderGRPCServer) getImpl(name string) (velero.SearchProvider, error) {
	impl, err := s.mux.GetHandler(name)
	if err != nil {
		return nil, err
	}
	p, ok := impl.(velero.SearchProvider)
	if !ok {
		return nil, errors.Errorf("%T is not a SearchProvider", impl)
	}
	return p, nil
}

func (s *SearchProviderGRPCServer) Init(ctx context.Context, req *generated.SearchProviderInitRequest) (resp *generated.Empty, err error) {
	defer func() {
		if panicked := recover(); panicked != nil {
			err = common.NewGRPCError(common.HandlePanic(panicked))
		}
	}()
	impl, err := s.getImpl(req.Plugin)
	if err != nil {
		return nil, common.NewGRPCError(err)
	}
	if err := impl.Init(req.Config); err != nil {
		return nil, common.NewGRPCError(err)
	}
	return &generated.Empty{}, nil
}

func (s *SearchProviderGRPCServer) IndexBackup(ctx context.Context, req *generated.IndexBackupRequest) (resp *generated.Empty, err error) {
	defer func() {
		if panicked := recover(); panicked != nil {
			err = common.NewGRPCError(common.HandlePanic(panicked))
		}
	}()
	impl, err := s.getImpl(req.Plugin)
	if err != nil {
		return nil, common.NewGRPCError(err)
	}
	if err := impl.IndexBackup(ctx, req.BackupName, req.TarballUrl); err != nil {
		return nil, common.NewGRPCError(err)
	}
	return &generated.Empty{}, nil
}

func (s *SearchProviderGRPCServer) DeleteBackup(ctx context.Context, req *generated.DeleteBackupRequest) (resp *generated.Empty, err error) {
	defer func() {
		if panicked := recover(); panicked != nil {
			err = common.NewGRPCError(common.HandlePanic(panicked))
		}
	}()
	impl, err := s.getImpl(req.Plugin)
	if err != nil {
		return nil, common.NewGRPCError(err)
	}
	if err := impl.DeleteBackup(ctx, req.BackupName); err != nil {
		return nil, common.NewGRPCError(err)
	}
	return &generated.Empty{}, nil
}

func (s *SearchProviderGRPCServer) Search(ctx context.Context, req *generated.SearchRequest) (resp *generated.SearchResponse, err error) {
	defer func() {
		if panicked := recover(); panicked != nil {
			err = common.NewGRPCError(common.HandlePanic(panicked))
		}
	}()
	impl, err := s.getImpl(req.Plugin)
	if err != nil {
		return nil, common.NewGRPCError(err)
	}
	params := velero.SearchParams{
		Name:       req.Name,
		Namespace:  req.Namespace,
		Kind:       req.Kind,
		APIVersion: req.ApiVersion,
		Labels:     req.Labels,
		BackupName: req.BackupName,
		Limit:      int(req.Limit),
		Offset:     int(req.Offset),
	}
	result, err := impl.Search(ctx, params)
	if err != nil {
		return nil, common.NewGRPCError(err)
	}
	return &generated.SearchResponse{
		Items:      toSearchProviderProtoRecords(result.Records),
		TotalCount: int32(result.TotalCount),
	}, nil
}

func (s *SearchProviderGRPCServer) Ready(ctx context.Context, req *generated.ReadyRequest) (resp *generated.ReadyResponse, err error) {
	defer func() {
		if panicked := recover(); panicked != nil {
			err = common.NewGRPCError(common.HandlePanic(panicked))
		}
	}()
	impl, err := s.getImpl(req.Plugin)
	if err != nil {
		return nil, common.NewGRPCError(err)
	}
	ready, err := impl.Ready(ctx)
	if err != nil {
		return nil, common.NewGRPCError(err)
	}
	return &generated.ReadyResponse{Ready: ready}, nil
}

func (s *SearchProviderGRPCServer) ListIndexedBackups(ctx context.Context, req *generated.ListIndexedBackupsRequest) (resp *generated.ListIndexedBackupsResponse, err error) {
	defer func() {
		if panicked := recover(); panicked != nil {
			err = common.NewGRPCError(common.HandlePanic(panicked))
		}
	}()
	impl, err := s.getImpl(req.Plugin)
	if err != nil {
		return nil, common.NewGRPCError(err)
	}
	names, err := impl.ListIndexedBackups(ctx)
	if err != nil {
		return nil, common.NewGRPCError(err)
	}
	return &generated.ListIndexedBackupsResponse{BackupNames: names}, nil
}

func (s *SearchProviderGRPCServer) MarkReady(ctx context.Context, req *generated.MarkReadyRequest) (resp *generated.Empty, err error) {
	defer func() {
		if panicked := recover(); panicked != nil {
			err = common.NewGRPCError(common.HandlePanic(panicked))
		}
	}()
	impl, err := s.getImpl(req.Plugin)
	if err != nil {
		return nil, common.NewGRPCError(err)
	}
	if err := impl.MarkReady(ctx); err != nil {
		return nil, common.NewGRPCError(err)
	}
	return &generated.Empty{}, nil
}
