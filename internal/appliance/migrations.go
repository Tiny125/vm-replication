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
	"strconv"
	"strings"
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
	req.SourceIP = strings.TrimSpace(req.SourceIP)
	if req.SourceIP != "" && !validHost(req.SourceIP) {
		writeErr(w, http.StatusBadRequest, "source IP is not a valid IP address or hostname")
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
	// If anything fails, roll the whole thing back (delete any volumes created
	// and the migration row) so a failed create never leaves a stray migration.
	for _, d := range m.Disks {
		port := s.cfg.BaseReceiverPort + int(d.ID)
		if err := s.st.AssignDiskPort(ctx, d.ID, port); err != nil {
			s.rollbackCreate(ctx, m.ID)
			writeErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		if err := s.provisionDiskStorage(ctx, m, d); err != nil {
			s.rollbackCreate(ctx, m.ID)
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
	label := volumeLabel(m.Name, d.ID)
	vol, err := cl.CreateVolume(ctx, label, s.cfg.Region, sizeGiB, s.cfg.ApplianceLinodeID)
	if err != nil {
		return err
	}
	// Record the volume id immediately (before waiting for it to become active)
	// so that a later failure still lets the rollback / delete path remove it.
	device := vol.FilesystemPath
	if device == "" {
		device = "/dev/disk/by-id/scsi-0Linode_Volume_" + label
	}
	_ = s.st.SetDiskVolume(ctx, d.ID, vol.ID, device)
	if _, err := cl.WaitVolumeActive(ctx, vol.ID, 3*time.Minute); err != nil {
		return err
	}
	_ = s.st.AddEvent(ctx, m.ID, "info", fmt.Sprintf("disk %d (%s): provisioned %dGiB volume %q (id %d)", d.Index, d.SourceDevice, sizeGiB, label, vol.ID))
	return nil
}

// sanitizeLabel keeps only Linode-safe label characters [A-Za-z0-9_-].
func sanitizeLabel(name string) string {
	clean := make([]rune, 0, len(name))
	for _, r := range name {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '_' {
			clean = append(clean, r)
		} else {
			clean = append(clean, '-')
		}
	}
	return string(clean)
}

// volumeLabel builds a UNIQUE Linode-safe replication-volume label
// "vmrep-<name>-<diskID>". The disk id guarantees uniqueness across migrations
// (and against orphaned volumes from a deleted migration of the same name),
// avoiding "Label must be unique". Linode labels are 1–32 chars.
func volumeLabel(name string, diskID int64) string {
	return fitLabel("vmrep-", sanitizeLabel(name), fmt.Sprintf("-%d", diskID))
}

// fitLabel assembles prefix+middle+suffix within Linode's 32-char limit,
// truncating only the middle (name) part.
func fitLabel(prefix, middle, suffix string) string {
	room := 32 - len(prefix) - len(suffix)
	if room < 0 {
		room = 0
	}
	if len(middle) > room {
		middle = middle[:room]
	}
	return prefix + middle + suffix
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
	// Readiness is auto-computed: cutover is allowed once every disk's initial
	// full sync is complete. No manual assessment, and the live agent/lag don't
	// gate it (so a previously-failed cutover can be retried).
	if !cutoverReady(m) {
		writeErr(w, http.StatusConflict, "the initial full sync must finish on all disks before cutover")
		return
	}
	if m.State == api.MigFailed {
		_ = s.st.AddEvent(ctx, id, "info", "retrying cutover on the already-replicated data")
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
	s.cleanupMigrationResources(ctx, m)
	if err := s.st.DeleteMigration(ctx, id); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// rollbackCreate undoes a failed migration creation: it re-loads the migration
// (to pick up any volume ids recorded so far), deletes those resources, and
// removes the migration row so nothing lingers in the console.
func (s *Server) rollbackCreate(ctx context.Context, id int64) {
	if m, err := s.st.Migration(ctx, id); err == nil {
		s.cleanupMigrationResources(ctx, m)
	}
	_ = s.st.DeleteMigration(ctx, id)
}

// cleanupMigrationResources stops a migration's receivers/finalize and removes
// its replication volumes (or file-fallback images) and manifests. It does NOT
// delete the migration row — the caller decides that. Safe to call repeatedly.
func (s *Server) cleanupMigrationResources(ctx context.Context, m api.Migration) {
	s.recMu.Lock()
	if cancel := s.finalizes[m.ID]; cancel != nil {
		cancel()
		delete(s.finalizes, m.ID)
	}
	s.recMu.Unlock()
	s.stopReceivers(m)

	cl, haveLinode := s.linodeClient(ctx)
	// Also remove anything created by a cutover: the launched instance and the
	// cloned <name>-cutover artifact volumes.
	if haveLinode && m.LaunchedID != 0 {
		if err := cl.DeleteInstance(ctx, m.LaunchedID); err != nil {
			log.Printf("appliance: delete cutover Linode %d failed (remove it in Cloud Manager): %v", m.LaunchedID, err)
		}
	}
	for _, d := range m.Disks {
		s.progress.Delete(d.ID)
		if haveLinode {
			if avid := artifactVolumeID(d.ArtifactID); avid != 0 {
				_ = cl.DetachVolume(ctx, avid)
				for i := 0; i < 10; i++ {
					if cl.DeleteVolume(ctx, avid) == nil {
						break
					}
					time.Sleep(2 * time.Second)
				}
			}
		}
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

	// 1) Make the boot disk bootable on Linode (virtio/GRUB/fstab/etc). This is
	//    best-effort: if it fails we still clone the replicated data into an image
	//    volume so the operator can attach it to a Linode and finish by hand,
	//    rather than being stranded with a failed migration and no artifact.
	//    kernel/rootDevice default to the GRUB2 path; the convert script tells us
	//    (via "vmrepl-layout:") when the disk is a partitionless whole-disk root,
	//    which must boot via a Linode-supplied kernel instead.
	kernel, rootDevice := "linode/grub2", "/dev/sda"
	if s.cfg.ConvertScript != "" && isBlockDevice(bootDevice) {
		log.Printf("appliance: migration %d: machine conversion on boot disk %s", m.ID, bootDevice)
		// machine-convert.sh requires bash (set -o pipefail, etc). Run it under
		// bash explicitly — invoking via /bin/sh would use dash on Debian/Ubuntu
		// and fail with "Illegal option -o pipefail".
		out, err := exec.CommandContext(ctx, bashPath(), s.cfg.ConvertScript, bootDevice).CombinedOutput()
		if canceled() {
			return
		}
		if strings.Contains(string(out), "vmrepl-layout: wholedisk") {
			// Partitionless filesystem: no on-disk bootloader, so boot with a
			// Linode kernel that mounts the whole volume as root.
			kernel, rootDevice = "linode/latest-64bit", "/dev/sda"
		}
		if err != nil {
			log.Printf("appliance: migration %d: machine-convert failed (continuing best-effort): %v\n%s", m.ID, err, trimOut(out))
			_ = s.st.AddEvent(sctx, m.ID, "warn", "boot disk conversion did not complete ("+oneLine(err.Error())+"); the image volume is still being created — it may need manual GRUB/virtio fixup before it boots (see docs/CUTOVER.md). Detail: "+oneLine(trimOut(out)))
		} else {
			_ = s.st.AddEvent(sctx, m.ID, "info", fmt.Sprintf("boot disk converted for Linode (virtio, network); boot kernel %s root %s", kernel, rootDevice))
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

	// On a retry, remove any instance/volumes left over from the previous cutover
	// attempt so we start clean and the <name>-cutover labels are free to reuse.
	s.cleanupCutoverArtifacts(sctx, m)

	// 2) Clone every disk's volume into a launchable <name>-cutover artifact.
	cloneIDs := make([]int64, len(m.Disks))
	for i, d := range m.Disks {
		label := cutoverVolumeLabel(m.Name, d.Index, len(m.Disks))
		log.Printf("appliance: migration %d: cloning disk %d volume %d -> %s", m.ID, d.Index, d.VolumeID, label)
		clone, err := cl.CloneVolume(ctx, d.VolumeID, label)
		if canceled() {
			return
		}
		if err != nil {
			s.fail(m.ID, fmt.Sprintf("clone disk %d into %s: %v", d.Index, label, err))
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
		_ = s.st.AddEvent(sctx, m.ID, "info", fmt.Sprintf("disk %d cloned into cutover volume %q (id %d)", d.Index, label, clone.ID))
	}
	// The boot clone is the migration's headline artifact.
	_ = s.st.SetMigrationImage(sctx, m.ID, fmt.Sprintf("volume:%d", cloneIDs[0]), 0)

	// 3) Optionally launch a <name>-cutover instance booting from the boot
	//    artifact with the data artifacts attached (sda, sdb, …).
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
		label := orDefault(req.Label, cutoverName(m.Name))
		inst, err := cl.CreateInstance(ctx, label, region, typ)
		if canceled() {
			return
		}
		if err != nil {
			s.fail(m.ID, "create instance: "+err.Error())
			return
		}
		// Record the instance id immediately so a failure below still lets a
		// retry (or delete) clean it up.
		_ = s.st.SetMigrationImage(sctx, m.ID, fmt.Sprintf("volume:%d", cloneIDs[0]), inst.ID)
		_ = s.st.AddEvent(sctx, m.ID, "info", fmt.Sprintf("created cutover Linode %q (id %d); building boot config", label, inst.ID))
		// Create the boot config that references the cloned volumes as devices;
		// this is what associates the volumes with the new instance. (Attaching a
		// volume before any config exists fails with "couldn't choose a
		// configuration profile to add this volume to".)
		cfgID, err := cl.CreateConfigBootingVolumes(ctx, inst.ID, cloneIDs, "boot-migrated", kernel, rootDevice)
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
		_ = s.st.SetMigrationState(sctx, m.ID, api.MigLaunched, "")
		_ = s.st.SetMigrateFinished(sctx, m.ID)
		_ = s.st.AddEvent(sctx, m.ID, "info", fmt.Sprintf("migration complete: launched cutover Linode %q (id %d) from %d volume(s)", label, inst.ID, len(cloneIDs)))
		return
	}

	_ = s.st.SetMigrationState(sctx, m.ID, api.MigImageReady, "")
	_ = s.st.SetMigrateFinished(sctx, m.ID)
	_ = s.st.AddEvent(sctx, m.ID, "info", fmt.Sprintf("migration complete: %d cutover volume(s) ready to launch", len(m.Disks)))
}

// cleanupCutoverArtifacts deletes the Linode instance and cloned volumes created
// by a previous cutover attempt (recorded on the migration), logging each step,
// then clears those references. No-op on a first cutover.
func (s *Server) cleanupCutoverArtifacts(ctx context.Context, m api.Migration) {
	cl, ok := s.linodeClient(ctx)
	if !ok {
		return
	}
	if m.LaunchedID != 0 {
		_ = s.st.AddEvent(ctx, m.ID, "info", fmt.Sprintf("removing previous cutover Linode %d before retrying", m.LaunchedID))
		if err := cl.DeleteInstance(ctx, m.LaunchedID); err != nil {
			_ = s.st.AddEvent(ctx, m.ID, "warn", fmt.Sprintf("could not delete previous cutover Linode %d (remove it in Cloud Manager): %v", m.LaunchedID, err))
		}
	}
	for _, d := range m.Disks {
		vid := artifactVolumeID(d.ArtifactID)
		if vid == 0 {
			continue
		}
		_ = s.st.AddEvent(ctx, m.ID, "info", fmt.Sprintf("removing previous cutover volume %d (disk %d) before retrying", vid, d.Index))
		_ = cl.DetachVolume(ctx, vid)
		var derr error
		for i := 0; i < 10; i++ {
			if derr = cl.DeleteVolume(ctx, vid); derr == nil {
				break
			}
			time.Sleep(2 * time.Second)
		}
		if derr != nil {
			_ = s.st.AddEvent(ctx, m.ID, "warn", fmt.Sprintf("could not delete previous cutover volume %d: %v", vid, derr))
		}
		_ = s.st.SetDiskArtifact(ctx, d.ID, "")
	}
	_ = s.st.SetMigrationImage(ctx, m.ID, "", 0)
}

// artifactVolumeID parses "volume:<id>" → id (0 if not a volume artifact).
func artifactVolumeID(artifact string) int64 {
	if !strings.HasPrefix(artifact, "volume:") {
		return 0
	}
	id, _ := strconv.ParseInt(strings.TrimPrefix(artifact, "volume:"), 10, 64)
	return id
}

// cutoverName is the instance label for a cutover launch: "<name>-cutover".
func cutoverName(name string) string { return sanitizeLabel(name) + "-cutover" }

// cutoverVolumeLabel is the cloned-volume label: "<name>-cutover" (with a disk
// suffix for multi-disk), capped to Linode's 32-char limit. Deterministic so a
// retry reuses the same label after the old volume is deleted.
func cutoverVolumeLabel(name string, idx, total int) string {
	suffix := "-cutover"
	if total > 1 {
		suffix = fmt.Sprintf("-cutover-%d", idx)
	}
	return fitLabel("", sanitizeLabel(name), suffix)
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
	// Cutover readiness depends only on the initial full sync (and storage) being
	// done — not on the live agent connection or replication lag, which are just
	// informational once baselined and would otherwise wrongly block a retry
	// (after a cutover the agent stops replicating, so lag/agent always "fail").
	v.CanMigrate = cutoverReady(m)
	v.Assessed = v.CanMigrate // no separate manual assessment; readiness is auto-computed

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

	// Reflect readiness in the displayed status so the operator can see at a
	// glance when it's safe to cut over: a replicating migration whose disks are
	// all baselined and within the RPO lag shows as "ready to cut over". This is
	// presentational and recomputed on every poll, so if lag grows again it
	// reverts to "replicating" automatically.
	if v.CanMigrate && (m.State == api.MigReplicating || m.State == api.MigAwaitingAgent) {
		v.Migration.State = api.MigReady
	}
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

// cutoverReady reports whether a migration can be cut over: every disk's initial
// full sync is complete and its storage is provisioned. It is independent of
// migration state, so a previously-failed migration can be retried, and of the
// live agent/lag, which stop mattering once the baseline exists.
func cutoverReady(m api.Migration) bool {
	if len(m.Disks) == 0 {
		return false
	}
	for _, d := range m.Disks {
		if !d.FullSyncDone {
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

// oneLine collapses whitespace/newlines to single spaces and caps the length,
// so multi-line command output fits cleanly into one activity-log entry.
func oneLine(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	const max = 400
	if len(s) > max {
		s = s[:max] + "…"
	}
	return s
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
