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
	"github.com/tiny125/vm-replication/internal/linode"
	"github.com/tiny125/vm-replication/internal/protocol"
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
			// Quiesce-failure report (not a data session): the agent tried and could
			// not capture a consistent image for cutover. Record it so quiesceForCutover
			// fails fast with the agent's actual reason instead of waiting out its timeout.
			if st.Hello.QuiesceError != "" {
				if s.wantDiskConsistency(diskID) {
					s.markDiskQuiesceFailed(diskID, st.Hello.QuiesceError)
					_ = s.st.AddEvent(s.ctx, migID, "warn", fmt.Sprintf("disk %d (%s): the source could not be quiesced for a consistent cutover — %s", diskIdx, dev0, st.Hello.QuiesceError))
				}
				return
			}
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
			// A crash-consistent pass landed: record it so a pending cutover can stop
			// waiting and clone a point-in-time image.
			if st.Hello.Consistent && s.wantDiskConsistency(diskID) {
				s.markDiskConsistent(diskID)
				_ = s.st.AddEvent(s.ctx, migID, "info", fmt.Sprintf("disk %d (%s): crash-consistent snapshot captured for cutover", diskIdx, dev0))
			}
			if st.Hello.FullSync && !wasBaselined {
				_ = s.st.AddEvent(s.ctx, migID, "info", fmt.Sprintf("disk %d (%s): initial full sync complete (%s)", diskIdx, st.Hello.DevicePath, humanBytes(st.BlocksWritten*int64(st.Hello.BlockSize))))
			} else if !wasBaselined {
				_ = s.st.AddEvent(s.ctx, migID, "info", fmt.Sprintf("disk %d: agent connected, replicating", diskIdx))
			}
		}, onProgress, func(serr error) {
			// The receiver port is publicly reachable, so internet scanners and other
			// non-agent clients connect and fail the TLS 1.3 + mutual-cert handshake.
			// Those are connection noise, NOT a replication failure — logging them as a
			// migration error raises a false "replication attempt failed" alarm on a
			// healthy migration. Keep them in the system log only.
			if isNonAgentHandshake(serr) {
				log.Printf("appliance: migration %d disk %d: ignored non-agent connection on receiver port: %v", migID, diskID, serr)
				return
			}
			_ = s.st.RecordDiskError(s.ctx, diskID, serr.Error())
			_ = s.st.AddEvent(s.ctx, migID, "error", fmt.Sprintf("disk %d (%s): replication attempt failed: %s", diskIdx, dev0, serr.Error()))
		}, func(h protocol.Hello) bool {
			// Bounce a live pass into a crash-consistent re-read while we're quiescing
			// this disk for cutover (but never re-request a pass already consistent).
			return s.wantDiskConsistency(diskID) && !h.Consistent
		})
		if err != nil && ctx.Err() == nil {
			log.Printf("appliance: receiver (migration %d disk %d) stopped: %v", migID, diskID, err)
		}
	}()
	return nil
}

// ---- crash-consistent cutover coordination ----
//
// To boot cleanly, the launched instance must come from a crash-consistent
// point-in-time image rather than a multi-minute "smear" of a live disk. So
// steady-state replication stays live (no source downtime), and only at cutover
// do we ask each disk's agent for ONE point-in-time (LVM/fsfreeze) pass. These
// helpers track that request/acknowledgement per disk.

// requestDiskConsistency flags a disk so its receiver bounces the next live pass
// and asks the agent to re-read crash-consistently. Clears any prior "done" mark.
func (s *Server) requestDiskConsistency(diskID int64) {
	s.recMu.Lock()
	s.consistReq[diskID] = true
	s.consistDone[diskID] = false
	delete(s.quiesceErr, diskID) // a fresh request clears any prior failure report
	s.recMu.Unlock()
}

// wantDiskConsistency reports whether we're still asking this disk's agent for a
// crash-consistent pass.
func (s *Server) wantDiskConsistency(diskID int64) bool {
	s.recMu.Lock()
	defer s.recMu.Unlock()
	return s.consistReq[diskID]
}

// markDiskConsistent records that a crash-consistent sync landed for this disk.
// It deliberately leaves the request set so any further (live) pass before we
// stop the receiver is still bounced — the last applied image stays consistent.
func (s *Server) markDiskConsistent(diskID int64) {
	s.recMu.Lock()
	s.consistDone[diskID] = true
	s.recMu.Unlock()
}

// diskConsistencyDone reports whether a crash-consistent sync has landed.
func (s *Server) diskConsistencyDone(diskID int64) bool {
	s.recMu.Lock()
	defer s.recMu.Unlock()
	return s.consistDone[diskID]
}

// markDiskQuiesceFailed records that a disk's agent reported it could not capture a
// consistent image for cutover (with the reason), so the wait can stop immediately.
func (s *Server) markDiskQuiesceFailed(diskID int64, reason string) {
	s.recMu.Lock()
	s.quiesceErr[diskID] = reason
	s.recMu.Unlock()
}

// diskQuiesceFailed returns the reported quiesce-failure reason for a disk, if any.
func (s *Server) diskQuiesceFailed(diskID int64) (string, bool) {
	s.recMu.Lock()
	defer s.recMu.Unlock()
	r, ok := s.quiesceErr[diskID]
	return r, ok
}

// clearConsistency forgets all consistency state for a migration's disks.
func (s *Server) clearConsistency(m api.Migration) {
	s.recMu.Lock()
	for _, d := range m.Disks {
		delete(s.consistReq, d.ID)
		delete(s.consistDone, d.ID)
		delete(s.quiesceErr, d.ID)
	}
	s.recMu.Unlock()
}

