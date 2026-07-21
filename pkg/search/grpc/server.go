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
	"strings"

	"github.com/sirupsen/logrus"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/emptypb"
	authv1 "k8s.io/api/authentication/v1"
	authzv1 "k8s.io/api/authorization/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/vmware-tanzu/velero/pkg/plugin/velero"
	searchv1 "github.com/vmware-tanzu/velero/pkg/search/generated"
)

const (
	defaultLimit = 100
	maxLimit     = 500
)

type Server struct {
	addr           string
	namespace      string
	searchProvider velero.SearchProvider
	kubeClient     kubernetes.Interface
	logger         logrus.FieldLogger
	searchv1.UnimplementedSearchServiceServer
}

func New(addr string, sp velero.SearchProvider, kubeClient kubernetes.Interface, logger logrus.FieldLogger) *Server {
	return &Server{
		addr:           addr,
		namespace:      "velero",
		searchProvider: sp,
		kubeClient:     kubeClient,
		logger:         logger,
	}
}

func (s *Server) WithNamespace(ns string) *Server {
	if ns != "" {
		s.namespace = ns
	}
	return s
}

func (s *Server) Start(ctx context.Context) error {
	lc := net.ListenConfig{}
	lis, err := lc.Listen(ctx, "tcp", s.addr)
	if err != nil {
		return err
	}

	srv := grpc.NewServer(grpc.UnaryInterceptor(s.authUnary), grpc.StreamInterceptor(s.authStream))
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

func (s *Server) authUnary(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
	if info.FullMethod == "/velero.search.v1.SearchService/Ready" {
		return handler(ctx, req)
	}
	if err := s.authorize(ctx); err != nil {
		return nil, err
	}
	return handler(ctx, req)
}

func (s *Server) authStream(srv any, ss grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
	if err := s.authorize(ss.Context()); err != nil {
		return err
	}
	return handler(srv, ss)
}

func (s *Server) authorize(ctx context.Context) error {
	if s.kubeClient == nil {
		return status.Error(codes.Unauthenticated, "kube client not configured")
	}
	md, ok := metadata.FromIncomingContext(ctx)
	if !ok {
		return status.Error(codes.Unauthenticated, "missing metadata")
	}
	values := md.Get("authorization")
	if len(values) == 0 {
		return status.Error(codes.Unauthenticated, "missing bearer token")
	}
	token := strings.TrimSpace(strings.TrimPrefix(values[0], "Bearer "))
	if token == "" {
		return status.Error(codes.Unauthenticated, "empty bearer token")
	}

	tr, err := s.kubeClient.AuthenticationV1().TokenReviews().Create(ctx, &authv1.TokenReview{
		Spec: authv1.TokenReviewSpec{Token: token},
	}, metav1.CreateOptions{})
	if err != nil || !tr.Status.Authenticated {
		return status.Error(codes.Unauthenticated, "token not authenticated")
	}

	sar, err := s.kubeClient.AuthorizationV1().SubjectAccessReviews().Create(ctx, &authzv1.SubjectAccessReview{
		Spec: authzv1.SubjectAccessReviewSpec{
			User:   tr.Status.User.Username,
			Groups: tr.Status.User.Groups,
			UID:    tr.Status.User.UID,
			Extra:  toExtra(tr.Status.User.Extra),
			ResourceAttributes: &authzv1.ResourceAttributes{
				Namespace: s.namespace,
				Verb:      "create",
				Group:     "velero.io",
				Resource:  "searchrequests",
			},
		},
	}, metav1.CreateOptions{})
	if err != nil || !sar.Status.Allowed {
		return status.Error(codes.PermissionDenied, "forbidden")
	}
	return nil
}

func toExtra(in map[string]authv1.ExtraValue) map[string]authzv1.ExtraValue {
	if in == nil {
		return nil
	}
	out := make(map[string]authzv1.ExtraValue, len(in))
	for k, v := range in {
		out[k] = authzv1.ExtraValue(v)
	}
	return out
}

func clampLimit(limit int32) int {
	n := int(limit)
	if n <= 0 {
		return defaultLimit
	}
	if n > maxLimit {
		return maxLimit
	}
	return n
}

func (s *Server) Search(ctx context.Context, req *searchv1.SearchRequest) (*searchv1.SearchResponse, error) {
	params := velero.SearchParams{
		Name:       strings.TrimSpace(req.Name),
		Namespace:  strings.TrimSpace(req.Namespace),
		Kind:       strings.TrimSpace(req.Kind),
		APIVersion: strings.TrimSpace(req.ApiVersion),
		Labels:     req.Labels,
		BackupName: strings.TrimSpace(req.BackupName),
		Limit:      clampLimit(req.Limit),
		Offset:     int(req.Offset),
	}
	if params.Offset < 0 {
		params.Offset = 0
	}

	res, err := s.searchProvider.Search(ctx, params)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "search failed: %v", err)
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
	// Page through results using limit/offset rather than loading unbounded sets.
	limit := clampLimit(req.Limit)
	offset := int(req.Offset)
	if offset < 0 {
		offset = 0
	}
	for {
		pageReq := *req
		pageReq.Limit = int32(limit)
		pageReq.Offset = int32(offset)
		res, err := s.Search(stream.Context(), &pageReq)
		if err != nil {
			return err
		}
		for _, item := range res.Items {
			if err := stream.Send(item); err != nil {
				return err
			}
		}
		offset += len(res.Items)
		if len(res.Items) < limit || offset >= int(res.TotalCount) {
			return nil
		}
	}
}

func (s *Server) Ready(ctx context.Context, req *emptypb.Empty) (*searchv1.ReadyResponse, error) {
	ready, err := s.searchProvider.Ready(ctx)
	if err != nil {
		return nil, err
	}
	return &searchv1.ReadyResponse{Ready: ready}, nil
}
