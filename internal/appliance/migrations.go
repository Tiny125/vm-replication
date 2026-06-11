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

	go func() {
		err := receiver.Serve(ctx, ln, device, manifest, false, func(st receiver.Stats) {
			total := blockdiff.NumBlocks(st.Hello.DeviceSize, st.Hello.BlockSize)
			bytes := st.BlocksWritten * int64(st.Hello.BlockSize)
			if err := s.st.RecordMigrationSync(s.ctx, migID, st.Hello.FullSync, total, st.ChangedBlocks, bytes); err != nil {
				log.Printf("appliance: record sync for migration %d: %v", migID, err)
			}
		})
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
	if !view.CanMigrate {
		writeErr(w, http.StatusConflict, "validation checks not satisfied; see migration validations")
		return
	}
	if err := s.st.SetMigrationState(ctx, id, api.MigMigrating, ""); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.stopReceiver(id) // quiesce: stop accepting blocks during finalize
	go s.finalize(m, req)
	writeJSON(w, http.StatusAccepted, map[string]string{"status": "migrating"})
}

// finalize converts the replicated disk to boot on Linode and produces the
// artifact (a cloned volume) plus an optional launched instance. Runs async;
// state/last_error reflect progress. Linode steps require a configured token.
func (s *Server) finalize(m api.Migration, req api.FinalizeRequest) {
	ctx := context.Background()
	device := s.dataDevicePath(m)

	// 1) Make the replicated disk bootable on Linode (virtio/GRUB/fstab/etc).
	if s.cfg.ConvertScript != "" && isBlockDevice(device) {
		log.Printf("appliance: migration %d: running machine conversion on %s", m.ID, device)
		out, err := exec.CommandContext(ctx, "/bin/sh", s.cfg.ConvertScript, device).CombinedOutput()
		if err != nil {
			s.fail(m.ID, fmt.Sprintf("machine-convert: %v: %s", err, trimOut(out)))
			return
		}
	} else {
		log.Printf("appliance: migration %d: skipping machine-convert (no script or non-block device)", m.ID)
	}

	cl, ok := s.linodeClient(ctx)
	if !ok || m.VolumeID == 0 {
		// No Linode automation: the converted data sits on the device; done.
		_ = s.st.SetMigrationState(ctx, m.ID, api.MigImageReady,
			"Linode automation not configured; migrated data is on "+device)
		return
	}

	// 2) Clone the volume into the immutable artifact ("snapshot").
	artifactLabel := fmt.Sprintf("vmrepl-img-%d", m.ID)
	log.Printf("appliance: migration %d: cloning volume %d -> %s", m.ID, m.VolumeID, artifactLabel)
	clone, err := cl.CloneVolume(ctx, m.VolumeID, artifactLabel)
	if err != nil {
		s.fail(m.ID, "clone volume: "+err.Error())
		return
	}
	if _, err := cl.WaitVolumeActive(ctx, clone.ID, 5*time.Minute); err != nil {
		s.fail(m.ID, "wait clone active: "+err.Error())
		return
	}
	_ = s.st.SetMigrationImage(ctx, m.ID, fmt.Sprintf("volume:%d", clone.ID), 0)

	// 3) Optionally launch a new instance booting from the cloned volume.
	if req.LaunchInstance {
		region := orDefault(req.Region, s.cfg.Region)
		typ := orDefault(req.Type, "g6-standard-2")
		label := orDefault(req.Label, m.Name)
		inst, err := cl.CreateInstance(ctx, label, region, typ)
		if err != nil {
			s.fail(m.ID, "create instance: "+err.Error())
			return
		}
		if err := cl.AttachVolume(ctx, clone.ID, inst.ID); err != nil {
			s.fail(m.ID, "attach artifact: "+err.Error())
			return
		}
		cfgID, err := cl.CreateConfigBootingVolume(ctx, inst.ID, clone.ID, "boot-migrated")
		if err != nil {
			s.fail(m.ID, "create boot config: "+err.Error())
			return
		}
		if err := cl.Boot(ctx, inst.ID, cfgID); err != nil {
			s.fail(m.ID, "boot instance: "+err.Error())
			return
		}
		_ = s.st.SetMigrationImage(ctx, m.ID, fmt.Sprintf("volume:%d", clone.ID), inst.ID)
		_ = s.st.SetMigrationState(ctx, m.ID, api.MigLaunched, "")
		return
	}

	_ = s.st.SetMigrationState(ctx, m.ID, api.MigImageReady, "")
}

func (s *Server) fail(id int64, msg string) {
	log.Printf("appliance: migration %d failed: %s", id, msg)
	_ = s.st.SetMigrationState(context.Background(), id, api.MigFailed, msg)
}

// ---- view + validations ----

func (s *Server) view(ctx context.Context, m api.Migration, token string) api.MigrationView {
	v := api.MigrationView{Migration: m}
	if !m.LastSyncAt.IsZero() {
		v.RPOSeconds = time.Since(m.LastSyncAt).Seconds()
	}
	v.Validations = s.validations(m, v.RPOSeconds)
	v.CanMigrate = allOK(v.Validations) &&
		(m.State == api.MigReplicating || m.State == api.MigReady || m.State == api.MigAwaitingAgent)

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
