// Package appliance is the turnkey vm-replication control plane: a single
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
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"
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
	ObjRegion         string          // explicit Object Storage region for audit logs (empty = follow the appliance's region)
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
	receivers map[int64]*receiverHandle    // diskID -> its receiver (stop + drain)
	finalizes map[int64]context.CancelFunc // migrationID -> cancel finalize run
	// Crash-consistent cutover coordination (keyed by disk ID, guarded by recMu):
	// consistReq marks a disk whose agent we want to re-read from a point-in-time
	// snapshot before launch; consistDone marks a disk that has since delivered
	// such a crash-consistent sync.
	consistReq  map[int64]bool
	consistDone map[int64]bool
	// quiesceErr records that a disk's agent reported it could not capture a
	// consistent image for cutover (keyed by diskID), so the wait can fail fast.
	quiesceErr map[int64]string
	// pendingCutover holds the FinalizeRequest from a guided cutover's phase 1 so
	// the /complete endpoint (phase 2) can reuse the operator's options without
	// re-prompting (keyed by migrationID, guarded by recMu).
	pendingCutover map[int64]api.FinalizeRequest
	progress       sync.Map // migrationID -> *syncProgress
	// Disk-boot cutover image streaming (see cutover_stream.go):
	// cutoverStreams: token -> *cutoverStream (authorized image downloads);
	// cutoverCmds:    migrationID -> the Lish copy command the console shows.
	cutoverStreams sync.Map
	cutoverCmds    sync.Map
	ctx            context.Context

	auditCh chan auditEntry // buffered audit entries -> DB (best-effort)
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
		receivers:      map[int64]*receiverHandle{},
		finalizes:      map[int64]context.CancelFunc{},
		consistReq:     map[int64]bool{},
		consistDone:    map[int64]bool{},
		quiesceErr:     map[int64]string{},
		pendingCutover: map[int64]api.FinalizeRequest{},
		ctx:            ctx,
		auditCh:        make(chan auditEntry, 2048),
	}
	go s.auditDrain()
	go s.auditUploader()
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

	// Disk-boot cutover copy (authenticated by a per-cutover token — the rescue
	// instance downloading these has no console session).
	s.mux.HandleFunc("GET /cutover/copy.sh", s.handleCutoverScript)
	s.mux.HandleFunc("GET /cutover/image", s.handleCutoverImage)

	// Console API (session-protected).
	s.mux.Handle("GET /api/v1/session", s.auth(s.handleSession))
	s.mux.Handle("GET /api/v1/migrations", s.auth(s.handleListMigrations))
	s.mux.Handle("POST /api/v1/migrations", s.auth(s.handleCreateMigration))
	s.mux.Handle("GET /api/v1/migrations/{id}", s.auth(s.handleGetMigration))
	s.mux.Handle("GET /api/v1/migrations/{id}/events", s.auth(s.handleMigrationEvents))
	s.mux.Handle("DELETE /api/v1/migrations/{id}", s.auth(s.handleDeleteMigration))
	s.mux.Handle("POST /api/v1/migrations/{id}/close", s.auth(s.handleCloseMigration))
	s.mux.Handle("POST /api/v1/migrations/{id}/replicate", s.auth(s.handleStartReplication))
	s.mux.Handle("POST /api/v1/migrations/{id}/pause", s.auth(s.handlePauseReplication))
	s.mux.Handle("POST /api/v1/migrations/{id}/start", s.auth(s.handleStartMigration))
	s.mux.Handle("POST /api/v1/migrations/{id}/complete", s.auth(s.handleCompleteCutover))
	s.mux.Handle("POST /api/v1/migrations/{id}/stop", s.auth(s.handleStopMigration))
	s.mux.Handle("POST /api/v1/diagnostics/connection", s.auth(s.handleConnTest))
	s.mux.Handle("GET /api/v1/linode/plans", s.auth(s.handleLinodePlans))
	s.mux.Handle("GET /api/v1/settings", s.auth(s.handleGetSettings))
	s.mux.Handle("POST /api/v1/settings/linode-token", s.auth(s.handleSetLinodeToken))
	s.mux.Handle("DELETE /api/v1/settings/linode-token", s.auth(s.handleDeleteLinodeToken))
	s.mux.Handle("POST /api/v1/settings/audit-bucket", s.auth(s.handleProvisionAuditBucket))
	s.mux.Handle("DELETE /api/v1/settings/audit-bucket", s.auth(s.handleDeleteAuditBucket))
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
		// Audit the console action. Skip read-only polling (GET) so the log
		// records what the operator *did*, not the dashboard's refresh traffic.
		if r.Method != http.MethodGet {
			sw := &statusWriter{ResponseWriter: w, code: http.StatusOK}
			h(sw, r)
			s.auditAction(levelForStatus(sw.code), fmt.Sprintf("%s %s -> %d", r.Method, r.URL.Path, sw.code))
			return
		}
		h(w, r)
	})
}

