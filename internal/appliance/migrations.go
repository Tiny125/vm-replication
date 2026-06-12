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
	"path/filepath"
	"time"

	"github.com/tiny125/vm-replication/internal/api"
	"github.com/tiny125/vm-replication/internal/blockdiff"
	"github.com/tiny125/vm-replication/internal/receiver"
	"github.com/tiny125/vm-replication/internal/transport"
)

// A migration moves one source server with one or more disks. Each disk gets
// its own receiver (port BaseReceiverPort+diskID), its own replication volume,
// and its own cloned artifact at cutover.

// ---- per-disk paths ----

func (s *Server) diskDevicePath(m api.Migration, d api.Disk) string {
	if d.VolumeDevice != "" {
		return d.VolumeDevice
	}
	return filepath.Join(s.cfg.DataDir, fmt.Sprintf("migration-%d-disk%d.img", m.ID, d.Index))
}

func (s *Server) diskManifestPath(m api.Migration, d api.Disk) string {
	return filepath.Join(s.cfg.DataDir, fmt.Sprintf("migration-%d-disk%d.cbt", m.ID, d.Index))
}

// ---- receiver manager (one receiver per disk) ----

// ensureReceivers starts (idempotently) an embedded receiver for every disk of
// a migration, each on its own port writing to its own device.
func (s *Server) ensureReceivers(m api.Migration) error {
	tlsCfg, err := transport.ServerConfig(s.cfg.TLS)
	if err != nil {
		return err
	}
	for _, d := range m.Disks {
		if d.ReceiverPort == 0 {
			continue
		}
		if err := s.ensureDiskReceiver(m, d, tlsCfg); err != nil {
			return err
		}
	}
	return nil
}

func (s *Server) ensureDiskReceiver(m api.Migration, d api.Disk, tlsCfg *tls.Config) error {
	s.recMu.Lock()
	defer s.recMu.Unlock()
	if _, ok := s.receivers[d.ID]; ok {
		return nil
	}
	ln, err := tls.Listen("tcp", fmt.Sprintf(":%d", d.ReceiverPort), tlsCfg)
	if err != nil {
		return fmt.Errorf("listen :%d: %w", d.ReceiverPort, err)
	}
	ctx, cancel := context.WithCancel(s.ctx)
	s.receivers[d.ID] = cancel

	device := s.diskDevicePath(m, d)
	manifest := s.diskManifestPath(m, d)
	migID, diskID := m.ID, d.ID
	log.Printf("appliance: migration %d disk %d (%s) receiver on :%d -> %s", migID, d.Index, d.SourceDevice, d.ReceiverPort, device)

	onProgress := func(written, total int64, fullSync bool) {
		v, _ := s.progress.LoadOrStore(diskID, &syncProgress{started: time.Now()})
		p := v.(*syncProgress)
		p.mu.Lock()
		if written == 0 {
			p.started = time.Now()
		}
		p.written, p.total, p.fullSync = written, total, fullSync
		p.mu.Unlock()
	}

	diskIdx := d.Index
	dev0 := d.SourceDevice
	go func() {
		err := receiver.Serve(ctx, ln, device, manifest, false, func(st receiver.Stats) {
			total := blockdiff.NumBlocks(st.Hello.DeviceSize, st.Hello.BlockSize)
			bytes := st.BlocksWritten * int64(st.Hello.BlockSize)
			wasBaselined := false
			if d0, derr := s.st.Migration(s.ctx, migID); derr == nil {
				for _, dk := range d0.Disks {
					if dk.ID == diskID {
						wasBaselined = dk.FullSyncDone
					}
				}
			}
			if err := s.st.RecordDiskSync(s.ctx, migID, diskID, st.Hello.FullSync, total, st.ChangedBlocks, bytes); err != nil {
				log.Printf("appliance: record sync (migration %d disk %d): %v", migID, diskID, err)
			}
			if st.Hello.FullSync && !wasBaselined {
				_ = s.st.AddEvent(s.ctx, migID, "info", fmt.Sprintf("disk %d (%s): initial full sync complete (%s)", diskIdx, st.Hello.DevicePath, humanBytes(st.BlocksWritten*int64(st.Hello.BlockSize))))
			} else if !wasBaselined {
				_ = s.st.AddEvent(s.ctx, migID, "info", fmt.Sprintf("disk %d: agent connected, replicating", diskIdx))
			}
		}, onProgress, func(serr error) {
			_ = s.st.RecordDiskError(s.ctx, diskID, serr.Error())
			_ = s.st.AddEvent(s.ctx, migID, "error", fmt.Sprintf("disk %d (%s): replication attempt failed: %s", diskIdx, dev0, serr.Error()))
		})
		if err != nil && ctx.Err() == nil {
			log.Printf("appliance: receiver (migration %d disk %d) stopped: %v", migID, diskID, err)
		}
	}()
	return nil
}

