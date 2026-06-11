package appliance

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/exec"
	"time"

	"github.com/tiny125/vm-replication/internal/api"
	"github.com/tiny125/vm-replication/internal/blockdiff"
	"github.com/tiny125/vm-replication/internal/receiver"
	"github.com/tiny125/vm-replication/internal/transport"
)

// ---- receiver manager ----

// ensureReceiver starts (idempotently) an embedded receiver for a migration,
// listening on its assigned port and writing to its data device.
func (s *Server) ensureReceiver(m api.Migration) error {
	if m.ReceiverPort == 0 {
		return fmt.Errorf("migration %d has no receiver port", m.ID)
	}
	s.recMu.Lock()
	defer s.recMu.Unlock()
	if _, ok := s.receivers[m.ID]; ok {
		return nil
	}
	tlsCfg, err := transport.ServerConfig(s.cfg.TLS)
	if err != nil {
		return err
	}
	ln, err := tls.Listen("tcp", fmt.Sprintf(":%d", m.ReceiverPort), tlsCfg)
	if err != nil {
		return fmt.Errorf("listen :%d: %w", m.ReceiverPort, err)
	}
	ctx, cancel := context.WithCancel(s.ctx)
	s.receivers[m.ID] = cancel

	device := s.dataDevicePath(m)
	manifest := s.manifestPath(m)
	migID := m.ID
	log.Printf("appliance: receiver for migration %d listening on :%d -> %s", migID, m.ReceiverPort, device)

	onProgress := func(written, total int64, fullSync bool) {
		v, _ := s.progress.LoadOrStore(migID, &syncProgress{started: time.Now()})
		p := v.(*syncProgress)
		p.mu.Lock()
		if written == 0 {
			p.started = time.Now()
		}
		p.written, p.total, p.fullSync = written, total, fullSync
		p.mu.Unlock()
	}

	go func() {
		err := receiver.Serve(ctx, ln, device, manifest, false, func(st receiver.Stats) {
			total := blockdiff.NumBlocks(st.Hello.DeviceSize, st.Hello.BlockSize)
			bytes := st.BlocksWritten * int64(st.Hello.BlockSize)
			if err := s.st.RecordMigrationSync(s.ctx, migID, st.Hello.FullSync, total, st.ChangedBlocks, bytes); err != nil {
				log.Printf("appliance: record sync for migration %d: %v", migID, err)
			}
		}, onProgress)
		if err != nil && ctx.Err() == nil {
			log.Printf("appliance: receiver for migration %d stopped: %v", migID, err)
		}
	}()
	return nil
}

func (s *Server) stopReceiver(id int64) {
	s.recMu.Lock()
	defer s.recMu.Unlock()
	if cancel, ok := s.receivers[id]; ok {
		cancel()
		delete(s.receivers, id)
	}
}

// ---- migration handlers ----

func (s *Server) handleCreateMigration(w http.ResponseWriter, r *http.Request) {
	var req api.CreateMigrationRequest
	if !readJSON(w, r, &req) {
		return
	}
	if req.Name == "" || req.SourceDevice == "" || req.SourceDiskSize <= 0 {
		writeErr(w, http.StatusBadRequest, "name, source_device and a positive source_disk_size are required")
		return
	}
	ctx := r.Context()
	m, token, err := s.st.CreateMigration(ctx, req)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	// Assign a unique receiver port.
	port := s.cfg.BaseReceiverPort + int(m.ID)
	if err := s.st.AssignReceiverPort(ctx, m.ID, port); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}

	// Provision storage: a Linode volume if automation is configured, else a
	// file under DataDir (created lazily by the receiver on first write).
	if err := s.provisionStorage(ctx, m.ID, req.SourceDiskSize); err != nil {
		_ = s.st.SetMigrationState(ctx, m.ID, api.MigFailed, "provision storage: "+err.Error())
		writeErr(w, http.StatusInternalServerError, "provision storage: "+err.Error())
		return
	}

	_ = s.st.SetMigrationState(ctx, m.ID, api.MigAwaitingAgent, "")
	m, _ = s.st.Migration(ctx, m.ID)
	if err := s.ensureReceiver(m); err != nil {
		log.Printf("appliance: ensureReceiver: %v", err)
	}
	writeJSON(w, http.StatusCreated, s.view(ctx, m, token))
}