// statusWriter captures the response status for the audit log.
type statusWriter struct {
	http.ResponseWriter
	code    int
	written bool
}

func (w *statusWriter) WriteHeader(code int) {
	if !w.written {
		w.code, w.written = code, true
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Write(b []byte) (int, error) {
	w.written = true
	return w.ResponseWriter.Write(b)
}

func levelForStatus(code int) string {
	if code >= 500 {
		return "error"
	}
	if code >= 400 {
		return "warn"
	}
	return "info"
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
	s.auditAction("info", "admin logged in to the console")
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
	ctx := r.Context()
	linodeSet, _ := s.st.LinodeTokenSet(ctx)
	account, _ := s.st.LinodeAccount(ctx)
	auditReady, _, _ := s.st.GetSetting(ctx, keyAuditReady)
	auditErr, _, _ := s.st.GetSetting(ctx, keyAuditErr)
	bucket, _ := s.auditBucket(ctx)
	writeJSON(w, http.StatusOK, map[string]any{
		"linode_token_set":    linodeSet,
		"linode_account":      account,
		"appliance_linode_id": s.cfg.ApplianceLinodeID,
		"public_host":         s.cfg.PublicHost,
		"region":              s.cfg.Region,
		"linode_automation":   s.cfg.ApplianceLinodeID != 0,
		"audit_ready":         auditReady == "1",
		"audit_bucket":        bucket.Label,
		"audit_region":        bucket.Region,
		"audit_error":         auditErr,
	})
}

func (s *Server) handleSetLinodeToken(w http.ResponseWriter, r *http.Request) {
	var req api.SetLinodeTokenRequest
	if !readJSON(w, r, &req) {
		return
	}
	token := strings.TrimSpace(req.Token)
	if token == "" {
		writeErr(w, http.StatusBadRequest, "token is required")
		return
	}
	// Validate the token against the real Linode API before storing it, so a
	// typo or revoked token is rejected immediately instead of failing later
	// during provisioning. GET /profile also tells us which account it belongs to.
	ctx := r.Context()
	prof, err := linode.New(token).GetProfile(ctx)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "Linode rejected this token — check it is valid and has Linodes + Volumes read/write: "+err.Error())
		return
	}
	if err := s.st.SetLinodeToken(ctx, token); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	account := prof.Username
	if account != "" && prof.Email != "" {
		account = prof.Username + " <" + prof.Email + ">"
	} else if account == "" {
		account = prof.Email
	}
	_ = s.st.SetLinodeAccount(ctx, account)
	// Provision the audit-log bucket (best-effort: a failure here, e.g. the token
	// lacks Object Storage scope, doesn't block using the token for migrations).
	s.ensureAuditBucket(ctx, token)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "linode_account": account})
}