// stopReceivers stops every disk receiver for a migration.
func (s *Server) stopReceivers(m api.Migration) {
	s.recMu.Lock()
	defer s.recMu.Unlock()
	for _, d := range m.Disks {
		if cancel, ok := s.receivers[d.ID]; ok {
			cancel()
			delete(s.receivers, d.ID)
		}
	}
}

// ---- migration handlers ----

func (s *Server) handleCreateMigration(w http.ResponseWriter, r *http.Request) {
	var req api.CreateMigrationRequest
	if !readJSON(w, r, &req) {
		return
	}
	devices := req.Devices
	if len(devices) == 0 && req.SourceDevice != "" {
		devices = []api.DeviceSpec{{Device: req.SourceDevice, SizeBytes: req.SourceDiskSize}}
	}
	if req.Name == "" || len(devices) == 0 {
		writeErr(w, http.StatusBadRequest, "name and at least one source device are required")
		return
	}
	for _, d := range devices {
		if d.Device == "" || d.SizeBytes <= 0 {
			writeErr(w, http.StatusBadRequest, "every disk needs a device path and a positive size")
			return
		}
	}
	if len(devices) > 8 {
		writeErr(w, http.StatusBadRequest, "at most 8 disks per migration (Linode device slots sda–sdh)")
		return
	}
	req.Devices = devices

	ctx := r.Context()
	m, token, err := s.st.CreateMigration(ctx, req)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, friendlyCreateErr(err))
		return
	}

	// Assign a unique receiver port per disk and provision a volume per disk.
	for _, d := range m.Disks {
		port := s.cfg.BaseReceiverPort + int(d.ID)
		if err := s.st.AssignDiskPort(ctx, d.ID, port); err != nil {
			writeErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		if err := s.provisionDiskStorage(ctx, m, d); err != nil {
			_ = s.st.SetMigrationState(ctx, m.ID, api.MigFailed, "provision storage: "+err.Error())
			writeErr(w, http.StatusInternalServerError, "provision storage: "+err.Error())
			return
		}
	}

	_ = s.st.AddEvent(ctx, m.ID, "info", fmt.Sprintf("migration created with %d disk(s); waiting for the source agent", len(m.Disks)))
	_ = s.st.SetMigrationState(ctx, m.ID, api.MigAwaitingAgent, "")
	m, _ = s.st.Migration(ctx, m.ID)
	if err := s.ensureReceivers(m); err != nil {
		log.Printf("appliance: ensureReceivers: %v", err)
	}
	writeJSON(w, http.StatusCreated, s.view(ctx, m, token))
}

