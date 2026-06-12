// Package appliance is the turnkey, AWS MGN–style control plane: a single
// service you install on a "replication server" that presents a password-
// protected web console, enrolls source agents with a copy-paste command,
// embeds a receiver per migration, tracks replication status, and finalizes a
// migration into a launchable Linode artifact.
package appliance

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"log"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/tiny125/vm-replication/internal/api"
	"github.com/tiny125/vm-replication/internal/linode"
	"github.com/tiny125/vm-replication/internal/store"
	"github.com/tiny125/vm-replication/internal/transport"
)

// Config configures the appliance server.
type Config struct {
	Store             *store.Store
	DataDir           string          // file-fallback volumes + manifests live here
	PublicHost        string          // IP/DNS that source agents reach this server at
	ConsolePort       int             // for building enrollment URLs
	Scheme            string          // "https" (default) or "http" for enrollment URLs
	PublicKeyPin      string          // base64 SPKI SHA-256 for curl --pinnedpubkey (empty = no pin)
	BaseReceiverPort  int             // first port for per-migration receivers
	Region            string          // default Linode region for volumes/instances
	TLS               transport.Files // appliance data-plane (receiver) cert/key/ca
	AgentCert         string          // agent.crt handed to sources during enrollment
	AgentKey          string          // agent.key handed to sources
	CACert            string          // ca.crt handed to sources
	AgentBinary       string          // path to the linux/amd64 agent binary to serve
	ApplianceLinodeID int64           // this server's Linode id (0 = file fallback)
	RPOTargetSec      int             // lag threshold for the "ready to migrate" gate
	ConvertScript     string          // path to machine-convert.sh (empty = skip convert)
}

// Server is the appliance HTTP service.
type Server struct {
	cfg Config
	st  *store.Store
	mux *http.ServeMux

	sessions sync.Map // sessionID -> expiry (time.Time)

	recMu     sync.Mutex
	receivers map[int64]context.CancelFunc // migrationID -> stop receiver
	finalizes map[int64]context.CancelFunc // migrationID -> cancel finalize run
	progress  sync.Map                     // migrationID -> *syncProgress
	ctx       context.Context
}

// syncProgress is live block-apply progress for the console (percent + ETA).
type syncProgress struct {
	mu       sync.Mutex
	written  int64
	total    int64
	fullSync bool
	started  time.Time
}

// New builds the appliance server. ctx governs the lifetime of embedded
// receivers.
func New(ctx context.Context, cfg Config) *Server {
	if cfg.BaseReceiverPort == 0 {
		cfg.BaseReceiverPort = 5000
	}
	if cfg.RPOTargetSec == 0 {
		cfg.RPOTargetSec = 120
	}
	s := &Server{
		cfg: cfg, st: cfg.Store, mux: http.NewServeMux(),
		receivers: map[int64]context.CancelFunc{},
		finalizes: map[int64]context.CancelFunc{},
		ctx:       ctx,
	}
	s.routes()
	return s
}

func (s *Server) routes() {
	// Public: console shell, health, auth.
	s.mux.HandleFunc("GET /", s.handleConsole)
	s.mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte("ok")) })
	s.mux.HandleFunc("POST /login", s.handleLogin)
	s.mux.HandleFunc("POST /logout", s.handleLogout)

	// Enrollment (authenticated by per-migration token, not the session).
	s.mux.HandleFunc("GET /install/agent.sh", s.handleAgentInstaller)
	s.mux.HandleFunc("GET /install/uninstall.sh", s.handleUninstallScript)
	s.mux.HandleFunc("GET /enroll/file", s.handleEnrollFile)
	s.mux.HandleFunc("GET /download/agent", s.handleDownloadAgent)

	// Console API (session-protected).
	s.mux.Handle("GET /api/v1/session", s.auth(s.handleSession))
	s.mux.Handle("GET /api/v1/migrations", s.auth(s.handleListMigrations))
	s.mux.Handle("POST /api/v1/migrations", s.auth(s.handleCreateMigration))
	s.mux.Handle("GET /api/v1/migrations/{id}", s.auth(s.handleGetMigration))
	s.mux.Handle("GET /api/v1/migrations/{id}/events", s.auth(s.handleMigrationEvents))
	s.mux.Handle("DELETE /api/v1/migrations/{id}", s.auth(s.handleDeleteMigration))
	s.mux.Handle("POST /api/v1/migrations/{id}/assess", s.auth(s.handleAssessMigration))
	s.mux.Handle("POST /api/v1/migrations/{id}/start", s.auth(s.handleStartMigration))
	s.mux.Handle("POST /api/v1/migrations/{id}/stop", s.auth(s.handleStopMigration))
	s.mux.Handle("POST /api/v1/diagnostics/connection", s.auth(s.handleConnTest))
	s.mux.Handle("GET /api/v1/settings", s.auth(s.handleGetSettings))
	s.mux.Handle("POST /api/v1/settings/linode-token", s.auth(s.handleSetLinodeToken))
	s.mux.Handle("DELETE /api/v1/settings/linode-token", s.auth(s.handleDeleteLinodeToken))
}