// handleDeleteLinodeToken removes the stored token (e.g. expired/rotated). It is
// refused while any migration is still ACTIVE (created or running): deleting such
// a migration uses the token to remove its Linode volumes, so removing the token
// first would orphan those volumes (the operator would have to clean them up by
// hand in Cloud Manager). Completed migrations (launched / image ready) or failed
// ones do not block removal.
func (s *Server) handleDeleteLinodeToken(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	migs, err := s.st.ListMigrations(ctx)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if n := activeMigrationCount(migs); n > 0 {
		writeErr(w, http.StatusConflict, fmt.Sprintf("cannot remove the Linode API token while %d migration(s) are still active (created or running) — deleting such a migration uses the token to remove its Linode volumes, so removing it now would orphan those volumes (you'd have to delete them by hand in Cloud Manager). Finish, launch, or delete those migrations first. Completed migrations don't block removal.", n))
		return
	}
	if err := s.st.DeleteLinodeToken(ctx); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// handleProvisionAuditBucket (re-)creates the audit-log Object Storage bucket
// (named vmrep-audit-<instance id>). If a bucket with that name already exists
// (we made it before, or the operator created it manually) it reports that
// instead of recreating. Useful after deleting the bucket, or to re-point the
// console at an existing one.
func (s *Server) handleProvisionAuditBucket(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	tok, err := s.st.LinodeToken(ctx)
	if err != nil || tok == "" {
		writeErr(w, http.StatusBadRequest, "add a Linode API token first")
		return
	}
	cl := linode.New(tok)
	if b, ok := s.existingAuditBucket(ctx, cl); ok {
		s.saveAuditBucket(ctx, b) // make sure our settings point at it
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "already_exists": true, "audit_bucket": b.Label, "audit_region": b.Region})
		return
	}
	s.ensureAuditBucket(ctx, tok)
	ready, _, _ := s.st.GetSetting(ctx, keyAuditReady)
	if ready != "1" {
		aerr, _, _ := s.st.GetSetting(ctx, keyAuditErr)
		writeErr(w, http.StatusBadGateway, "could not create the audit bucket: "+aerr)
		return
	}
	b, _ := s.auditBucket(ctx)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "created": true, "audit_bucket": b.Label, "audit_region": b.Region})
}

// handleDeleteAuditBucket empties and deletes the audit-log Object Storage bucket
// (and all logs in it). Guarded two ways: no migration may be active (created or
// running), and the caller must supply the correct console password.
func (s *Server) handleDeleteAuditBucket(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	var req struct {
		Password string `json:"password"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	ok, err := s.st.VerifyAdminPassword(ctx, req.Password)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if !ok {
		writeErr(w, http.StatusForbidden, "incorrect console password")
		return
	}
	migs, err := s.st.ListMigrations(ctx)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	active := activeMigrationCount(migs)
	if active > 0 {
		writeErr(w, http.StatusConflict, fmt.Sprintf("cannot delete the audit bucket while %d migration(s) are still active (created or running) — finish, launch, or delete them first", active))
		return
	}
	tok, err := s.st.LinodeToken(ctx)
	if err != nil || tok == "" {
		writeErr(w, http.StatusBadRequest, "no Linode API token is configured")
		return
	}
	cl := linode.New(tok)
	b, ok := s.auditBucket(ctx)
	if !ok {
		// Not tracked locally as ready — it may still exist in the account; find it.
		if eb, found := s.existingAuditBucket(ctx, cl); found {
			b = eb
		} else {
			s.clearAuditBucket(ctx) // nothing to delete; just clear local state
			writeJSON(w, http.StatusOK, map[string]any{"ok": true, "deleted": false})
			return
		}
	}
	if err := cl.EmptyBucket(ctx, b); err != nil {
		writeErr(w, http.StatusBadGateway, "could not empty the audit bucket: "+err.Error())
		return
	}
	if err := cl.DeleteBucket(ctx, b); err != nil {
		writeErr(w, http.StatusBadGateway, "could not delete the audit bucket: "+err.Error())
		return
	}
	s.clearAuditBucket(ctx)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "deleted": true, "audit_bucket": b.Label})
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