// provisionDiskStorage creates and attaches a Linode volume for one disk when
// automation is configured; otherwise it's a no-op (file fallback).
func (s *Server) provisionDiskStorage(ctx context.Context, m api.Migration, d api.Disk) error {
	cl, ok := s.linodeClient(ctx)
	if !ok || s.cfg.ApplianceLinodeID == 0 {
		return nil // file fallback
	}
	// Size the volume with headroom so the agent's exact device size (in bytes)
	// is always <= the volume, regardless of GB/GiB rounding. We round the source
	// up to whole GiB and add 1 GiB of slack; the receiver requires the target be
	// at least the source size, so undersizing would reject the session.
	sizeGiB := int((d.SizeBytes+(1<<30)-1)/(1<<30)) + 1
	if sizeGiB < 10 {
		sizeGiB = 10 // Linode minimum volume size
	}
	label := volumeLabel(m.Name, d.Index, len(m.Disks))
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
	if err := s.st.SetDiskVolume(ctx, d.ID, vol.ID, device); err != nil {
		return err
	}
	_ = s.st.AddEvent(ctx, m.ID, "info", fmt.Sprintf("disk %d (%s): provisioned %dGiB volume %q (id %d)", d.Index, d.SourceDevice, sizeGiB, label, vol.ID))
	return nil
}

// volumeLabel builds a Linode-safe volume label "vmrep-<name>" (with a disk
// suffix for multi-disk). Linode labels are 1–32 chars, [A-Za-z0-9_-].
func volumeLabel(name string, idx, total int) string {
	clean := make([]rune, 0, len(name))
	for _, r := range name {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '_' {
			clean = append(clean, r)
		} else {
			clean = append(clean, '-')
		}
	}
	base := "vmrep-" + string(clean)
	suffix := ""
	if total > 1 {
		suffix = fmt.Sprintf("-d%d", idx)
	}
	max := 32 - len(suffix)
	if len(base) > max {
		base = base[:max]
	}
	return base + suffix
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

// handleMigrationEvents returns a migration's activity log (newest first).
func (s *Server) handleMigrationEvents(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	events, err := s.st.Events(r.Context(), id, 200)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if events == nil {
		events = []api.Event{}
	}
	writeJSON(w, http.StatusOK, events)
}

// handleAssessMigration runs the pre-migration assessment: re-evaluates all
// validation checks and records a pass (gating the Cutover button).
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
	if err := s.st.SetAssessed(ctx, id, allOK(view.Validations)); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	m, _ = s.st.Migration(ctx, id)
	writeJSON(w, http.StatusOK, s.view(ctx, m, ""))
}

// handleStartMigration cuts over: it stops replication and runs finalize
// (convert + clone every disk + optional launch). The console labels this
// "Cutover instance".
func (s *Server) handleStartMigration(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	var req api.FinalizeRequest
	_ = json.NewDecoder(r.Body).Decode(&req) // body optional
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
	s.stopReceivers(m) // quiesce all disks during finalize

	finCtx, cancel := context.WithCancel(s.ctx)
	s.recMu.Lock()
	s.finalizes[id] = cancel
	s.recMu.Unlock()
	go s.finalize(finCtx, m, req)
	writeJSON(w, http.StatusAccepted, map[string]string{"status": "migrating"})
}

// handleStopMigration cancels an in-flight finalize and resumes replication.
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
	_ = s.st.SetAssessed(ctx, id, false)
	_ = s.st.SetMigrationState(ctx, id, api.MigReplicating, "stopped by operator")
	_ = s.st.AddEvent(ctx, id, "warn", "cutover stopped by operator; replication resumed")
	if m2, err := s.st.Migration(ctx, id); err == nil {
		_ = s.ensureReceivers(m2)
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "stopped"})
}