// provisionStorage creates and attaches a Linode volume for the migration when
// automation is configured; otherwise it's a no-op (file fallback).
func (s *Server) provisionStorage(ctx context.Context, migID, sourceBytes int64) error {
	cl, ok := s.linodeClient(ctx)
	if !ok || s.cfg.ApplianceLinodeID == 0 {
		return nil // file fallback
	}
	sizeGiB := int((sourceBytes + (1 << 30) - 1) / (1 << 30))
	if sizeGiB < 10 {
		sizeGiB = 10 // Linode minimum volume size
	}
	label := fmt.Sprintf("vmrepl-%d", migID)
	vol, err := cl.CreateVolume(ctx, label, s.cfg.Region, sizeGiB, s.cfg.ApplianceLinodeID)
	if err != nil {
		return err
	}
	if _, err := cl.WaitVolumeActive(ctx, vol.ID, 3*time.Minute); err != nil {
		return err
	}
	device := vol.FilesystemPath
	if device == "" {
		device = "/dev/disk/by-id/scsi-0Linode_Volume_" + label
	}
	return s.st.SetMigrationVolume(ctx, migID, vol.ID, device)
}

func (s *Server) handleListMigrations(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	migs, err := s.st.ListMigrations(ctx)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	out := make([]api.MigrationView, 0, len(migs))
	for _, m := range migs {
		out = append(out, s.view(ctx, m, ""))
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleGetMigration(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	ctx := r.Context()
	m, err := s.st.Migration(ctx, id)
	if err != nil {
		writeErr(w, http.StatusNotFound, "not found")
		return
	}
	writeJSON(w, http.StatusOK, s.view(ctx, m, ""))
}

// handleAssessMigration runs the pre-migration assessment: it re-evaluates all
// validation checks and records a pass (gating the Start button). A failed
// assessment clears any earlier pass and returns the failing checks.
func (s *Server) handleAssessMigration(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	ctx := r.Context()
	m, err := s.st.Migration(ctx, id)
	if err != nil {
		writeErr(w, http.StatusNotFound, "not found")
		return
	}
	view := s.view(ctx, m, "")
	passed := allOK(view.Validations)
	if err := s.st.SetAssessed(ctx, id, passed); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	m, _ = s.st.Migration(ctx, id)
	writeJSON(w, http.StatusOK, s.view(ctx, m, ""))
}

func (s *Server) handleStartMigration(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	var req api.FinalizeRequest
	// Body is optional (defaults = no launch); tolerate an empty body.
	_ = json.NewDecoder(r.Body).Decode(&req)
	ctx := r.Context()
	m, err := s.st.Migration(ctx, id)
	if err != nil {
		writeErr(w, http.StatusNotFound, "not found")
		return
	}
	view := s.view(ctx, m, "")
	if !view.Assessed {
		writeErr(w, http.StatusConflict, "run the pre-migration assessment first")
		return
	}
	if !view.CanMigrate {
		writeErr(w, http.StatusConflict, "validation checks not satisfied; see migration validations")
		return
	}
	if err := s.st.SetMigrationState(ctx, id, api.MigMigrating, ""); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	_ = s.st.SetMigrateStarted(ctx, id)
	s.stopReceiver(id) // quiesce: stop accepting blocks during finalize

	finCtx, cancel := context.WithCancel(s.ctx)
	s.recMu.Lock()
	s.finalizes[id] = cancel
	s.recMu.Unlock()
	go s.finalize(finCtx, m, req)
	writeJSON(w, http.StatusAccepted, map[string]string{"status": "migrating"})
}

// handleStopMigration cancels an in-flight finalize and returns the migration
// to replicating (the receiver restarts; the assessment must be re-run).
func (s *Server) handleStopMigration(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	ctx := r.Context()
	m, err := s.st.Migration(ctx, id)
	if err != nil {
		writeErr(w, http.StatusNotFound, "not found")
		return
	}
	if m.State != api.MigMigrating {
		writeErr(w, http.StatusConflict, "migration is not running")
		return
	}
	s.recMu.Lock()
	cancel := s.finalizes[id]
	delete(s.finalizes, id)
	s.recMu.Unlock()
	if cancel != nil {
		cancel()
	}
	_ = s.st.SetAssessed(ctx, id, false) // require a fresh assessment before retry
	_ = s.st.SetMigrationState(ctx, id, api.MigReplicating, "stopped by operator")
	if m2, err := s.st.Migration(ctx, id); err == nil {
		_ = s.ensureReceiver(m2)
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "stopped"})
}

// handleDeleteMigration removes a migration: stops its receiver, cancels any
// finalize, best-effort detaches and deletes its replication volume (the cloned
// artifact, if one was produced, is kept), and deletes the record.
func (s *Server) handleDeleteMigration(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	ctx := r.Context()
	m, err := s.st.Migration(ctx, id)
	if err != nil {
		writeErr(w, http.StatusNotFound, "not found")
		return
	}
	s.recMu.Lock()
	if cancel := s.finalizes[id]; cancel != nil {
		cancel()
		delete(s.finalizes, id)
	}
	s.recMu.Unlock()
	s.stopReceiver(id)
	s.progress.Delete(id)

	if m.VolumeID != 0 {
		if cl, ok := s.linodeClient(ctx); ok {
			if err := cl.DetachVolume(ctx, m.VolumeID); err != nil {
				log.Printf("appliance: detach volume %d: %v", m.VolumeID, err)
			}
			// Detach is async; retry the delete briefly.
			var derr error
			for i := 0; i < 10; i++ {
				if derr = cl.DeleteVolume(ctx, m.VolumeID); derr == nil {
					break
				}
				time.Sleep(2 * time.Second)
			}
			if derr != nil {
				log.Printf("appliance: delete volume %d failed (remove it in Cloud Manager): %v", m.VolumeID, derr)
			}
		}
	} else {
		_ = os.Remove(s.dataDevicePath(m)) // file-fallback image
	}
	_ = os.Remove(s.manifestPath(m))

	if err := s.st.DeleteMigration(ctx, id); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// finalize converts the replicated disk to boot on Linode and produces the
// artifact (a cloned volume) plus an optional launched instance. Runs async
// under a cancellable ctx (the console Stop button); state/last_error reflect
// progress. Linode steps require a configured token.
func (s *Server) finalize(ctx context.Context, m api.Migration, req api.FinalizeRequest) {
	defer func() {
		s.recMu.Lock()
		delete(s.finalizes, m.ID)
		s.recMu.Unlock()
	}()
	canceled := func() bool { return ctx.Err() != nil }
	device := s.dataDevicePath(m)

	// Store writes use the server context: ctx is canceled by the Stop button,
	// and the Stop handler owns the state transition in that case.
	sctx := s.ctx

	// 1) Make the replicated disk bootable on Linode (virtio/GRUB/fstab/etc).
	if s.cfg.ConvertScript != "" && isBlockDevice(device) {
		log.Printf("appliance: migration %d: running machine conversion on %s", m.ID, device)
		out, err := exec.CommandContext(ctx, "/bin/sh", s.cfg.ConvertScript, device).CombinedOutput()
		if canceled() {
			return
		}
		if err != nil {
			s.fail(m.ID, fmt.Sprintf("machine-convert: %v: %s", err, trimOut(out)))
			return
		}
	} else {
		log.Printf("appliance: migration %d: skipping machine-convert (no script or non-block device)", m.ID)
	}

	cl, ok := s.linodeClient(sctx)
	if !ok || m.VolumeID == 0 {
		// No Linode automation: the converted data sits on the device; done.
		_ = s.st.SetMigrationState(sctx, m.ID, api.MigImageReady,
			"Linode automation not configured; migrated data is on "+device)
		_ = s.st.SetMigrateFinished(sctx, m.ID)
		return
	}

	// 2) Clone the volume into the immutable artifact ("snapshot").
	artifactLabel := fmt.Sprintf("vmrepl-img-%d", m.ID)
	log.Printf("appliance: migration %d: cloning volume %d -> %s", m.ID, m.VolumeID, artifactLabel)
	clone, err := cl.CloneVolume(ctx, m.VolumeID, artifactLabel)
	if canceled() {
		return
	}
	if err != nil {
		s.fail(m.ID, "clone volume: "+err.Error())
		return
	}
	if _, err := cl.WaitVolumeActive(ctx, clone.ID, 10*time.Minute); err != nil {
		if canceled() {
			return
		}
		s.fail(m.ID, "wait clone active: "+err.Error())
		return
	}
	_ = s.st.SetMigrationImage(sctx, m.ID, fmt.Sprintf("volume:%d", clone.ID), 0)

	// 3) Optionally launch a new instance booting from the cloned volume.
	if req.LaunchInstance {
		// The cloned volume lives in the appliance's region, so the instance must
		// too; resolve it from the appliance rather than trusting a flag.
		region := req.Region
		if region == "" {
			if inst, err := cl.GetInstance(ctx, s.cfg.ApplianceLinodeID); err == nil && inst.Region != "" {
				region = inst.Region
			} else {
				region = s.cfg.Region
			}
		}
		typ := orDefault(req.Type, "g6-standard-2")
		label := orDefault(req.Label, m.Name)
		inst, err := cl.CreateInstance(ctx, label, region, typ)
		if canceled() {
			return
		}
		if err != nil {
			s.fail(m.ID, "create instance: "+err.Error())
			return
		}
		if err := cl.AttachVolume(ctx, clone.ID, inst.ID); err != nil {
			if canceled() {
				return
			}
			s.fail(m.ID, "attach artifact: "+err.Error())
			return
		}
		cfgID, err := cl.CreateConfigBootingVolume(ctx, inst.ID, clone.ID, "boot-migrated")
		if err != nil {
			if canceled() {
				return
			}
			s.fail(m.ID, "create boot config: "+err.Error())
			return
		}
		if err := cl.Boot(ctx, inst.ID, cfgID); err != nil {
			if canceled() {
				return
			}
			s.fail(m.ID, "boot instance: "+err.Error())
			return
		}
		_ = s.st.SetMigrationImage(sctx, m.ID, fmt.Sprintf("volume:%d", clone.ID), inst.ID)
		_ = s.st.SetMigrationState(sctx, m.ID, api.MigLaunched, "")
		_ = s.st.SetMigrateFinished(sctx, m.ID)
		return
	}

	_ = s.st.SetMigrationState(sctx, m.ID, api.MigImageReady, "")
	_ = s.st.SetMigrateFinished(sctx, m.ID)
}

func (s *Server) fail(id int64, msg string) {
	log.Printf("appliance: migration %d failed: %s", id, msg)
	_ = s.st.SetMigrationState(context.Background(), id, api.MigFailed, msg)
}

// ---- view + validations ----

func (s *Server) view(ctx context.Context, m api.Migration, token string) api.MigrationView {
	v := api.MigrationView{Migration: m, PercentDone: -1, ETASeconds: -1}
	if !m.LastSyncAt.IsZero() {
		v.RPOSeconds = time.Since(m.LastSyncAt).Seconds()
	}
	v.Validations = s.validations(m, v.RPOSeconds)
	v.CanMigrate = allOK(v.Validations) &&
		(m.State == api.MigReplicating || m.State == api.MigReady || m.State == api.MigAwaitingAgent)
	v.Assessed = !m.AssessedAt.IsZero() && v.CanMigrate // a pass goes stale if checks regress

	// Live phase / percent / ETA for the console (auto-refreshed by polling).
	switch m.State {
	case api.MigCreated, api.MigAwaitingAgent:
		v.Phase = "waiting for agent"
	case api.MigReplicating, api.MigReady:
		v.Phase = "replicating"
		if p, ok := s.progress.Load(m.ID); ok {
			sp := p.(*syncProgress)
			sp.mu.Lock()
			written, total, full, started := sp.written, sp.total, sp.fullSync, sp.started
			sp.mu.Unlock()
			// Percent + ETA are meaningful during the initial full sync, where the
			// expected block count is known up front.
			if full && !m.FullSyncDone && total > 0 && written < total {
				v.Phase = "initial sync"
				v.PercentDone = float64(written) / float64(total) * 100
				if elapsed := time.Since(started); written > 0 && elapsed > 0 {
					remain := float64(elapsed) * float64(total-written) / float64(written)
					v.ETASeconds = int64(time.Duration(remain).Seconds())
				}
				v.ElapsedSeconds = int64(time.Since(started).Seconds())
			}
		}
	case api.MigMigrating:
		v.Phase = "finalizing (convert + clone)"
		if !m.MigrateStarted.IsZero() {
			v.ElapsedSeconds = int64(time.Since(m.MigrateStarted).Seconds())
		}
	case api.MigImageReady, api.MigLaunched:
		v.Phase = "completed"
		v.PercentDone = 100
		if !m.MigrateStarted.IsZero() && !m.MigrateFinished.IsZero() {
			v.ElapsedSeconds = int64(m.MigrateFinished.Sub(m.MigrateStarted).Seconds())
		}
	case api.MigFailed:
		v.Phase = "failed"
	}

	if token == "" {
		// Look up the enrollment token so the console can show the command.
		token, _ = s.st.EnrollToken(ctx, m.ID)
	}
	if token != "" {
		v.EnrollCmd = s.enrollCmd(token, m)
	}
	return v
}

func (s *Server) validations(m api.Migration, rpoSec float64) []api.ValidationCheck {
	agentSeen := !m.AgentLastSeen.IsZero() && time.Since(m.AgentLastSeen) < 5*time.Minute
	lagOK := !m.LastSyncAt.IsZero() && rpoSec <= float64(s.cfg.RPOTargetSec)
	storageOK := m.VolumeDevice != "" || s.cfg.ApplianceLinodeID == 0 // file fallback counts as ok

	return []api.ValidationCheck{
		{Name: "Agent connected", OK: agentSeen, Detail: lastSeenDetail(m.AgentLastSeen)},
		{Name: "Initial full sync complete", OK: m.FullSyncDone, Detail: boolDetail(m.FullSyncDone, "baseline replicated", "no full sync yet")},
		{Name: fmt.Sprintf("Replication lag within %ds", s.cfg.RPOTargetSec), OK: lagOK, Detail: lagDetail(m.LastSyncAt, rpoSec)},
		{Name: "Storage provisioned", OK: storageOK, Detail: boolDetail(storageOK, "ready", "volume not attached")},
	}
}

func (s *Server) enrollCmd(token string, m api.Migration) string {
	return fmt.Sprintf("curl -fsSL %s'%s://%s:%d/install/agent.sh?token=%s' | sudo bash",
		s.curlPinFlag(), s.scheme(), s.cfg.PublicHost, s.cfg.ConsolePort, token)
}

// scheme returns the console URL scheme (https unless explicitly http).
func (s *Server) scheme() string {
	if s.cfg.Scheme == "" {
		return "https"
	}
	return s.cfg.Scheme
}

// curlPinFlag returns the curl flags (with trailing space) that authenticate the
// download against the self-signed console cert via public-key pinning. -k skips
// CA-chain validation (we have no public CA) while --pinnedpubkey still requires
// the server to prove possession of the pinned key, so the connection remains
// MITM-proof. Empty when no pin is configured (plain HTTP / external CA).
func (s *Server) curlPinFlag() string {
	if s.cfg.PublicKeyPin != "" {
		return "-k --pinnedpubkey 'sha256//" + s.cfg.PublicKeyPin + "' "
	}
	return ""
}

// ---- helpers ----

func allOK(checks []api.ValidationCheck) bool {
	for _, c := range checks {
		if !c.OK {
			return false
		}
	}
	return true
}

func isBlockDevice(path string) bool {
	info, err := os.Stat(path)
	if err != nil {
		return false
	}
	return info.Mode()&os.ModeDevice != 0
}

func trimOut(b []byte) string {
	const max = 2000
	if len(b) > max {
		b = b[len(b)-max:]
	}
	return string(b)
}

func orDefault(v, def string) string {
	if v == "" {
		return def
	}
	return v
}

func boolDetail(ok bool, yes, no string) string {
	if ok {
		return yes
	}
	return no
}

func lastSeenDetail(t time.Time) string {
	if t.IsZero() {
		return "agent has not checked in"
	}
	return "last seen " + time.Since(t).Round(time.Second).String() + " ago"
}

func lagDetail(last time.Time, rpoSec float64) string {
	if last.IsZero() {
		return "no completed sync yet"
	}
	return fmt.Sprintf("%.0fs since last successful sync", rpoSec)
}