// consistencyWait bounds how long cutover waits for a crash-consistent snapshot
// from the source. The agent timer fires ~every 60s and the snapshot read takes
// a little longer, so this comfortably covers a normal pass; on timeout we fall
// back to cloning the current replicated data (with a clear warning).
const consistencyWait = 6 * time.Minute

// quiesceForCutover asks every disk's agent for one crash-consistent (point-in-
// time) snapshot pass and waits for them to land, so the cloned image is a
// single instant — the key to a clean boot. If no agent is checking in, or the
// agents are an older build that ignores the request, it warns and proceeds
// with the current data.
// quiesceForCutover returns true only if every disk delivered a crash-consistent
// pass (so the cloned image is a clean point-in-time). It returns false when there
// is no agent to ask or the wait times out — the caller proceeds on the current
// data, which may be inconsistent.
func (s *Server) quiesceForCutover(ctx context.Context, m api.Migration) bool {
	anyAgent := false
	for _, d := range m.Disks {
		if !d.AgentLastSeen.IsZero() && time.Since(d.AgentLastSeen) < 5*time.Minute {
			anyAgent = true
		}
		s.requestDiskConsistency(d.ID)
	}
	if !anyAgent {
		_ = s.st.AddEvent(s.ctx, m.ID, "warn", "cutover: no source agent has checked in recently, so a fresh crash-consistent snapshot can't be taken; cloning the current replicated data as-is (it may be inconsistent if the source was changing)")
		return false
	}
	_ = s.st.AddEvent(s.ctx, m.ID, "info", "cutover: quiescing the source for a crash-consistent point-in-time snapshot before launch — this can take a while (often several minutes, and longer for busy or large disks)")

	deadline := time.Now().Add(consistencyWait)
	for {
		if ctx.Err() != nil {
			return false
		}
		// Fail fast if any agent has actively reported it cannot quiesce — no point
		// waiting out the timeout when we already know a consistent image won't come.
		for _, d := range m.Disks {
			if reason, failed := s.diskQuiesceFailed(d.ID); failed {
				_ = s.st.AddEvent(s.ctx, m.ID, "warn", "cutover: the source reported it cannot be quiesced for a consistent image ("+reason+"). Stop the source's apps/services so its root can be remounted read-only, then start the cutover again.")
				return false
			}
		}
		allDone := true
		for _, d := range m.Disks {
			if !s.diskConsistencyDone(d.ID) {
				allDone = false
				break
			}
		}
		if allDone {
			_ = s.st.AddEvent(s.ctx, m.ID, "info", "cutover: crash-consistent snapshot captured on all disks; converting and cloning the point-in-time image")
			return true
		}
		if time.Now().After(deadline) {
			_ = s.st.AddEvent(s.ctx, m.ID, "warn", "cutover: timed out waiting for a crash-consistent snapshot from the source (the agent may be an older build, or offline, or the root could not be quiesced — stop the source's apps so its root can be remounted read-only); proceeding with the current replicated data, which may be inconsistent")
			return false
		}
		select {
		case <-ctx.Done():
			return false
		case <-time.After(3 * time.Second):
		}
	}
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
	req.SourceHostname = strings.TrimSpace(req.SourceHostname)
	if req.SourceHostname == "" || !validHost(req.SourceHostname) {
		writeErr(w, http.StatusBadRequest, "source hostname is not a valid hostname (letters, digits, dots and hyphens only) — fix it and create again")
		return
	}
	req.Devices = devices

	ctx := r.Context()
	// A migration provisions Linode volumes and later needs the token again to
	// remove them on delete, so require a present, working token up front (in
	// automation mode). This also fails fast on a revoked/insufficient token
	// instead of half-creating a migration that can't provision storage.
	if s.cfg.ApplianceLinodeID != 0 {
		cl, ok := s.linodeClient(ctx)
		if !ok {
			writeErr(w, http.StatusBadRequest, "add a valid Linode API token in Settings before creating a migration — the appliance needs it to provision storage now and to remove the volumes when you delete the migration")
			return
		}
		if _, err := cl.GetProfile(ctx); err != nil {
			writeErr(w, http.StatusBadGateway, "the stored Linode API token is not working (revoked, or missing Linodes + Volumes read/write) — update it in Settings before creating a migration: "+err.Error())
			return
		}
	}
	// Resolve the boot target and chosen plan. The console offers a plan picker
	// for both modes; if it sends an explicit linode_type we validate it, else we
	// fall back (volume: launch default at cutover; disk: smallest plan that fits).
	if req.PlanClass != "dedicated" {
		req.PlanClass = "shared"
	}
	var total int64
	for _, d := range devices {
		total += d.SizeBytes
	}
	switch req.BootTarget {
	case "", api.BootTargetVolume:
		req.BootTarget = api.BootTargetVolume
		// Validate the picked plan exists, when we have a token to check against.
		if req.LinodeType != "" {
			if cl, ok := s.linodeClient(ctx); ok {
				types, err := cl.ListTypes(ctx)
				if err != nil {
					writeErr(w, http.StatusBadGateway, "could not load Linode plans: "+err.Error())
					return
				}
				if linode.TypeDiskMB(types, req.LinodeType) == 0 {
					writeErr(w, http.StatusBadRequest, "the selected Linode plan is not valid — pick one from the list")
					return
				}
			}
		}
	case api.BootTargetDisk:
		if len(devices) > 1 {
			writeErr(w, http.StatusBadRequest, "local-disk boot currently supports a single disk; use Separate-volume boot for multi-disk migrations")
			return
		}
		cl, ok := s.linodeClient(ctx)
		if !ok {
			writeErr(w, http.StatusBadRequest, "local-disk boot needs a valid Linode API token (to size the plan) — add one in Settings")
			return
		}
		types, err := cl.ListTypes(ctx)
		if err != nil {
			writeErr(w, http.StatusBadGateway, "could not load Linode plans: "+err.Error())
			return
		}
		if req.LinodeType != "" {
			// Honour the explicit pick, but it must be in-class and large enough.
			var chosen *linode.LinodeType
			for i := range types {
				if types[i].ID == req.LinodeType {
					chosen = &types[i]
					break
				}
			}
			if chosen == nil || !linode.PlanClasses(req.PlanClass)[chosen.Class] {
				writeErr(w, http.StatusBadRequest, "the selected plan is not a "+req.PlanClass+" plan — pick one from the list")
				return
			}
			if int64(chosen.DiskMB)*1024*1024 < total {
				writeErr(w, http.StatusBadRequest, fmt.Sprintf("the selected plan's %d GB disk is too small for %s of data — pick a larger plan", chosen.DiskMB/1024, humanBytes(total)))
				return
			}
		} else {
			plan, ok := linode.ClosestType(types, req.PlanClass, total)
			if !ok {
				writeErr(w, http.StatusBadRequest, fmt.Sprintf("no %s Linode plan has enough local disk for %s of data — choose a larger source or use Separate-volume boot", req.PlanClass, humanBytes(total)))
				return
			}
			req.LinodeType = plan.ID
		}
	default:
		writeErr(w, http.StatusBadRequest, "boot_target must be 'volume' or 'disk'")
		return
	}
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
	if m.BootTarget == api.BootTargetDisk {
		_ = s.st.AddEvent(ctx, m.ID, "info", fmt.Sprintf("boot target: Linode local disk (%s plan %s)", m.PlanClass, m.LinodeType))
	} else if m.LinodeType != "" {
		_ = s.st.AddEvent(ctx, m.ID, "info", fmt.Sprintf("boot target: separate Block Storage volume; launch plan %s", m.LinodeType))
	} else {
		_ = s.st.AddEvent(ctx, m.ID, "info", "boot target: separate Block Storage volume")
	}
	_ = s.st.SetMigrationState(ctx, m.ID, api.MigAwaitingAgent, "")
	m, _ = s.st.Migration(ctx, m.ID)
	if err := s.ensureReceivers(m); err != nil {
		log.Printf("appliance: ensureReceivers: %v", err)
	}
	writeJSON(w, http.StatusCreated, s.view(ctx, m, token))
}

