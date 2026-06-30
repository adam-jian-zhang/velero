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
	"net/http"
	"strings"
	"time"

	"github.com/sirupsen/logrus"
	"k8s.io/client-go/kubernetes"

	"github.com/vmware-tanzu/velero/pkg/plugin/velero"
)

type Server struct {
	addr           string
	searchProvider velero.SearchProvider
	kubeClient     kubernetes.Interface
	logger         logrus.FieldLogger
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
		// Mock auth for now: just pass through.
		// In a real implementation, we'd use s.kubeClient to create a TokenReview
		next(w, r)
	}
}

func (s *Server) searchAll(w http.ResponseWriter, r *http.Request) {
	s.handleSearch(w, r, "")
}

func (s *Server) searchInBackup(w http.ResponseWriter, r *http.Request) {
	parts := strings.Split(r.URL.Path, "/")
	// /apis/search.velero.io/v2alpha1/backups/{name}/resources
	if len(parts) < 6 || parts[5] == "" {
		http.Error(w, "missing backup name", http.StatusBadRequest)
		return
	}
	backupName := parts[5]
	s.handleSearch(w, r, backupName)
}

func (s *Server) handleSearch(w http.ResponseWriter, r *http.Request, backupName string) {
	params := velero.SearchParams{
		Name:       r.URL.Query().Get("name"),
		Namespace:  r.URL.Query().Get("namespace"),
		Kind:       r.URL.Query().Get("kind"),
		APIVersion: r.URL.Query().Get("apiVersion"),
		BackupName: backupName,
		Limit:      100, // naive parsing for limits/offsets skipped for brevity
	}

	res, err := s.searchProvider.Search(r.Context(), params)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(res); err != nil {
		s.logger.WithError(err).Error("Failed to encode response")
	}
}