// handleDeleteMigration removes a migration: stops all receivers, cancels any
// finalize, best-effort detaches+deletes every replication volume (cloned
// artifacts are kept), and deletes the record.
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
	s.stopReceivers(m)

	cl, haveLinode := s.linodeClient(ctx)
	for _, d := range m.Disks {
		s.progress.Delete(d.ID)
		if d.VolumeID != 0 && haveLinode {
			_ = cl.DetachVolume(ctx, d.VolumeID)
			var derr error
			for i := 0; i < 10; i++ {
				if derr = cl.DeleteVolume(ctx, d.VolumeID); derr == nil {
					break
				}
				time.Sleep(2 * time.Second)
			}
			if derr != nil {
				log.Printf("appliance: delete volume %d failed (remove it in Cloud Manager): %v", d.VolumeID, derr)
			}
		} else {
			_ = os.Remove(s.diskDevicePath(m, d))
		}
		_ = os.Remove(s.diskManifestPath(m, d))
	}

	if err := s.st.DeleteMigration(ctx, id); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// finalize converts the boot disk to boot on Linode, clones every disk's volume
// into an artifact, and optionally launches an instance with all artifacts
// attached. Runs under a cancellable ctx (the Stop button).
func (s *Server) finalize(ctx context.Context, m api.Migration, req api.FinalizeRequest) {
	defer func() {
		s.recMu.Lock()
		delete(s.finalizes, m.ID)
		s.recMu.Unlock()
	}()
	canceled := func() bool { return ctx.Err() != nil }
	sctx := s.ctx // store writes survive a Stop (which owns its own transition)

	boot := m.Disks[0]
	bootDevice := s.diskDevicePath(m, boot)

	// 1) Make the boot disk bootable on Linode (virtio/GRUB/fstab/etc).
	if s.cfg.ConvertScript != "" && isBlockDevice(bootDevice) {
		log.Printf("appliance: migration %d: machine conversion on boot disk %s", m.ID, bootDevice)
		// machine-convert.sh requires bash (set -o pipefail, etc). Run it under
		// bash explicitly — invoking via /bin/sh would use dash on Debian/Ubuntu
		// and fail with "Illegal option -o pipefail".
		out, err := exec.CommandContext(ctx, bashPath(), s.cfg.ConvertScript, bootDevice).CombinedOutput()
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
	if !ok || boot.VolumeID == 0 {
		_ = s.st.SetMigrationState(sctx, m.ID, api.MigImageReady,
			"Linode automation not configured; migrated data is on the appliance volumes/files")
		_ = s.st.SetMigrateFinished(sctx, m.ID)
		return
	}

	_ = s.st.AddEvent(sctx, m.ID, "info", "cutover started: converting boot disk and cloning volumes")

	// 2) Clone every disk's volume into an immutable artifact, in disk order.
	cloneIDs := make([]int64, len(m.Disks))
	for i, d := range m.Disks {
		label := volumeLabel(m.Name, d.Index, len(m.Disks)) + "-img"
		if len(label) > 32 {
			label = label[:32]
		}
		log.Printf("appliance: migration %d: cloning disk %d volume %d -> %s", m.ID, d.Index, d.VolumeID, label)
		clone, err := cl.CloneVolume(ctx, d.VolumeID, label)
		if canceled() {
			return
		}
		if err != nil {
			s.fail(m.ID, fmt.Sprintf("clone disk %d volume: %v", d.Index, err))
			return
		}
		if _, err := cl.WaitVolumeActive(ctx, clone.ID, 15*time.Minute); err != nil {
			if canceled() {
				return
			}
			s.fail(m.ID, fmt.Sprintf("wait clone (disk %d) active: %v", d.Index, err))
			return
		}
		cloneIDs[i] = clone.ID
		_ = s.st.SetDiskArtifact(sctx, d.ID, fmt.Sprintf("volume:%d", clone.ID))
	}
	// The boot clone is the migration's headline artifact.
	_ = s.st.SetMigrationImage(sctx, m.ID, fmt.Sprintf("volume:%d", cloneIDs[0]), 0)

	// 3) Optionally launch a new instance booting from the boot artifact with the
	//    data artifacts attached (sda, sdb, …).
	if req.LaunchInstance {
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
		for i, vid := range cloneIDs {
			if err := cl.AttachVolume(ctx, vid, inst.ID); err != nil {
				if canceled() {
					return
				}
				s.fail(m.ID, fmt.Sprintf("attach artifact (disk %d): %v", i, err))
				return
			}
		}
		cfgID, err := cl.CreateConfigBootingVolumes(ctx, inst.ID, cloneIDs, "boot-migrated")
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
		_ = s.st.SetMigrationImage(sctx, m.ID, fmt.Sprintf("volume:%d", cloneIDs[0]), inst.ID)
		_ = s.st.SetMigrationState(sctx, m.ID, api.MigLaunched, "")
		_ = s.st.SetMigrateFinished(sctx, m.ID)
		_ = s.st.AddEvent(sctx, m.ID, "info", fmt.Sprintf("migration complete: launched Linode %d from the image volumes", inst.ID))
		return
	}

	_ = s.st.SetMigrationState(sctx, m.ID, api.MigImageReady, "")
	_ = s.st.SetMigrateFinished(sctx, m.ID)
	_ = s.st.AddEvent(sctx, m.ID, "info", fmt.Sprintf("migration complete: %d image volume(s) ready to launch", len(m.Disks)))
}

func (s *Server) fail(id int64, msg string) {
	log.Printf("appliance: migration %d failed: %s", id, msg)
	_ = s.st.SetMigrationState(context.Background(), id, api.MigFailed, msg)
	_ = s.st.AddEvent(context.Background(), id, "error", "cutover failed: "+msg)
}

// ---- view + validations (aggregate across disks) ----

func (s *Server) view(ctx context.Context, m api.Migration, token string) api.MigrationView {
	v := api.MigrationView{Migration: m, PercentDone: -1, ETASeconds: -1}

	// RPO = worst (largest) lag across disks that have completed a sync.
	var maxLag float64
	for _, d := range m.Disks {
		if !d.LastSyncAt.IsZero() {
			if lag := time.Since(d.LastSyncAt).Seconds(); lag > maxLag {
				maxLag = lag
			}
		}
	}
	v.RPOSeconds = maxLag
	v.Validations = s.validations(m, maxLag)
	v.CanMigrate = allOK(v.Validations) &&
		(m.State == api.MigReplicating || m.State == api.MigReady || m.State == api.MigAwaitingAgent)
	v.Assessed = !m.AssessedAt.IsZero() && v.CanMigrate

	switch m.State {
	case api.MigCreated, api.MigAwaitingAgent:
		v.Phase = "waiting for agent"
	case api.MigReplicating, api.MigReady:
		v.Phase = "replicating"
		// Aggregate initial-sync progress across all disks still baselining.
		var sumWritten, sumTotal int64
		var earliest time.Time
		baselining := false
		for _, d := range m.Disks {
			if d.FullSyncDone {
				if d.TotalBlocks > 0 {
					sumWritten += d.TotalBlocks
					sumTotal += d.TotalBlocks
				}
				continue
			}
			if p, ok := s.progress.Load(d.ID); ok {
				sp := p.(*syncProgress)
				sp.mu.Lock()
				written, total, full, started := sp.written, sp.total, sp.fullSync, sp.started
				sp.mu.Unlock()
				if full && total > 0 {
					baselining = true
					sumWritten += written
					sumTotal += total
					if earliest.IsZero() || started.Before(earliest) {
						earliest = started
					}
				}
			}
		}
		if baselining && sumTotal > 0 {
			v.Phase = "initial sync"
			v.PercentDone = float64(sumWritten) / float64(sumTotal) * 100
			if !earliest.IsZero() && sumWritten > 0 {
				elapsed := time.Since(earliest)
				remain := float64(elapsed) * float64(sumTotal-sumWritten) / float64(sumWritten)
				v.ETASeconds = int64(time.Duration(remain).Seconds())
				v.ElapsedSeconds = int64(elapsed.Seconds())
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
		token, _ = s.st.EnrollToken(ctx, m.ID)
	}
	if token != "" {
		v.EnrollCmd = s.enrollCmd(token, m)
	}
	v.UninstallCmd = s.uninstallCmd()
	return v
}

func (s *Server) validations(m api.Migration, rpoSec float64) []api.ValidationCheck {
	n := len(m.Disks)
	agentsSeen, fullDone, storageOK := 0, 0, 0
	var anySync bool
	for _, d := range m.Disks {
		if !d.AgentLastSeen.IsZero() && time.Since(d.AgentLastSeen) < 5*time.Minute {
			agentsSeen++
		}
		if d.FullSyncDone {
			fullDone++
		}
		if d.VolumeDevice != "" || s.cfg.ApplianceLinodeID == 0 {
			storageOK++
		}
		if !d.LastSyncAt.IsZero() {
			anySync = true
		}
	}
	allAgents := n > 0 && agentsSeen == n
	allFull := n > 0 && fullDone == n
	allStorage := n > 0 && storageOK == n
	lagOK := anySync && allFull && rpoSec <= float64(s.cfg.RPOTargetSec)

	diskWord := func(k int) string { return fmt.Sprintf("%d/%d disks", k, n) }
	return []api.ValidationCheck{
		{Name: "Storage provisioned", OK: allStorage, Detail: diskWord(storageOK) + " ready"},
		{Name: "Agent connected", OK: allAgents, Detail: diskWord(agentsSeen) + " checked in"},
		{Name: "Initial full sync complete", OK: allFull, Detail: diskWord(fullDone) + " baselined"},
		{Name: fmt.Sprintf("Replication lag within %ds", s.cfg.RPOTargetSec), OK: lagOK, Detail: lagDetail2(anySync, rpoSec)},
	}
}

func (s *Server) enrollCmd(token string, m api.Migration) string {
	return fmt.Sprintf("curl -fsSL %s'%s://%s:%d/install/agent.sh?token=%s' | sudo bash",
		s.curlPinFlag(), s.scheme(), s.cfg.PublicHost, s.cfg.ConsolePort, token)
}

// uninstallCmd is the one-liner that removes the agent from a source server.
func (s *Server) uninstallCmd() string {
	return fmt.Sprintf("curl -fsSL %s'%s://%s:%d/install/uninstall.sh' | sudo bash",
		s.curlPinFlag(), s.scheme(), s.cfg.PublicHost, s.cfg.ConsolePort)
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
// CA-chain validation (no public CA) while --pinnedpubkey still requires the
// pinned key, so the connection is MITM-proof. Empty for plain HTTP / external CA.
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

// bashPath resolves a bash interpreter for running machine-convert.sh. It
// prefers $PATH, then falls back to the usual absolute locations, and finally
// to "bash" (letting exec surface a clear not-found error).
func bashPath() string {
	if p, err := exec.LookPath("bash"); err == nil {
		return p
	}
	for _, p := range []string{"/bin/bash", "/usr/bin/bash", "/usr/local/bin/bash"} {
		if info, err := os.Stat(p); err == nil && !info.IsDir() {
			return p
		}
	}
	return "bash"
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

func humanBytes(n int64) string {
	const u = 1 << 10
	if n < u {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(u), 0
	for x := n / u; x >= u; x /= u {
		div *= u
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}

func lagDetail2(anySync bool, rpoSec float64) string {
	if !anySync {
		return "no completed sync yet"
	}
	return fmt.Sprintf("worst lag %.0fs across disks", rpoSec)
}

// friendlyCreateErr turns the SQLite unique-name violation into a clear message.
func friendlyCreateErr(err error) string {
	if err != nil && containsAny(err.Error(), "UNIQUE", "unique") && containsAny(err.Error(), "name") {
		return "a migration with that name already exists — choose a different name"
	}
	return err.Error()
}

func containsAny(s string, subs ...string) bool {
	for _, sub := range subs {
		for i := 0; i+len(sub) <= len(s); i++ {
			if s[i:i+len(sub)] == sub {
				return true
			}
		}
	}
	return false
}
