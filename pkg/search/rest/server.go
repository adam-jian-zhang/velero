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
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/sirupsen/logrus"
	authv1 "k8s.io/api/authentication/v1"
	authzv1 "k8s.io/api/authorization/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/vmware-tanzu/velero/pkg/plugin/velero"
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

// WithNamespace sets the Velero namespace used for SubjectAccessReview checks.
func (s *Server) WithNamespace(ns string) *Server {
	if ns != "" {
		s.namespace = ns
	}
	return s
}

func (s *Server) Start(ctx context.Context) error {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", s.healthz)
	mux.HandleFunc("/readyz", s.readyz)
	mux.HandleFunc("/apis/search.velero.io/v2alpha1/resources", s.auth(s.searchAll))
	mux.HandleFunc("/apis/search.velero.io/v2alpha1/backups/", s.auth(s.searchInBackup))

	srv := &http.Server{
		Addr:              s.addr,
		Handler:           mux,
		ReadHeaderTimeout: 3 * time.Second,
	}

	go func() {
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			s.logger.WithError(err).Error("REST server error")
		}
	}()

	<-ctx.Done()
	return srv.Shutdown(context.Background())
}

func (s *Server) healthz(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok"))
}

func (s *Server) readyz(w http.ResponseWriter, r *http.Request) {
	ready, err := s.searchProvider.Ready(r.Context())
	if err != nil || !ready {
		w.WriteHeader(http.StatusServiceUnavailable)
		return
	}
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok"))
}

func (s *Server) auth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if err := s.authorize(r); err != nil {
			http.Error(w, err.Error(), http.StatusUnauthorized)
			return
		}
		next(w, r)
	}
}

func (s *Server) authorize(r *http.Request) error {
	if s.kubeClient == nil {
		return errUnauthorized("kube client not configured")
	}
	authzHeader := r.Header.Get("Authorization")
	if !strings.HasPrefix(authzHeader, "Bearer ") {
		return errUnauthorized("missing bearer token")
	}
	token := strings.TrimSpace(strings.TrimPrefix(authzHeader, "Bearer "))
	if token == "" {
		return errUnauthorized("empty bearer token")
	}

	tr, err := s.kubeClient.AuthenticationV1().TokenReviews().Create(r.Context(), &authv1.TokenReview{
		Spec: authv1.TokenReviewSpec{Token: token},
	}, metav1.CreateOptions{})
	if err != nil {
		return errUnauthorized("token review failed")
	}
	if !tr.Status.Authenticated {
		return errUnauthorized("token not authenticated")
	}

	sar, err := s.kubeClient.AuthorizationV1().SubjectAccessReviews().Create(r.Context(), &authzv1.SubjectAccessReview{
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
	if err != nil {
		return errUnauthorized("access review failed")
	}
	if !sar.Status.Allowed {
		return errUnauthorized("forbidden")
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

type unauthorizedError string

func (e unauthorizedError) Error() string { return string(e) }

func errUnauthorized(msg string) error { return unauthorizedError(msg) }

func (s *Server) searchAll(w http.ResponseWriter, r *http.Request) {
	s.handleSearch(w, r, "")
}

func (s *Server) searchInBackup(w http.ResponseWriter, r *http.Request) {
	parts := strings.Split(r.URL.Path, "/")
	// /apis/search.velero.io/v2alpha1/backups/{name}/resources
	if len(parts) < 7 || parts[5] == "" || parts[6] != "resources" {
		http.Error(w, "expected /apis/search.velero.io/v2alpha1/backups/{name}/resources", http.StatusBadRequest)
		return
	}
	s.handleSearch(w, r, parts[5])
}

type searchResponse struct {
	Items      []velero.ResourceRecord `json:"items"`
	TotalCount int                     `json:"totalCount"`
}

func (s *Server) handleSearch(w http.ResponseWriter, r *http.Request, backupName string) {
	params, err := parseSearchParams(r, backupName)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	res, err := s.searchProvider.Search(ctx, params)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(searchResponse{
		Items:      res.Records,
		TotalCount: res.TotalCount,
	}); err != nil {
		s.logger.WithError(err).Error("Failed to encode response")
	}
}

func parseSearchParams(r *http.Request, backupName string) (velero.SearchParams, error) {
	q := r.URL.Query()
	params := velero.SearchParams{
		Name:       strings.TrimSpace(q.Get("name")),
		Namespace:  strings.TrimSpace(q.Get("namespace")),
		Kind:       strings.TrimSpace(q.Get("kind")),
		APIVersion: strings.TrimSpace(q.Get("apiVersion")),
		BackupName: backupName,
		Labels:     map[string]string{},
		Limit:      defaultLimit,
	}

	for _, raw := range q["label"] {
		k, v, ok := strings.Cut(raw, "=")
		if !ok || strings.TrimSpace(k) == "" {
			return params, fmt.Errorf("invalid label filter, expected key=value")
		}
		params.Labels[strings.TrimSpace(k)] = strings.TrimSpace(v)
	}

	if lim := q.Get("limit"); lim != "" {
		n, err := strconv.Atoi(lim)
		if err != nil || n < 1 {
			return params, fmt.Errorf("invalid limit")
		}
		if n > maxLimit {
			n = maxLimit
		}
		params.Limit = n
	}
	if off := q.Get("offset"); off != "" {
		n, err := strconv.Atoi(off)
		if err != nil || n < 0 {
			return params, fmt.Errorf("invalid offset")
		}
		params.Offset = n
	}
	return params, nil
}
