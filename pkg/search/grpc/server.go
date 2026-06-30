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
	"net"

	"github.com/sirupsen/logrus"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/types/known/emptypb"
	"k8s.io/client-go/kubernetes"

	"github.com/vmware-tanzu/velero/pkg/plugin/velero"
	searchv1 "github.com/vmware-tanzu/velero/pkg/search/generated"
)

type Server struct {
	addr           string
	searchProvider velero.SearchProvider
	kubeClient     kubernetes.Interface
	logger         logrus.FieldLogger
	searchv1.UnimplementedSearchServiceServer
}

func New(addr string, sp velero.SearchProvider, kubeClient kubernetes.Interface, logger logrus.FieldLogger) *Server {
	return &Server{
		addr:           addr,
		searchProvider: sp,
		kubeClient:     kubeClient,
		logger:         logger,
	}
}

func (s *Server) Start(ctx context.Context) error {
	lc := net.ListenConfig{}
	lis, err := lc.Listen(ctx, "tcp", s.addr)
	if err != nil {
		return err
	}

	srv := grpc.NewServer()
	searchv1.RegisterSearchServiceServer(srv, s)

	go func() {
		if err := srv.Serve(lis); err != nil {
			s.logger.WithError(err).Error("gRPC server error")
		}
	}()

	<-ctx.Done()
	srv.GracefulStop()
	return nil
}

func (s *Server) Search(ctx context.Context, req *searchv1.SearchRequest) (*searchv1.SearchResponse, error) {
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

	res, err := s.searchProvider.Search(ctx, params)
	if err != nil {
		return nil, err
	}

	protoItems := make([]*searchv1.ResourceRecord, len(res.Records))
	for i, r := range res.Records {
		protoItems[i] = &searchv1.ResourceRecord{
			BackupName:   r.BackupName,
			ResourceName: r.ResourceName,
			ApiVersion:   r.APIVersion,
			Kind:         r.Kind,
			Namespace:    r.Namespace,
			Labels:       r.Labels,
		}
	}

	return &searchv1.SearchResponse{
		Items:      protoItems,
		TotalCount: int32(res.TotalCount),
	}, nil
}

func (s *Server) SearchStream(req *searchv1.SearchRequest, stream searchv1.SearchService_SearchStreamServer) error {
	// naive stream implementation
	res, err := s.Search(stream.Context(), req)
	if err != nil {
		return err
	}
	for _, item := range res.Items {
		if err := stream.Send(item); err != nil {
			return err
		}
	}
	return nil
}

func (s *Server) Ready(ctx context.Context, req *emptypb.Empty) (*searchv1.ReadyResponse, error) {
	ready, err := s.searchProvider.Ready(ctx)
	if err != nil {
		return nil, err
	}
	return &searchv1.ReadyResponse{Ready: ready}, nil
}
