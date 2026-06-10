// Package controlplane is the orchestration/state HTTP service. It exposes a
// REST API for inventory and replication jobs, ingests per-sync reports from
// agents, computes RPO/lag, serves a small dashboard, and exports Prometheus
// metrics.
package controlplane

import (
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/tiny125/vm-replication/internal/api"
	"github.com/tiny125/vm-replication/internal/store"
)

// Server wires the store to an HTTP handler.
type Server struct {
	st    *store.Store
	token string // bearer token required on API + metrics (empty = open, dev only)
	mux   *http.ServeMux
}

// New builds a control plane server. If token is non-empty, all /api and
// /metrics requests must present "Authorization: Bearer <token>".
func New(st *store.Store, token string) *Server {
	s := &Server{st: st, token: token, mux: http.NewServeMux()}
	s.routes()
	return s
}

// Handler returns the root HTTP handler (with request logging).
func (s *Server) Handler() http.Handler { return logging(s.mux) }

func (s *Server) routes() {
	// Public: dashboard shell + health. The dashboard JS supplies the token.
	s.mux.HandleFunc("GET /", s.handleDashboard)
	s.mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("ok")) })

	// Authenticated API.
	s.mux.Handle("POST /api/v1/servers", s.auth(s.handleRegisterServer))
	s.mux.Handle("GET /api/v1/servers", s.auth(s.handleListServers))
	s.mux.Handle("POST /api/v1/jobs", s.auth(s.handleCreateJob))
	s.mux.Handle("GET /api/v1/jobs", s.auth(s.handleListJobs))
	s.mux.Handle("GET /api/v1/jobs/{id}", s.auth(s.handleGetJob))
	s.mux.Handle("POST /api/v1/jobs/{id}/state", s.auth(s.handleSetState))
	s.mux.Handle("POST /api/v1/jobs/{id}/syncs", s.auth(s.handleReportSync))
	s.mux.Handle("GET /api/v1/jobs/{id}/syncs", s.auth(s.handleListSyncs))
	s.mux.Handle("GET /api/v1/status", s.auth(s.handleStatus))
	s.mux.Handle("GET /metrics", s.auth(s.handleMetrics))
}

// auth wraps a handler with bearer-token verification.
func (s *Server) auth(h http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.token != "" {
			got := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
			if subtleCompare(got, s.token) != 1 {
				writeErr(w, http.StatusUnauthorized, "invalid or missing bearer token")
				return
			}
		}
		h(w, r)
	})
}

func logging(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		next.ServeHTTP(w, r)
		log.Printf("%s %s %s", r.Method, r.URL.Path, time.Since(start).Round(time.Millisecond))
	})
}

// ---- handlers ----

func (s *Server) handleRegisterServer(w http.ResponseWriter, r *http.Request) {
	var req api.RegisterServerRequest
	if !readJSON(w, r, &req) {
		return
	}
	if req.Name == "" || req.Role == "" {
		writeErr(w, http.StatusBadRequest, "name and role are required")
		return
	}
	sv, err := s.st.UpsertServer(r.Context(), req)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, sv)
}

func (s *Server) handleListServers(w http.ResponseWriter, r *http.Request) {
	servers, err := s.st.ListServers(r.Context())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, servers)
}

func (s *Server) handleCreateJob(w http.ResponseWriter, r *http.Request) {
	var req api.CreateJobRequest
	if !readJSON(w, r, &req) {
		return
	}
	if req.Name == "" {
		writeErr(w, http.StatusBadRequest, "name is required")
		return
	}
	job, err := s.st.CreateJob(r.Context(), req)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, job)
}

func (s *Server) handleListJobs(w http.ResponseWriter, r *http.Request) {
	jobs, err := s.st.ListJobs(r.Context())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, jobs)
}

func (s *Server) handleGetJob(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	job, err := s.st.Job(r.Context(), id)
	if handleStoreErr(w, err) {
		return
	}
	st, err := s.st.JobStatus(r.Context(), job, time.Now())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, st)
}

func (s *Server) handleSetState(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	var req api.SetStateRequest
	if !readJSON(w, r, &req) {
		return
	}
	if !req.State.Valid() {
		writeErr(w, http.StatusBadRequest, "invalid job state")
		return
	}
	if err := s.st.SetJobState(r.Context(), id, req.State); handleStoreErr(w, err) {
		return
	}
	job, _ := s.st.Job(r.Context(), id)
	writeJSON(w, http.StatusOK, job)
}

func (s *Server) handleReportSync(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	var req api.ReportSyncRequest
	if !readJSON(w, r, &req) {
		return
	}
	if !req.Mode.Valid() {
		writeErr(w, http.StatusBadRequest, "invalid sync mode")
		return
	}
	sy, err := s.st.AddSync(r.Context(), id, req)
	if handleStoreErr(w, err) {
		return
	}
	writeJSON(w, http.StatusCreated, sy)
}

func (s *Server) handleListSyncs(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	syncs, err := s.st.ListSyncs(r.Context(), id, limit)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, syncs)
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	statuses, err := s.st.AllJobStatuses(r.Context())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, statuses)
}

// ---- helpers ----

func pathID(w http.ResponseWriter, r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "invalid id")
		return 0, false
	}
	return id, true
}

func handleStoreErr(w http.ResponseWriter, err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, store.ErrNotFound) {
		writeErr(w, http.StatusNotFound, "not found")
	} else {
		writeErr(w, http.StatusInternalServerError, err.Error())
	}
	return true
}

func readJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	defer r.Body.Close()
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return false
	}
	return true
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}

// subtleCompare is a constant-time string compare returning 1 on equal.
func subtleCompare(a, b string) int {
	if len(a) != len(b) {
		return 0
	}
	var v byte
	for i := 0; i < len(a); i++ {
		v |= a[i] ^ b[i]
	}
	if v == 0 {
		return 1
	}
	return 0
}