// handleLinodePlans returns the Linode plans usable for local-disk boot, grouped
// to "shared"/"dedicated", so the console can show the closest-fit plan for the
// entered disk size.
func (s *Server) handleLinodePlans(w http.ResponseWriter, r *http.Request) {
	cl, ok := s.linodeClient(r.Context())
	if !ok {
		writeErr(w, http.StatusBadRequest, "add a valid Linode API token in Settings to load plans")
		return
	}
	types, err := cl.ListTypes(r.Context())
	if err != nil {
		writeErr(w, http.StatusBadGateway, "could not load Linode plans: "+err.Error())
		return
	}
	plans := []api.LinodePlan{}
	for _, t := range types {
		var class string
		switch {
		case linode.PlanClasses("shared")[t.Class]:
			class = "shared"
		case linode.PlanClasses("dedicated")[t.Class]:
			class = "dedicated"
		default:
			continue // skip gpu/highmem/premium for now
		}
		plans = append(plans, api.LinodePlan{
			ID: t.ID, Label: t.Label, Class: class,
			DiskGB: t.DiskMB / 1024, MemoryMB: t.MemoryMB, VCPUs: t.VCPUs,
			PriceMonthly: t.Price.Monthly,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"plans": plans})
}

// provisionDiskStorage creates and attaches a Linode volume for one disk when
// automation is configured; otherwise it's a no-op (file fallback).
func (s *Server) provisionDiskStorage(ctx context.Context, m api.Migration, d api.Disk) error {
	cl, ok := s.linodeClient(ctx)
	if !ok || s.cfg.ApplianceLinodeID == 0 {
		return nil // file fallback
	}
	// Size the volume to exactly the entered size, rounded up to whole GiB. The
	// console form computes size_bytes as <GB input> * 2^30, so this matches the
	// operator's input 1:1; the receiver only requires target >= source, and an
	// equal-size volume satisfies that. Sizes of 1–19 GiB are bumped to 20 GiB
	// (small-volume floor; Linode's own minimum is 10).
	sizeGiB := int((d.SizeBytes + (1 << 30) - 1) / (1 << 30))
	if sizeGiB < 20 {
		sizeGiB = 20
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
	// Receivers stay up so finalize can take a final crash-consistent snapshot
	// from the source before it stops them and clones the image.

	finCtx, cancel := context.WithCancel(s.ctx)
	s.recMu.Lock()
	s.finalizes[id] = cancel
	s.recMu.Unlock()
	go s.finalize(finCtx, m, req)
	writeJSON(w, http.StatusAccepted, map[string]string{"status": "migrating"})
}

// handleCompleteCutover runs phase 2 of a guided cutover: after the operator has
// powered off the source, convert + clone + launch using the options captured in
// phase 1. Only valid while the migration is paused in MigAwaitingCutover.
func (s *Server) handleCompleteCutover(w http.ResponseWriter, r *http.Request) {
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
	if m.State != api.MigAwaitingCutover {
		writeErr(w, http.StatusConflict, "migration is not awaiting cutover completion")
		return
	}
	s.recMu.Lock()
	req, have := s.pendingCutover[id]
	s.recMu.Unlock()
	if !have {
		// Lost across a restart — fall back to a fresh request body (or defaults).
		_ = json.NewDecoder(r.Body).Decode(&req)
	}
	if err := s.st.SetMigrationState(ctx, id, api.MigMigrating, ""); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	_ = s.st.AddEvent(ctx, id, "info", "cutover: operator confirmed — completing (convert, clone, launch)")
	finCtx, cancel := context.WithCancel(s.ctx)
	s.recMu.Lock()
	s.finalizes[id] = cancel
	s.recMu.Unlock()
	go s.finalizeComplete(finCtx, m, req)
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
	if m.State != api.MigMigrating && m.State != api.MigAwaitingCutover {
		writeErr(w, http.StatusConflict, "migration is not running")
		return
	}
	s.recMu.Lock()
	cancel := s.finalizes[id]
	delete(s.finalizes, id)
	delete(s.pendingCutover, id)
	s.recMu.Unlock()
	if cancel != nil {
		cancel()
	}
	s.clearConsistency(m) // drop any pending crash-consistent cutover request
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
	s.cleanupMigrationResources(ctx, m, true) // keep (detach) the replication volume
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
		s.cleanupMigrationResources(ctx, m, false) // failed create: delete everything
	}
	_ = s.st.DeleteMigration(ctx, id)
}

// cleanupMigrationResources stops a migration's receivers/finalize and removes
// its replication volumes (or file-fallback images) and manifests. It does NOT
// delete the migration row — the caller decides that. Safe to call repeatedly.
// cleanupMigrationResources stops a migration's receivers/finalize, deletes the
// cutover artifacts (launched instance + <name>-cutover clone volumes), and
// handles the replication volume. When keepReplVolume is true (user delete) the
// vrep-<name> replication volume is only DETACHED — kept in the account so the
// operator can still reference it — otherwise it is deleted (failed-create
// rollback). File-fallback images are always removed.
func (s *Server) cleanupMigrationResources(ctx context.Context, m api.Migration, keepReplVolume bool) {
	s.recMu.Lock()
	if cancel := s.finalizes[m.ID]; cancel != nil {
		cancel()
		delete(s.finalizes, m.ID)
	}
	s.recMu.Unlock()
	s.stopReceivers(m)

	cl, haveLinode := s.linodeClient(ctx)
	// Always remove cutover artifacts: the launched instance and clone volumes.
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
			_ = cl.DetachVolume(ctx, d.VolumeID) // always detach from the appliance
			if !keepReplVolume {
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
				log.Printf("appliance: migration %d: replication volume %d detached and kept for reference", m.ID, d.VolumeID)
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

	// 0) Quiesce for a crash-consistent cutover: ask each disk's agent for one
	//    final point-in-time snapshot pass so the cloned image reflects a single
	//    instant and boots cleanly, then stop the receivers.
	//
	//    "Skip snapshot" is the operator's explicit promise that the replicated data
	//    is ALREADY consistent — the source is powered off, or its root filesystem
	//    has been quiesced (e.g. `mount -o remount,ro /`). We honour it as given.
	//    Note: the automatic snapshot only works when the source root is on LVM; a
	//    plain whole-disk source (a typical cloud image) has no point-in-time
	//    mechanism, so quiesceForCutover will warn and proceed on live data. For such
	//    sources, quiesce the source first and use skip — see docs/CUTOVER.md.
	consistent := false
	if req.SkipSnapshot {
		_ = s.st.AddEvent(sctx, m.ID, "info", "cutover: skipping the point-in-time snapshot at the operator's request — the source is reported powered off or quiesced, so the current replicated data is treated as consistent. (If the source was still running and writing, the image may be inconsistent.)")
		consistent = true // operator's assertion
	} else if !s.sourceAgentActive(m) {
		// No agent checked in — the source appears powered off, so the current
		// replicated data is already a static, consistent point-in-time.
		_ = s.st.AddEvent(sctx, m.ID, "info", "cutover: the source agent is not checked in (the source appears powered off), so the current replicated data is treated as a consistent point-in-time")
		consistent = true
	} else {
		consistent = s.quiesceForCutover(ctx, m)
	}
	if canceled() {
		return
	}

	// Guided cutover must NOT proceed on an inconsistent image. If the source was
	// still running but couldn't be quiesced (apps holding the root, or an outdated
	// agent), fail fast WITHOUT stopping replication, so the operator can fix it and
	// retry rather than ending up at a grub> prompt.
	if req.GuidedShutdown && !consistent {
		s.clearConsistency(m)
		_ = s.st.SetMigrationState(sctx, m.ID, api.MigReplicating, "")
		_ = s.st.AddEvent(sctx, m.ID, "warn", "cutover: could NOT capture a consistent image — the source root could not be remounted read-only (stop the source's apps/services so nothing is writing to / ), or the source agent predates this feature (re-enroll the source). Replication is still running; fix the above and start the cutover again.")
		return
	}

	s.stopReceivers(m)
	s.clearConsistency(m)

	if req.GuidedShutdown {
		// Guided cutover: a consistent point-in-time image is now frozen (receivers
		// stopped). Pause for the operator to power the source off (verified by the
		// agent going silent), then resume via /complete. Stash the request so phase
		// 2 reuses the options.
		s.recMu.Lock()
		s.pendingCutover[m.ID] = req
		s.recMu.Unlock()
		_ = s.st.SetMigrationState(sctx, m.ID, api.MigAwaitingCutover, "")
		_ = s.st.AddEvent(sctx, m.ID, "info", "cutover: consistent point-in-time image captured and frozen. Now POWER OFF the source server; once the appliance confirms the source agent has stopped, click \"Complete cutover\" to convert, clone and launch.")
		return
	}
	s.finalizeComplete(ctx, m, req)
}

// finalizeComplete is cutover phase 2: convert the boot disk, clone every disk
// into launchable artifacts, and (for disk-boot) stream onto the local disk. It
// runs inline after phase 1 for a normal cutover, or when the operator confirms a
// guided cutover via /complete (after powering off the source). The consistent
// image is already captured and the receivers are stopped by phase 1.
func (s *Server) finalizeComplete(ctx context.Context, m api.Migration, req api.FinalizeRequest) {
	defer func() {
		s.recMu.Lock()
		delete(s.finalizes, m.ID)
		delete(s.pendingCutover, m.ID)
		s.recMu.Unlock()
	}()
	canceled := func() bool { return ctx.Err() != nil }
	sctx := s.ctx

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
	convertFailed := false
	convertEnvIssue := false // failure was a missing command / bad env, not inconsistent data
	if s.cfg.ConvertScript != "" && isBlockDevice(bootDevice) {
		log.Printf("appliance: migration %d: machine conversion on boot disk %s", m.ID, bootDevice)
		// machine-convert.sh requires bash (set -o pipefail, etc). Run it under
		// bash explicitly — invoking via /bin/sh would use dash on Debian/Ubuntu
		// and fail with "Illegal option -o pipefail".
		cmd := exec.CommandContext(ctx, bashPath(), s.cfg.ConvertScript, bootDevice)
		// Pass any console/SSH access the operator entered via the environment (not
		// argv) so the secrets don't show up in `ps`. The script seeds them into the
		// migrated image's root account inside the chroot.
		cmd.Env = os.Environ()
		if req.RootPassword != "" {
			cmd.Env = append(cmd.Env, "VMREPL_ROOT_PASSWORD="+req.RootPassword)
		}
		if req.SSHAuthorizedKey != "" {
			cmd.Env = append(cmd.Env, "VMREPL_SSH_AUTHORIZED_KEY="+req.SSHAuthorizedKey)
		}
		if m.BootTarget == api.BootTargetDisk {
			// Bake in the one-shot that, on the volume-boot phase, copies the image
			// onto the instance's local disk (see finalizeDisk).
			cmd.Env = append(cmd.Env, "VMREPL_DISK_INSTALL=1")
			// The plan's local disk is a little smaller than a same-sized volume
			// (Linode reserves a sliver), so a 1:1 whole-disk copy of an image that
			// equals the plan size won't fit. Tell convert to shrink a whole-disk
			// ext filesystem to the plan disk minus a safety margin so it fits.
			shrinkReq := false
			if cl, ok := s.linodeClient(sctx); ok {
				if types, terr := cl.ListTypes(sctx); terr == nil {
					if mb := linode.TypeDiskMB(types, m.LinodeType); mb > 256 {
						cmd.Env = append(cmd.Env, fmt.Sprintf("VMREPL_SHRINK_MB=%d", mb-128))
						shrinkReq = true
						_ = s.st.AddEvent(sctx, m.ID, "info", fmt.Sprintf("cutover: requesting whole-disk ext shrink to %d MiB so the image fits the %s local disk (%d MiB)", mb-128, m.LinodeType, mb))
					}
				}
			}
			if !shrinkReq {
				_ = s.st.AddEvent(sctx, m.ID, "warn", "cutover: could not determine the plan's local disk size, so the image filesystem will not be shrunk — if the copy reports it does not fit, retry once the appliance can reach the Linode API, or use a larger plan")
			}
		}
		out, err := cmd.CombinedOutput()
		if canceled() {
			return
		}
		if strings.Contains(string(out), "vmrepl-layout: wholedisk") {
			// Partitionless filesystem: no on-disk bootloader, so boot with a
			// Linode kernel that mounts the whole volume as root.
			kernel = "linode/latest-64bit"
		}
		// Use the exact root device the convert script detected (e.g. /dev/sda1
		// for a partitioned disk) — booting a partitioned disk with root_device
		// /dev/sda panics with "unable to mount root fs".
		if rd := convertField(string(out), "vmrepl-root:"); rd != "" {
			rootDevice = rd
		}
		if err != nil {
			convertFailed = true
			convertEnvIssue = convertFailureEnvIssue(err, string(out))
			log.Printf("appliance: migration %d: machine-convert failed (continuing best-effort): %v\n%s", m.ID, err, trimOut(out))
			if convertEnvIssue {
				_ = s.st.AddEvent(sctx, m.ID, "warn", "boot disk conversion could not finish because a command was missing in the conversion environment (exit 127 / \"command not found\"). This is an appliance/PATH problem, NOT an inconsistent source — re-syncing won't help. Update the appliance to the latest build (the convert step now pins a full PATH) and retry the cutover. Detail: "+oneLine(trimOut(out)))
			} else {
				_ = s.st.AddEvent(sctx, m.ID, "warn", "boot disk conversion could not finish, so the launched instance may not boot. Most often the replicated filesystem is inconsistent because the source kept changing during the copy — the reliable fix is a fresh full sync of a quiesced/idle source, then cut over again. The image volume is still created so you can also repair it manually in Rescue Mode (see docs/TROUBLESHOOTING.md). Detail: "+oneLine(trimOut(out)))
			}
		} else {
			_ = s.st.AddEvent(sctx, m.ID, "info", fmt.Sprintf("boot disk converted for Linode (virtio, network); boot kernel %s root %s", kernel, rootDevice))
			if access := accessSeededNote(req); access != "" {
				_ = s.st.AddEvent(sctx, m.ID, "info", "cutover: "+access+" — you can log in to the launched instance without rescue mode")
			}
			// Surface what the shrink step did (or didn't do) — otherwise a skipped or
			// failed shrink is invisible until the in-guest copy reports "does not fit".
			if m.BootTarget == api.BootTargetDisk {
				if sr := convertField(string(out), "vmrepl-shrink:"); sr != "" {
					lvl := "info"
					if strings.Contains(sr, "failed") || strings.Contains(sr, "skipped") {
						lvl = "warn"
					}
					_ = s.st.AddEvent(sctx, m.ID, lvl, "cutover: filesystem shrink "+sr)
				} else {
					_ = s.st.AddEvent(sctx, m.ID, "warn", "cutover: convert did not report a filesystem-shrink result (the shrink step did not run); if the copy reports it does not fit, the running appliance may predate the shrink fix — restart applianced after deploying, then retry")
				}
			}
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

	// Local-disk boot diverges here: instead of launching from cloned volumes, it
	// streams the image onto the instance's local disk. Steady-state replication
	// and the boot conversion above are identical.
	if m.BootTarget == api.BootTargetDisk {
		// Disk-boot streams the converted image onto the instance's local disk and
		// boots from it. If the conversion failed (e.g. the root filesystem couldn't
		// be located because the replicated copy is inconsistent), proceeding would
		// boot an unconverted image — which drops to a grub> prompt and then hangs
		// the cutover for an hour waiting for a power-off that never comes. Abort
		// with a clear, actionable error instead. (The replication volume is left
		// intact for manual inspection in Rescue Mode.)
		if convertFailed {
			if convertEnvIssue {
				s.fail(m.ID, "boot disk conversion failed because a required command was missing in the conversion environment (exit 127 / \"command not found\"). This is an appliance/PATH bug, NOT an inconsistent source — re-syncing won't help. Update the appliance to the latest build (the convert step now pins a full PATH), restart applianced, and retry the cutover. The replication volume is left intact for inspection; see the conversion detail above.")
			} else {
				s.fail(m.ID, "boot disk conversion failed, so the local-disk image would not boot (you'd see a grub> prompt and a stuck cutover). This almost always means the replicated filesystem is inconsistent — retry the cutover and let it take the crash-consistent snapshot (do not skip it; power the source off if you want to skip). See the conversion detail above for the fsck output.")
			}
			return
		}
		s.finalizeDisk(ctx, m, cl, kernel, rootDevice)
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
		// Plan: the one chosen on the create form (stored on the migration), then
		// an explicit finalize override, then a safe default.
		typ := orDefault(m.LinodeType, orDefault(req.Type, "g6-standard-2"))
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

// retryBusy retries fn while Linode reports the instance is busy (a transient
// state during provisioning/cloning/booting), backing off between attempts.
func retryBusy(ctx context.Context, fn func() error) error {
	var err error
	for i := 0; i < 15; i++ {
		err = fn()
		if err == nil {
			return err
		}
		// Stop immediately if our own context is done (the operator canceled or the
		// overall deadline passed) — the error is terminal, not transient.
		if ctx.Err() != nil {
			return err
		}
		if !retryableLinodeErr(err) {
			return err
		}
		select {
		case <-ctx.Done():
			return err
		case <-time.After(time.Duration(2+i) * time.Second):
		}
	}
	return err
}

// retryableLinodeErr reports whether a Linode API call is worth retrying: either
// the instance was transiently "busy", or the request timed out / dropped at the
// HTTP layer (e.g. "Client.Timeout exceeded while awaiting headers", connection
// reset, EOF). These are safe to retry only alongside an idempotent operation —
// see CreateDiskIfAbsent — so a request that timed out after succeeding isn't
// performed twice.
func retryableLinodeErr(err error) bool {
	if err == nil {
		return false
	}
	s := strings.ToLower(err.Error())
	for _, sub := range []string{"busy", "client.timeout", "deadline exceeded", "timeout", "connection reset", "connection refused", "eof", "no such host", "temporary"} {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}

// finalizeDisk cuts a disk-mode migration over to a Linode that boots from its
// local (plan) disk. Local Linode disks can't be attached to the appliance, so
// instead of cloning a volume onto the instance we: clone the replicated volume,
// create the target Linode plus a blank raw local disk, boot it ONCE from the
// volume (raw disk attached as sdb) so the in-guest one-shot installed by
// machine-convert copies the volume onto the local disk and powers off, then
// boot the instance from that local disk and drop the volume. Single-disk only
// (enforced at create time). The boot conversion already ran in finalize().
func (s *Server) finalizeDisk(ctx context.Context, m api.Migration, cl *linode.Client, kernel, rootDevice string) {
	sctx := s.ctx
	canceled := func() bool { return ctx.Err() != nil }
	boot := m.Disks[0]

	_ = s.st.AddEvent(sctx, m.ID, "info", "cutover (local disk) started: cloning the converted image")
	s.cleanupCutoverArtifacts(sctx, m) // retry-safe

	// 1) Clone the replication volume into the image we copy onto the local disk.
	label := cutoverVolumeLabel(m.Name, boot.Index, len(m.Disks))
	clone, err := cl.CloneVolume(ctx, boot.VolumeID, label)
	if canceled() {
		return
	}
	if err != nil {
		s.fail(m.ID, "clone boot volume: "+err.Error())
		return
	}
	if _, err := cl.WaitVolumeActive(ctx, clone.ID, 15*time.Minute); err != nil {
		if canceled() {
			return
		}
		s.fail(m.ID, "wait clone active: "+err.Error())
		return
	}
	_ = s.st.SetDiskArtifact(sctx, boot.ID, fmt.Sprintf("volume:%d", clone.ID))

	// 2) Create the target instance on the resolved plan (region = appliance's).
	region := s.cfg.Region
	if inst, err := cl.GetInstance(ctx, s.cfg.ApplianceLinodeID); err == nil && inst.Region != "" {
		region = inst.Region
	}
	inst, err := cl.CreateInstance(ctx, cutoverName(m.Name), region, m.LinodeType)
	if canceled() {
		return
	}
	if err != nil {
		s.fail(m.ID, "create instance: "+err.Error())
		return
	}
	_ = s.st.SetMigrationImage(sctx, m.ID, fmt.Sprintf("volume:%d", clone.ID), inst.ID)
	_ = s.st.AddEvent(sctx, m.ID, "info", fmt.Sprintf("created cutover Linode %q (id %d) on plan %s", cutoverName(m.Name), inst.ID, m.LinodeType))

	// A brand-new Linode is still "provisioning" for a short while, during which
	// disk operations are rejected with "Linode busy". Wait until it settles to
	// "offline" before adding the local disk.
	if err := cl.WaitInstanceStatus(ctx, inst.ID, "offline", 10*time.Minute); err != nil {
		if canceled() {
			return
		}
		s.fail(m.ID, "wait for instance to finish provisioning: "+err.Error())
		return
	}

	// 3) Create a blank raw local disk sized to the plan's full storage.
	diskMB := 0
	if types, terr := cl.ListTypes(ctx); terr == nil {
		diskMB = linode.TypeDiskMB(types, m.LinodeType)
	}
	if diskMB == 0 { // fallback: at least the image size, rounded up to MB
		diskMB = int((boot.SizeBytes + (1 << 20) - 1) / (1 << 20))
	}
	var rawDisk linode.Disk
	err = retryBusy(ctx, func() error {
		var e error
		rawDisk, e = cl.CreateDiskIfAbsent(ctx, inst.ID, "vmrepl-boot", diskMB, "raw")
		return e
	})
	if canceled() {
		return
	}
	if err != nil {
		s.fail(m.ID, "create local disk: "+err.Error())
		return
	}
	if err := cl.WaitDiskReady(ctx, inst.ID, rawDisk.ID, 10*time.Minute); err != nil {
		if canceled() {
			return
		}
		s.fail(m.ID, "wait local disk ready: "+err.Error())
		return
	}

	// 4) Boot once from the volume (sda) with the raw disk attached (sdb); the
	//    in-guest one-shot copies sda->sdb and powers the instance off.
	installCfg, err := cl.CreateConfig(ctx, inst.ID, "vmrepl-install", kernel, rootDevice,
		map[string]any{"sda": map[string]any{"volume_id": clone.ID}, "sdb": map[string]any{"disk_id": rawDisk.ID}})
	if err != nil {
		if canceled() {
			return
		}
		s.fail(m.ID, "create install config: "+err.Error())
		return
	}
	if err := retryBusy(ctx, func() error { return cl.Boot(ctx, inst.ID, installCfg) }); err != nil {
		if canceled() {
			return
		}
		s.fail(m.ID, "boot for install: "+err.Error())
		return
	}
	_ = s.st.AddEvent(sctx, m.ID, "info", "booting from the volume to copy the image onto the local disk (several minutes)…")
	if err := cl.WaitInstanceStatus(ctx, inst.ID, "running", 10*time.Minute); err != nil {
		if canceled() {
			return
		}
		s.fail(m.ID, "instance did not start the copy boot: "+err.Error())
		return
	}
	if err := cl.WaitInstanceStatus(ctx, inst.ID, "offline", 60*time.Minute); err != nil {
		if canceled() {
			return
		}
		s.fail(m.ID, "local-disk copy did not finish (instance never powered off — check it in Lish): "+err.Error())
		return
	}
	_ = s.st.AddEvent(sctx, m.ID, "info", "image copied onto the local disk; switching boot to the local disk")

	// 5) Boot from the local disk (sda) and confirm it comes up.
	bootCfg, err := cl.CreateConfig(ctx, inst.ID, "boot-migrated", kernel, rootDevice,
		map[string]any{"sda": map[string]any{"disk_id": rawDisk.ID}})
	if err != nil {
		if canceled() {
			return
		}
		s.fail(m.ID, "create boot config: "+err.Error())
		return
	}
	if err := retryBusy(ctx, func() error { return cl.Boot(ctx, inst.ID, bootCfg) }); err != nil {
		if canceled() {
			return
		}
		s.fail(m.ID, "boot from local disk: "+err.Error())
		return
	}
	if err := cl.WaitInstanceStatus(ctx, inst.ID, "running", 10*time.Minute); err != nil {
		if canceled() {
			return
		}
		s.fail(m.ID, "instance did not boot from the local disk: "+err.Error())
		return
	}

	// 6) Drop the now-unneeded cutover volume.
	_ = cl.DetachVolume(ctx, clone.ID)
	var derr error
	for i := 0; i < 10; i++ {
		if derr = cl.DeleteVolume(ctx, clone.ID); derr == nil {
			break
		}
		time.Sleep(2 * time.Second)
	}
	if derr != nil {
		_ = s.st.AddEvent(sctx, m.ID, "warn", fmt.Sprintf("local-disk boot is up, but temporary volume %d could not be deleted (remove it in Cloud Manager): %v", clone.ID, derr))
	}
	_ = s.st.SetDiskArtifact(sctx, boot.ID, fmt.Sprintf("disk:%d", rawDisk.ID))
	_ = s.st.SetMigrationImage(sctx, m.ID, fmt.Sprintf("disk:%d", rawDisk.ID), inst.ID)
	_ = s.st.SetMigrationState(sctx, m.ID, api.MigLaunched, "")
	_ = s.st.SetMigrateFinished(sctx, m.ID)
	_ = s.st.AddEvent(sctx, m.ID, "info", fmt.Sprintf("migration complete: %q (id %d) is booting from its local disk on plan %s", cutoverName(m.Name), inst.ID, m.LinodeType))
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
		if err := s.detachAndDeleteVolume(ctx, cl, vid); err != nil {
			_ = s.st.AddEvent(ctx, m.ID, "warn", fmt.Sprintf("could not delete previous cutover volume %d (detach it and delete in Cloud Manager, or the next clone may fail on a duplicate label): %v", vid, err))
		}
		_ = s.st.SetDiskArtifact(ctx, d.ID, "")
	}
	_ = s.st.SetMigrationImage(ctx, m.ID, "", 0)
}

// detachAndDeleteVolume detaches a volume and waits for the detach to actually
// complete before deleting it. Detach is asynchronous on Linode: deleting while
// the volume is still attached (e.g. right after deleting the instance it was on)
// fails with "must be detached", which then leaves the stale clone in place and
// makes the next clone fail with "Label must be unique". Poll until the volume is
// no longer attached to any Linode, then delete (with retries).
func (s *Server) detachAndDeleteVolume(ctx context.Context, cl *linode.Client, vid int64) error {
	_ = cl.DetachVolume(ctx, vid)
	deadline := time.Now().Add(2 * time.Minute)
	for {
		v, err := cl.GetVolume(ctx, vid)
		if err != nil {
			break // gone (404) or unreachable — fall through to delete, which no-ops if absent
		}
		if v.LinodeID == 0 && v.Status == "active" {
			break // fully detached
		}
		if ctx.Err() != nil || time.Now().After(deadline) {
			break
		}
		time.Sleep(3 * time.Second)
	}
	var derr error
	for i := 0; i < 10; i++ {
		if derr = cl.DeleteVolume(ctx, vid); derr == nil {
			return nil
		}
		if ctx.Err() != nil {
			return derr
		}
		time.Sleep(3 * time.Second)
	}
	return derr
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

// sourceAgentActive reports whether any disk's agent has checked in recently —
// i.e. the source OS is still running. Used to decide whether a snapshot can be
// taken (agent up) or the source is already off (current data is consistent), and
// to gate guided cutover's "source powered off" confirmation.
func (s *Server) sourceAgentActive(m api.Migration) bool {
	for _, d := range m.Disks {
		if !d.AgentLastSeen.IsZero() && time.Since(d.AgentLastSeen) < 2*time.Minute {
			return true
		}
	}
	return false
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
	// Lag reflects how recent the last completed sync is vs the RPO target. It is
	// independent of the baseline being finished, so it turns green as soon as a
	// sync lands within the target (e.g. 99s < 120s) instead of staying red until
	// baseline. It's informational once baselined (cutover no longer depends on it).
	lagOK := anySync && rpoSec <= float64(s.cfg.RPOTargetSec)

	diskWord := func(k int) string { return fmt.Sprintf("%d/%d disks", k, n) }
	return []api.ValidationCheck{
		// Pre-migration: environment/connectivity readiness while replicating.
		{Name: "Storage provisioned", OK: allStorage, Detail: diskWord(storageOK) + " ready", Group: "pre"},
		{Name: "Agent connected", OK: allAgents, Detail: diskWord(agentsSeen) + " checked in", Group: "pre"},
		{Name: fmt.Sprintf("Replication lag within %ds", s.cfg.RPOTargetSec), OK: lagOK, Detail: lagDetail2(anySync, rpoSec), Group: "pre"},
		// Migration: the gate that actually allows cutover.
		{Name: "Initial full sync complete", OK: allFull, Detail: diskWord(fullDone) + " baselined", Group: "migration"},
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

// isNonAgentHandshake reports whether a receiver session error is just a stray,
// non-agent connection failing the TLS 1.3 + mutual-certificate handshake (internet
// port scanners, health probes, plain-HTTP pokes). The real agent always negotiates
// TLS 1.3 with a valid client cert, so these can never be a genuine replication
// failure — we keep them out of the migration's error log to avoid false alarms.
func isNonAgentHandshake(err error) bool {
	if err == nil {
		return false
	}
	s := strings.ToLower(err.Error())
	for _, sig := range []string{
		"client offered only unsupported versions",        // old TLS (no 1.3)
		"remote error: tls:",                              // peer aborted the handshake
		"first record does not look like a tls handshake", // plain HTTP / junk
		"tls: bad certificate",
		"tls: certificate required",
		"client didn't provide a certificate",
		"tls: no certificates configured",
		"tls: unsupported", // misc unsupported handshake parameters
	} {
		if strings.Contains(s, sig) {
			return true
		}
	}
	return false
}

func trimOut(b []byte) string {
	const max = 2000
	if len(b) > max {
		b = b[len(b)-max:]
	}
	return string(b)
}

// convertFailureEnvIssue reports whether a failed machine-convert run died because
// a command was missing / the environment was unusable (exit 127 or a shell
// "command not found"), rather than because the source filesystem was inconsistent.
// The two need opposite remedies — fix/redeploy the appliance vs. re-sync a quiesced
// source — so we must not report the former as the latter (as the old message did).
func convertFailureEnvIssue(err error, out string) bool {
	if ee, ok := err.(*exec.ExitError); ok && ee.ExitCode() == 127 {
		return true
	}
	lo := strings.ToLower(out)
	return strings.Contains(lo, "command not found") || strings.Contains(lo, ": not found")
}

func orDefault(v, def string) string {
	if v == "" {
		return def
	}
	return v
}

// accessSeededNote summarizes which console/SSH access was seeded into the image
// at cutover, for the activity log. It never includes the secret itself.
func accessSeededNote(req api.FinalizeRequest) string {
	switch {
	case req.RootPassword != "" && req.SSHAuthorizedKey != "":
		return "set the root password and installed the SSH key for root"
	case req.RootPassword != "":
		return "set and unlocked the root password"
	case req.SSHAuthorizedKey != "":
		return "installed the SSH key for root"
	default:
		return ""
	}
}

// convertField extracts the value of a "key: value" line emitted by
// machine-convert.sh (e.g. "vmrepl-root: /dev/sda1") from its output.
func convertField(out, key string) string {
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, key) {
			return strings.TrimSpace(strings.TrimPrefix(line, key))
		}
	}
	return ""
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