// Handler returns the root HTTP handler.
func (s *Server) Handler() http.Handler { return s.mux }

// ---- sessions / auth ----

const sessionCookie = "vmrepl_session"
const sessionTTL = 12 * time.Hour

func (s *Server) newSession() string {
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	id := hex.EncodeToString(b)
	s.sessions.Store(id, time.Now().Add(sessionTTL))
	return id
}

func (s *Server) validSession(id string) bool {
	v, ok := s.sessions.Load(id)
	if !ok {
		return false
	}
	exp := v.(time.Time)
	if time.Now().After(exp) {
		s.sessions.Delete(id)
		return false
	}
	return true
}

func (s *Server) auth(h http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := r.Cookie(sessionCookie)
		if err != nil || !s.validSession(c.Value) {
			writeErr(w, http.StatusUnauthorized, "not logged in")
			return
		}
		h(w, r)
	})
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	var req api.LoginRequest
	if !readJSON(w, r, &req) {
		return
	}
	ok, err := s.st.VerifyAdminPassword(r.Context(), req.Password)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if !ok {
		// Constant-ish failure; avoid leaking timing on the lookup path.
		time.Sleep(300 * time.Millisecond)
		writeErr(w, http.StatusUnauthorized, "invalid password")
		return
	}
	id := s.newSession()
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookie, Value: id, Path: "/", HttpOnly: true, SameSite: http.SameSiteLaxMode,
		MaxAge: int(sessionTTL.Seconds()),
	})
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(sessionCookie); err == nil {
		s.sessions.Delete(c.Value)
	}
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: "", Path: "/", MaxAge: -1})
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) handleSession(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// ---- settings ----

func (s *Server) handleGetSettings(w http.ResponseWriter, r *http.Request) {
	linodeSet, _ := s.st.LinodeTokenSet(r.Context())
	writeJSON(w, http.StatusOK, map[string]any{
		"linode_token_set":    linodeSet,
		"appliance_linode_id": s.cfg.ApplianceLinodeID,
		"public_host":         s.cfg.PublicHost,
		"region":              s.cfg.Region,
		"linode_automation":   s.cfg.ApplianceLinodeID != 0,
	})
}

func (s *Server) handleSetLinodeToken(w http.ResponseWriter, r *http.Request) {
	var req api.SetLinodeTokenRequest
	if !readJSON(w, r, &req) {
		return
	}
	if req.Token == "" {
		writeErr(w, http.StatusBadRequest, "token is required")
		return
	}
	if err := s.st.SetLinodeToken(r.Context(), req.Token); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// handleDeleteLinodeToken removes the stored token (e.g. expired/rotated).
func (s *Server) handleDeleteLinodeToken(w http.ResponseWriter, r *http.Request) {
	if err := s.st.DeleteLinodeToken(r.Context()); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// ---- helpers ----

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

func pathID(w http.ResponseWriter, r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "invalid id")
		return 0, false
	}
	return id, true
}

// linodeClient returns a Linode client if a token is stored, else (nil,false).
func (s *Server) linodeClient(ctx context.Context) (*linode.Client, bool) {
	tok, err := s.st.LinodeToken(ctx)
	if err != nil || tok == "" {
		return nil, false
	}
	return linode.New(tok), true
}

// StartActiveReceivers (re)starts receivers for all migrations that are mid-
// replication, e.g. after the appliance restarts.
func (s *Server) StartActiveReceivers() {
	migs, err := s.st.ListMigrations(s.ctx)
	if err != nil {
		log.Printf("appliance: list migrations: %v", err)
		return
	}
	for _, m := range migs {
		switch m.State {
		case api.MigAwaitingAgent, api.MigReplicating, api.MigReady:
			if err := s.ensureReceivers(m); err != nil {
				log.Printf("appliance: start receivers for migration %d: %v", m.ID, err)
			}
		}
	}
}
