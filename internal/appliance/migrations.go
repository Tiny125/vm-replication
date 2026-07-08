package appliance

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/base64"
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
	// File transfer: the receiver writes the source's file TREE into a staging
	// directory (handleFileSession), not a block device/image.
	if isFileMethod(m.BootTarget) {
		return s.fileStageRoot(m)
	}
	if d.VolumeDevice != "" {
		return d.VolumeDevice
	}
	return filepath.Join(s.cfg.DataDir, fmt.Sprintf("migration-%d-disk%d.img", m.ID, d.Index))
}

// fileStageRoot is where a file-transfer migration's copied tree is staged on
// the appliance before it is delivered to the launched destination at cutover.
func (s *Server) fileStageRoot(m api.Migration) string {
	return filepath.Join(s.cfg.DataDir, fmt.Sprintf("filemig-%d-root", m.ID))
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
	// done closes when the serve loop has fully exited — including any session
	// that was in flight at cancel time — so a freeze can DRAIN the receiver
	// (wait for the pass to finish) instead of freezing mid-pass.
	done := make(chan struct{})
	s.receivers[d.ID] = &receiverHandle{cancel: cancel, done: done}

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
		if written == 0 {
			// Data is now flowing: advance to "replicating" so the console shows live
			// initial-sync progress, instead of staying "awaiting_agent" (and jumping
			// straight to 100%) until the first pass finishes. No-op once replicating.
			if err := s.st.MarkReplicating(s.ctx, migID); err != nil {
				log.Printf("appliance: mark replicating (migration %d): %v", migID, err)
			}
		}
	}

	diskIdx := d.Index
	dev0 := d.SourceDevice
	declaredSize := d.SizeBytes
	// The enrollment job id this migration's agent runs with (checkAgentHello).
	// A lookup failure leaves it empty, which skips the identity check rather
	// than locking out a healthy agent on a transient store error.
	expectJob := ""
	if tok, err := s.st.EnrollToken(s.ctx, m.ID); err == nil {
		expectJob = enrollJobID(tok)
	}
	// Consecutive identical failures (the agent retries every ~60s, so a
	// persistent problem like a wrong source disk repeats verbatim) produce ONE
	// activity-log event, not one per minute. onError runs from this receiver's
	// single accept loop, so no locking is needed.
	lastErrEvt := ""
	go func() {
		defer close(done)
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
			lastErrEvt = "" // a pass landed; log the next failure even if it repeats
			// File-transfer session: no block geometry (BlockSize/DeviceSize are 0,
			// so NumBlocks would divide by zero). A completed file pass IS the
			// baseline — record entries as the sync with the bytes it applied.
			if st.Hello.Mode == protocol.ModeFile {
				wasBaselined := false
				if d0, derr := s.st.Migration(s.ctx, migID); derr == nil {
					for _, dk := range d0.Disks {
						if dk.ID == diskID {
							wasBaselined = dk.FullSyncDone
						}
					}
				}
				if err := s.st.RecordDiskSync(s.ctx, migID, diskID, true, st.BlocksWritten, st.ChangedBlocks, st.BytesOnWire); err != nil {
					log.Printf("appliance: record file sync (migration %d disk %d): %v", migID, diskID, err)
				}
				if !wasBaselined {
					_ = s.st.AddEvent(s.ctx, migID, "info", fmt.Sprintf("file copy complete: %d items (%s) staged for %s — ready to cut over", st.BlocksWritten, humanBytes(st.BytesOnWire), dev0))
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
			// The source dropping mid-pass DURING CUTOVER is the expected result of
			// "power off the source now" — log it softly instead of raising a red
			// replication failure (and don't leave a scary LastError on the card).
			if isSourceDisconnect(serr) {
				if mm, merr := s.st.Migration(s.ctx, migID); merr == nil &&
					(mm.State == api.MigMigrating || mm.State == api.MigAwaitingCutover) {
					_ = s.st.AddEvent(s.ctx, migID, "info", fmt.Sprintf("disk %d (%s): the source went offline mid-pass during cutover — expected if you just powered it off; the interrupted pass was discarded whole, so the frozen image is the last complete pass", diskIdx, dev0))
					return
				}
			}
			_ = s.st.RecordDiskError(s.ctx, diskID, serr.Error())
			if serr.Error() != lastErrEvt {
				lastErrEvt = serr.Error()
				_ = s.st.AddEvent(s.ctx, migID, "error", fmt.Sprintf("disk %d (%s): replication attempt failed: %s", diskIdx, dev0, serr.Error()))
			}
		}, func(h protocol.Hello) bool {
			// Bounce a live pass into a crash-consistent re-read while we're quiescing
			// this disk for cutover (but never re-request a pass already consistent).
			return s.wantDiskConsistency(diskID) && !h.Consistent
		}, func(h protocol.Hello) bool {
			// Replication gate: record that this disk's agent connected (validates the
			// connection in the console, even before any data flows) and report whether
			// the operator has started replication. While disabled the receiver holds.
			if err := s.st.RecordAgentConnected(s.ctx, diskID); err != nil {
				log.Printf("appliance: record agent connected (migration %d disk %d): %v", migID, diskID, err)
			}
			return s.replicationEnabled(migID)
		}, func(h protocol.Hello) error {
			// Identity + wrong-disk guard: only THIS migration's enrolled agent
			// (matching job id), reading a plausibly-sized device, may feed this
			// disk. Everything else is rejected at the Hello, before any data
			// lands and before the console shows "agent connected".
			return checkAgentHello(expectJob, declaredSize, diskIdx, dev0, h)
		}, func(h protocol.Hello) (string, string, bool) {
			// File-transfer redirect: send the agent straight to the launched
			// destination Linode (direct copy, no appliance staging). Returns
			// hold=true while the destination is still launching/booting, the
			// destination target once its receiver is up, or ("","",false) when
			// there is no automation (appliance-staging fallback).
			return s.fileDataTarget(migID)
		})
		if err != nil && ctx.Err() == nil {
			log.Printf("appliance: receiver (migration %d disk %d) stopped: %v", migID, diskID, err)
		}
	}()
	return nil
}

// replicationEnabled reports whether the receiver should apply data for a
// migration (the gate consulted on every agent handshake). It is authoritative
// on the ReplicationEnabled flag so the operator can both start and PAUSE
// replication. The one exception is an in-progress cutover (migrating /
// awaiting_cutover): the final crash-consistent pass must be allowed through even
// if replication was paused. On a transient read error it fails open (allows)
// rather than stalling an active migration. Migrations created before this flow
// are marked enabled by a one-time startup backfill, so they are never held.
func (s *Server) replicationEnabled(migID int64) bool {
	m, err := s.st.Migration(s.ctx, migID)
	if err != nil {
		return true
	}
	if m.ReplicationEnabled {
		return true
	}
	switch m.State {
	case api.MigMigrating, api.MigAwaitingCutover:
		return true // cutover needs the final consistent pass even if paused
	default:
		return false
	}
}

// pausableStates are the migration states where replication can be started,
// paused or resumed (i.e. the steady replication phase, before cutover).
func replicationControllable(st api.MigrationState) bool {
	switch st {
	case api.MigCreated, api.MigAwaitingAgent, api.MigReplicating, api.MigReady:
		return true
	default:
		return false
	}
}

// replicationHasRun reports whether replication has ever actually moved data for
// this migration (so the console can tell "not started yet" from "paused").
func replicationHasRun(m api.Migration) bool {
	if m.State == api.MigReplicating {
		return true
	}
	for _, d := range m.Disks {
		if d.FullSyncDone || !d.LastSyncAt.IsZero() {
			return true
		}
	}
	return false
}

// Connection-validation windows for the gated-start flow.
const (
	// agentConnectedWindow is how recently a disk's agent must have handshaked to
	// count as "connected" (the agent re-checks every 60s; this allows a miss).
	agentConnectedWindow = 150 * time.Second
	// agentConnectGrace is how long after the agent download (enrolled_at) we wait
	// for a first connection before the console reports "connection failed". It's
	// the typical install+connect time plus a buffer; it stays soft (it flips back
	// to connected the moment a handshake lands).
	agentConnectGrace = 90 * time.Second
)

// diskAgentConnected reports whether a disk's agent handshaked within the window.
func diskAgentConnected(d api.Disk) bool {
	return !d.AgentConnectedAt.IsZero() && time.Since(d.AgentConnectedAt) < agentConnectedWindow
}

// allAgentsConnected reports whether every disk's agent is currently connected.
func allAgentsConnected(m api.Migration) bool {
	if len(m.Disks) == 0 {
		return false
	}
	for _, d := range m.Disks {
		if !diskAgentConnected(d) {
			return false
		}
	}
	return true
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

// receiverHandle tracks one disk receiver: cancel stops it (closes the
// listener; a session already in flight gets receiver.DrainGrace to finish
// before being severed), done closes when its serve loop has fully exited.
type receiverHandle struct {
	cancel context.CancelFunc
	done   chan struct{}
}

// stopReceivers stops every disk receiver for a migration without waiting
// (delete/cleanup paths, where an in-flight pass is irrelevant).
func (s *Server) stopReceivers(m api.Migration) {
	s.recMu.Lock()
	defer s.recMu.Unlock()
	for _, d := range m.Disks {
		if h, ok := s.receivers[d.ID]; ok {
			h.cancel()
			delete(s.receivers, d.ID)
		}
	}
}

// setCutoverFreezing marks (or clears) a migration whose guided cutover step 1
// — drain the in-flight pass, then freeze — is running. The console shows a
// "keep the source running" banner while set; once the migration parks in
// awaiting_cutover the card switches to "power off the source server now".
func (s *Server) setCutoverFreezing(migID int64, on bool) {
	if on {
		s.cutoverFreezing.Store(migID, true)
	} else {
		s.cutoverFreezing.Delete(migID)
	}
}

// cutoverFreezingFor reports whether a migration's freeze is in progress.
func (s *Server) cutoverFreezingFor(migID int64) bool {
	_, ok := s.cutoverFreezing.Load(migID)
	return ok
}

// drainReceivers stops a migration's receivers like stopReceivers, but WAITS
// (up to grace) for any replication pass already in flight to finish first —
// a completed pass ends at one consistent instant, so a freeze that drains
// always captures a clean image regardless of when the operator powers the
// source off. Returns false when a session was still running at the deadline
// (the receiver severs it shortly after — receiver.DrainGrace — but the frozen
// image may then mix two points in time).
func (s *Server) drainReceivers(m api.Migration, grace time.Duration) bool {
	s.recMu.Lock()
	var waits []chan struct{}
	for _, d := range m.Disks {
		if h, ok := s.receivers[d.ID]; ok {
			h.cancel()
			delete(s.receivers, d.ID)
			waits = append(waits, h.done)
		}
	}
	s.recMu.Unlock()
	deadline := time.NewTimer(grace)
	defer deadline.Stop()
	for _, done := range waits {
		select {
		case <-done:
		case <-deadline.C:
			return false
		}
	}
	return true
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
	case api.BootTargetFile:
		// File-transfer: one source entry sized by USED bytes, a destination OS
		// image, and a plan whose disk fits used+headroom. No block volume is
		// provisioned (the data streams to a launched destination).
		if err := s.validateFileCreate(ctx, &req, total); err != nil {
			writeErr(w, http.StatusBadRequest, err.Error())
			return
		}
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
		// Block methods provision a replication volume per disk here; the file
		// method streams to a launched destination and provisions none.
		if provisionsBlockStorage(m.BootTarget) {
			if err := s.provisionDiskStorage(ctx, m, d); err != nil {
				s.rollbackCreate(ctx, m.ID)
				writeErr(w, http.StatusInternalServerError, "provision storage: "+err.Error())
				return
			}
		}
	}

	_ = s.st.AddEvent(ctx, m.ID, "info", fmt.Sprintf("migration created with %d disk(s); waiting for the source agent", len(m.Disks)))
	if isFileMethod(m.BootTarget) {
		_ = s.st.AddEvent(ctx, m.ID, "info", fmt.Sprintf("method: file transfer — copies used files onto a new %s Linode running %s", m.LinodeType, m.OSImage))
	} else if m.BootTarget == api.BootTargetDisk {
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

// handleLinodeImages returns the deployable public OS images (grouped by vendor
// client-side), for the file-transfer method's destination OS dropdown.
func (s *Server) handleLinodeImages(w http.ResponseWriter, r *http.Request) {
	cl, ok := s.linodeClient(r.Context())
	if !ok {
		writeErr(w, http.StatusBadRequest, "add a valid Linode API token in Settings to load OS images")
		return
	}
	images, err := cl.ListImages(r.Context())
	if err != nil {
		writeErr(w, http.StatusBadGateway, "could not load Linode images: "+err.Error())
		return
	}
	// Only public distribution images make sense as a fresh destination OS.
	out := []map[string]any{}
	for _, im := range images {
		if !im.IsPublic {
			continue
		}
		out = append(out, map[string]any{"id": im.ID, "label": im.Label, "vendor": im.Vendor})
	}
	writeJSON(w, http.StatusOK, map[string]any{"images": out})
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

// handleStartReplication flips the replication gate on after the operator has
// confirmed (in the console) that the agent connection is validated. Until this
// is called the receiver acknowledges agent connections but holds (applies no
// data); afterwards the next agent pass streams the baseline. The console labels
// this "Start replication".
func (s *Server) handleStartReplication(w http.ResponseWriter, r *http.Request) {
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
	if m.ReplicationEnabled {
		// Idempotent: already running.
		writeJSON(w, http.StatusOK, map[string]string{"status": "replicating"})
		return
	}
	// Valid while in the replication phase (start, or resume after a pause), once
	// every disk's agent connection has been validated.
	if !replicationControllable(m.State) {
		writeErr(w, http.StatusConflict, "replication can only be started or resumed during the replication phase")
		return
	}
	if !allAgentsConnected(m) {
		writeErr(w, http.StatusConflict, "the agent has not connected on all disks yet — wait for the connection to be validated")
		return
	}
	resuming := replicationHasRun(m)
	if err := s.st.SetReplicationEnabled(ctx, id, true); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	switch {
	case resuming && isFileMethod(m.BootTarget):
		_ = s.st.AddEvent(ctx, id, "info", "replication resumed by operator; the agent re-copies only the files that changed during the pause on its next pass (within ~60s)")
	case resuming:
		_ = s.st.AddEvent(ctx, id, "info", "replication resumed by operator; the agent ships only the blocks changed during the pause on its next pass (within ~60s)")
	case isFileMethod(m.BootTarget):
		_ = s.st.AddEvent(ctx, id, "info", "replication started by operator; launching the destination Linode, then the agent copies your used files straight into it (within ~60s)")
	default:
		_ = s.st.AddEvent(ctx, id, "info", "replication started by operator; the agent will stream the initial full sync on its next pass (within ~60s)")
	}
	// File transfer (direct): launch the destination now so the agent copies
	// straight into it. Idempotent; a no-op for block methods and file-fallback.
	if isFileMethod(m.BootTarget) {
		s.ensureFileDestination(m)
	}
	// Receivers are already listening from create time; they will now accept data.
	writeJSON(w, http.StatusAccepted, map[string]string{"status": "replicating"})
}

// handlePauseReplication pauses replication: it disables the gate so the agent's
// next pass is held (acknowledged but no data applied). Already-replicated data
// and the change-tracking checkpoints are kept, so resuming ships only a delta.
// The console labels this "Pause replication".
func (s *Server) handlePauseReplication(w http.ResponseWriter, r *http.Request) {
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
	if !m.ReplicationEnabled {
		// Idempotent: already paused / not running.
		writeJSON(w, http.StatusOK, map[string]string{"status": "paused"})
		return
	}
	if !replicationControllable(m.State) {
		writeErr(w, http.StatusConflict, "replication can only be paused during the replication phase")
		return
	}
	if err := s.st.SetReplicationEnabled(ctx, id, false); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if isFileMethod(m.BootTarget) {
		_ = s.st.AddEvent(ctx, id, "info", "replication paused by operator; a pass already in flight finishes, then the agent holds. Resume to continue re-copying only the files that changed (no full re-copy).")
	} else {
		_ = s.st.AddEvent(ctx, id, "info", "replication paused by operator; a pass already in flight finishes, then the agent holds. Resume to continue with an incremental delta sync (no full re-copy).")
	}
	writeJSON(w, http.StatusAccepted, map[string]string{"status": "paused"})
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
		if isFileMethod(m.BootTarget) {
			writeErr(w, http.StatusConflict, "the initial file copy must finish before cutover")
		} else {
			writeErr(w, http.StatusConflict, "the initial full sync must finish on all disks before cutover")
		}
		return
	}
	if m.State == api.MigFailed {
		if isFileMethod(m.BootTarget) {
			_ = s.st.AddEvent(ctx, id, "info", "retrying cutover — rebooting the destination into the copied files")
		} else {
			_ = s.st.AddEvent(ctx, id, "info", "retrying cutover on the already-replicated data")
		}
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
	if isFileMethod(m.BootTarget) {
		_ = s.st.AddEvent(ctx, id, "info", "cutover: operator confirmed — rebooting the destination into your copied files")
	} else {
		_ = s.st.AddEvent(ctx, id, "info", "cutover: operator confirmed — completing (convert, clone, launch)")
	}
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
	s.cleanupMigrationResources(ctx, m, cleanupOpts{keepReplVolume: true}) // keep (detach) the replication volume
	if err := s.st.DeleteMigration(ctx, id); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// handleCloseMigration finishes a successful migration: it removes the temporary
// vmrep-<name> replication volume (no longer needed once the server is migrated)
// and the migration row, while KEEPING the launched cutover instance and its
// clone volumes — those are the user's new server. Only allowed once the
// migration has launched (or its image is ready); the console asks the operator
// to confirm first. This is the safe counterpart to Delete, which instead tears
// down the launched instance.
func (s *Server) handleCloseMigration(w http.ResponseWriter, r *http.Request) {
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
	if !canCloseMigration(m.State) {
		writeErr(w, http.StatusConflict, "only a completed migration (launched or image ready) can be closed — use Delete for one that is still in progress")
		return
	}
	// Delete the replication volume, keep the launched instance + clone volumes.
	s.cleanupMigrationResources(ctx, m, cleanupOpts{keepLaunched: true})
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
		s.cleanupMigrationResources(ctx, m, cleanupOpts{}) // failed create: delete everything
	}
	_ = s.st.DeleteMigration(ctx, id)
}

// cleanupOpts controls which resources cleanupMigrationResources removes.
type cleanupOpts struct {
	// keepReplVolume: only DETACH the vmrep-<name> replication volume (keep it in
	// the account for reference) instead of deleting it. Used by user delete.
	keepReplVolume bool
	// keepLaunched: keep the launched cutover instance and its <name>-cutover
	// clone volumes. Used by "Close migration" (finish a successful migration):
	// the migrated server and its volumes are the user's to keep — only the
	// temporary replication volume is removed.
	keepLaunched bool
}

// cleanupMigrationResources stops a migration's receivers/finalize and removes
// its Linode resources (or file-fallback images) and manifests per opts. It does
// NOT delete the migration row — the caller decides that. Safe to call
// repeatedly.
//
//   - The cutover artifacts (launched instance + <name>-cutover clone volumes)
//     are deleted unless opts.keepLaunched is set.
//   - The vmrep-<name> replication volume is deleted, or only DETACHED and kept
//     when opts.keepReplVolume is set.
//   - File-fallback images and manifests are always removed.
func (s *Server) cleanupMigrationResources(ctx context.Context, m api.Migration, opts cleanupOpts) {
	s.recMu.Lock()
	if cancel := s.finalizes[m.ID]; cancel != nil {
		cancel()
		delete(s.finalizes, m.ID)
	}
	s.recMu.Unlock()
	s.stopReceivers(m)
	s.dropFileDest(m.ID) // forget any direct file-transfer destination tracking

	cl, haveLinode := s.linodeClient(ctx)
	// Remove cutover artifacts (launched instance + clone volumes) unless the
	// caller is closing a successful migration and wants to keep the new server.
	if haveLinode && m.LaunchedID != 0 && !opts.keepLaunched {
		if err := cl.DeleteInstance(ctx, m.LaunchedID); err != nil {
			log.Printf("appliance: delete cutover Linode %d failed (remove it in Cloud Manager): %v", m.LaunchedID, err)
		}
	}
	for _, d := range m.Disks {
		s.progress.Delete(d.ID)
		if haveLinode && !opts.keepLaunched {
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
			if !opts.keepReplVolume {
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
		} else if isFileMethod(m.BootTarget) {
			_ = os.RemoveAll(s.diskDevicePath(m, d)) // the file-staging tree
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

	// msg picks method-appropriate wording: the file method copies files onto an
	// already-launched destination (no image/volume/convert), so its cutover
	// events must not use block-method vocabulary.
	fileMode := isFileMethod(m.BootTarget)
	msg := func(fileMsg, blockMsg string) string {
		if fileMode {
			return fileMsg
		}
		return blockMsg
	}

	// Guided step 1 is now running (drain + freeze): light the console's "keep
	// the source running" banner until the migration parks in awaiting_cutover
	// (the state the deferred clear races benignly with) or this run ends.
	if req.GuidedShutdown {
		s.setCutoverFreezing(m.ID, true)
		defer s.setCutoverFreezing(m.ID, false)
	}

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
		_ = s.st.AddEvent(sctx, m.ID, "info", msg(
			"cutover: replication stopped; holding the copied files for launch. Power off the source before launching so the copy is final.",
			"cutover: replication stopped; freezing the current replicated copy as the image (crash-consistent, repaired with fsck on convert). Power off the source before launching so the captured state is final."))
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
		_ = s.st.AddEvent(sctx, m.ID, "warn", "cutover: could NOT capture a consistent image — the source root could not be remounted read-only (a process is still writing to / ). This is expected on a running root with no LVM. Either retry with \"Skip the read-only snapshot\" ticked (cut over from the current crash-consistent data, repaired with fsck), or power the source off and cut over again. Replication is still running.")
		return
	}

	// Freeze: stop NEW passes and wait for any pass already in flight to finish,
	// so the frozen image is one complete, consistent pass. Without the drain, a
	// pass could keep applying after the "freeze" and then be cut partway when
	// the operator powers the source off (as instructed), leaving the image a
	// mix of two points in time.
	drainStart := time.Now()
	if s.drainReceivers(m, receiver.DrainGrace) {
		if waited := time.Since(drainStart); waited > 2*time.Second {
			_ = s.st.AddEvent(sctx, m.ID, "info", fmt.Sprintf(msg(
				"cutover: waited %s for the in-flight file-copy pass to end — passes apply atomically (an interrupted pass is discarded whole), so the copied files are the last complete pass",
				"cutover: waited %s for the in-flight replication pass to end — delta passes apply atomically (an interrupted pass is discarded whole), so the frozen image is the last complete pass"), waited.Round(time.Second)))
		}
	} else {
		_ = s.st.AddEvent(sctx, m.ID, "warn", fmt.Sprintf(msg(
			"cutover: a file-copy pass was still running after %s and was severed; its partial data was discarded whole — the copied files remain the last completely applied pass",
			"cutover: a replication pass was still running after %s and was severed; its staged data was discarded whole — the frozen image remains the last completely applied pass"), receiver.DrainGrace))
	}
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
		_ = s.st.AddEvent(sctx, m.ID, "info", msg(
			"cutover step 1 done: replication stopped and the copied files held for launch. Now POWER OFF the source server, then click \"Launch instance\" to reboot the destination into your migrated files.",
			"cutover step 1 done: replication stopped and the current copy frozen as the image. Now POWER OFF the source server, then click \"Launch instance\" to convert, clone and launch."))
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

	// File transfer: no block conversion/clone — launch the destination from the
	// chosen OS image and deliver the staged file tree onto it. Wholly separate
	// from the block finalize below.
	if isFileMethod(m.BootTarget) {
		s.finalizeFile(ctx, m, req)
		return
	}

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
	convertEnvIssue := false   // failure was a missing command / bad env, not inconsistent data
	convertNoRoot := false     // no root/OS filesystem on the disk (wrong source device)
	convertMountIssue := false // chroot setup failed (a pseudo-fs mount point was not a directory)
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
		// (Disk-boot: no in-guest installer is baked in any more — the copy runs
		// in RESCUE MODE, streamed from the appliance; see finalizeDisk. The
		// whole-disk ext shrink also runs in finalizeDisk, sized to the ACTUAL
		// local disk Linode creates rather than the plan's nominal size.)
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
			convertNoRoot = convertFailureNoRoot(string(out))
			convertMountIssue = convertFailureMountIssue(string(out))
			log.Printf("appliance: migration %d: machine-convert failed: %v\n%s", m.ID, err, trimOut(out))
			if convertNoRoot {
				_ = s.st.AddEvent(sctx, m.ID, "error", "boot disk conversion failed: "+wrongDiskMsg+" Detail: "+oneLine(trimOut(out)))
			} else if convertMountIssue {
				_ = s.st.AddEvent(sctx, m.ID, "warn", convertMountMsg+" Detail: "+oneLine(trimOut(out)))
			} else if convertEnvIssue {
				_ = s.st.AddEvent(sctx, m.ID, "warn", "boot disk conversion could not finish because a command was missing in the conversion environment (exit 127 / \"command not found\"). This is an appliance/PATH problem, NOT an inconsistent source — re-syncing won't help. Update the appliance to the latest build (the convert step now pins a full PATH) and retry the cutover. Detail: "+oneLine(trimOut(out)))
			} else {
				_ = s.st.AddEvent(sctx, m.ID, "warn", "boot disk conversion could not finish, so the launched instance may not boot. Most often the replicated filesystem is inconsistent because the source kept changing during the copy — the reliable fix is a fresh full sync of a quiesced/idle source, then cut over again. The image volume is still created so you can also repair it manually in Rescue Mode (see docs/TROUBLESHOOTING.md). Detail: "+oneLine(trimOut(out)))
			}
		} else {
			_ = s.st.AddEvent(sctx, m.ID, "info", fmt.Sprintf("boot disk converted for Linode (virtio, network); boot kernel %s root %s", kernel, rootDevice))
			if access := accessSeededNote(req); access != "" {
				_ = s.st.AddEvent(sctx, m.ID, "info", "cutover: "+access+" — you can log in to the launched instance without rescue mode")
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
			if convertNoRoot {
				s.fail(m.ID, "cutover aborted — "+wrongDiskMsg)
			} else if convertMountIssue {
				s.fail(m.ID, convertMountMsg)
			} else if convertEnvIssue {
				s.fail(m.ID, "boot disk conversion failed because a required command was missing in the conversion environment (exit 127 / \"command not found\"). This is an appliance/PATH bug, NOT an inconsistent source — re-syncing won't help. Update the appliance to the latest build (the convert step now pins a full PATH), restart applianced, and retry the cutover. The replication volume is left intact for inspection; see the conversion detail above.")
			} else {
				s.fail(m.ID, "boot disk conversion failed, so the local-disk image would not boot (you'd see a grub> prompt and a stuck cutover). This almost always means the replicated filesystem is inconsistent — retry the cutover and let it take the crash-consistent snapshot (do not skip it; power the source off if you want to skip). See the conversion detail above for the fsck output.")
			}
			return
		}
		s.finalizeDisk(ctx, m, cl, kernel, rootDevice, cutoverInstanceLabel(req.Label, m.Name))
		return
	}

	// If the disk holds no OS root (wrong source device — e.g. swap), a launched
	// instance would only reach a grub> prompt. Abort BEFORE cloning/launching so we
	// never produce an unbootable instance; the replication volume is left intact.
	if convertNoRoot {
		s.fail(m.ID, "cutover aborted — "+wrongDiskMsg)
		return
	}

	_ = s.st.AddEvent(sctx, m.ID, "info", "cutover started: converting boot disk and cloning volumes")

	// On a retry, remove any instance/volumes left over from the previous cutover
	// attempt so we start clean and the <name>-cutover labels are free to reuse.
	s.cleanupCutoverArtifacts(sctx, m)

	// 2) Clone every disk's volume into a launchable <name>-cutover artifact.
	cloneIDs := make([]int64, len(m.Disks))
	for i, d := range m.Disks {
		label := cutoverVolumeLabelFor(req.VolumeLabel, m.Name, d.Index, len(m.Disks))
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
		label := cutoverInstanceLabel(req.Label, m.Name)
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
// local (plan) disk. A local disk can only be written from INSIDE the instance,
// so: create the target Linode plus a blank raw local disk, boot it into RESCUE
// MODE (Finnix) with that disk as /dev/sda, and have the operator paste ONE
// command in its Lish console — it streams the converted image straight off the
// appliance's replication volume onto the disk, grows the root, and powers the
// instance off; the appliance then boots it from the local disk. No clone
// volume, no reliance on the migrated OS booting from a volume (see
// cutover_stream.go). Single-disk only (enforced at create time). The boot
// conversion already ran in finalize().
func (s *Server) finalizeDisk(ctx context.Context, m api.Migration, cl *linode.Client, kernel, rootDevice, instLabel string) {
	sctx := s.ctx
	canceled := func() bool { return ctx.Err() != nil }
	boot := m.Disks[0]
	bootDevice := s.diskDevicePath(m, boot)

	_ = s.st.AddEvent(sctx, m.ID, "info", "cutover (local disk) started")
	s.cleanupCutoverArtifacts(sctx, m) // retry-safe

	// 1) Create the target instance on the resolved plan (region = appliance's).
	//    Create it (and its local disk) BEFORE cloning the image so the filesystem
	//    shrink can be sized to the real local disk Linode hands us, not a guess.
	region := s.cfg.Region
	if inst, err := cl.GetInstance(ctx, s.cfg.ApplianceLinodeID); err == nil && inst.Region != "" {
		region = inst.Region
	}
	inst, err := cl.CreateInstance(ctx, instLabel, region, m.LinodeType)
	if canceled() {
		return
	}
	if err != nil {
		s.fail(m.ID, "create instance: "+err.Error())
		return
	}
	// Record the instance id now so a later failure or retry can clean it up.
	_ = s.st.SetMigrationImage(sctx, m.ID, "", inst.ID)
	_ = s.st.AddEvent(sctx, m.ID, "info", fmt.Sprintf("created cutover Linode %q (id %d) on plan %s", instLabel, inst.ID, m.LinodeType))

	// Disable Lassie (the Shutdown Watchdog) for the install phase. The in-guest
	// one-shot signals "copy done" by powering the instance off; with Lassie enabled
	// Linode would instead auto-reboot it, so it would never settle "offline" and the
	// cutover would hang until timeout. Re-enabled after the final boot below.
	if err := cl.SetWatchdog(ctx, inst.ID, false); err != nil {
		log.Printf("appliance: migration %d: could not disable shutdown watchdog on instance %d: %v", m.ID, inst.ID, err)
		// Surface it: with Lassie still on, Linode auto-reboots the instance after
		// the copy's power-off, so the copy loops and the cutover waits its full
		// budget — the operator should know to disable the watchdog by hand.
		_ = s.st.AddEvent(sctx, m.ID, "warn", "could not disable the shutdown watchdog (Lassie) on the cutover instance — if the copy finishes but the instance auto-reboots instead of staying off, disable the watchdog in Cloud Manager (instance Settings) and Retry cutover: "+err.Error())
	}

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

	// 2) Create a blank raw local disk sized to the plan's full storage, then read
	//    back its ACTUAL size: Linode reserves a sliver, so the real disk is a little
	//    smaller than the nominal plan disk, and that real size is what the image
	//    must be shrunk to fit.
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
	actualMB := rawDisk.Size
	if actualMB <= 0 {
		actualMB = diskMB
	}

	// 3) Shrink the whole-disk ext filesystem on the source boot device to fit the
	//    ACTUAL local disk before cloning it. Sizing to the real disk (minus a small
	//    rounding headroom) replaces the old fixed margin guessed against the plan's
	//    nominal size. Only whole-disk ext{2,3,4} roots are shrunk; the convert
	//    script no-ops other layouts. The first normal boot grows the root back to
	//    fill the disk.
	shrunkMB := 0 // resulting filesystem size, when the shrink ran and reported one
	if s.cfg.ConvertScript != "" && isBlockDevice(bootDevice) && actualMB > 256 {
		target := actualMB - 16 // headroom for block rounding, anchored to the real disk
		_ = s.st.AddEvent(sctx, m.ID, "info", fmt.Sprintf("cutover: shrinking the image filesystem to fit the %s local disk (%d MiB; target %d MiB)", m.LinodeType, actualMB, target))
		shrinkCmd := exec.CommandContext(ctx, bashPath(), s.cfg.ConvertScript, bootDevice)
		shrinkCmd.Env = append(os.Environ(), "VMREPL_SHRINK_ONLY=1", fmt.Sprintf("VMREPL_SHRINK_MB=%d", target))
		out, serr := shrinkCmd.CombinedOutput()
		if canceled() {
			return
		}
		if sr := convertField(string(out), "vmrepl-shrink:"); sr != "" {
			lvl := "info"
			if strings.Contains(sr, "failed") || strings.Contains(sr, "skipped") {
				lvl = "warn"
			}
			_ = s.st.AddEvent(sctx, m.ID, lvl, "cutover: filesystem shrink "+sr)
			shrunkMB = shrinkResultMB(sr)
		}
		if serr != nil {
			log.Printf("appliance: migration %d: filesystem shrink command failed: %v\n%s", m.ID, serr, trimOut(out))
		}
	}

	// Authoritative fit check against the ACTUAL disk: fail fast now instead of
	// waiting on a copy that can never complete. Covers both the shrunk case and
	// unshrinkable (partitioned/non-ext — i.e. stock cloud) images, whose full
	// source size must fit as-is.
	if msg := diskFitError(boot.SizeBytes, shrunkMB, actualMB, m.LinodeType); msg != "" {
		s.fail(m.ID, msg)
		return
	}

	// 4) Authorize the image download and boot the instance into RESCUE MODE with
	//    the blank local disk attached as /dev/sda. Rescue (Finnix) always boots
	//    and never touches the migrated OS: the operator pastes ONE command in
	//    its Lish console, which streams the converted image straight off the
	//    appliance's own (fully hydrated) replication volume onto the local disk,
	//    grows the root, and powers the instance off — the "copy done" signal.
	//    No clone volume is created, so there is nothing to hydrate slowly and
	//    nothing extra to clean up.
	streamBytes := boot.SizeBytes
	if shrunkMB > 0 {
		streamBytes = int64(shrunkMB) << 20 // only the (shrunk) filesystem needs copying
	}
	copyBudget := diskCopyTimeout(streamBytes)
	token := s.registerCutoverStream(m.ID, bootDevice, streamBytes, copyBudget+time.Hour)
	defer s.dropCutoverStream(m.ID) // invalidate the token + hide the command however this run ends
	s.setCutoverCopyCmd(m.ID, s.cutoverCopyCmd(token))

	if err := retryBusy(ctx, func() error {
		return cl.RescueInstance(ctx, inst.ID, map[string]any{"sda": map[string]any{"disk_id": rawDisk.ID}})
	}); err != nil {
		if canceled() {
			return
		}
		s.fail(m.ID, "boot into rescue mode: "+err.Error())
		return
	}
	if err := cl.WaitInstanceStatus(ctx, inst.ID, "running", 10*time.Minute); err != nil {
		if canceled() {
			return
		}
		s.fail(m.ID, "instance did not boot into rescue mode: "+err.Error())
		return
	}
	_ = s.st.AddEvent(sctx, m.ID, "warn", fmt.Sprintf("ACTION NEEDED — instance %q (id %d) is in RESCUE MODE. Open its Lish console (Cloud Manager → Linodes → %s → Launch LISH Console) and paste the copy command shown on this card. It streams %s onto the local disk with live progress, then powers the instance off; the appliance finishes automatically from there.", instLabel, inst.ID, instLabel, humanBytes(streamBytes)))

	// The pasted command signals completion by powering the instance off. Wait in
	// 15-minute slices up to the size-aware budget (which also absorbs the human
	// delay before the paste), emitting a progress event per slice so the wait is
	// visibly alive.
	copyStart := time.Now()
	for {
		slice := 15 * time.Minute
		if rem := copyBudget - time.Since(copyStart); rem < slice {
			slice = rem
		}
		var werr error
		if slice > 0 {
			werr = cl.WaitInstanceStatus(ctx, inst.ID, "offline", slice)
		} else {
			werr = fmt.Errorf("copy budget exhausted")
		}
		if canceled() {
			return
		}
		if werr == nil {
			break // powered off — copy complete
		}
		if time.Since(copyStart) < copyBudget {
			_ = s.st.AddEvent(sctx, m.ID, "info", fmt.Sprintf("cutover: waiting for the rescue copy to finish (%s elapsed of up to %s) — if you haven't yet, paste the copy command from this card into the instance's Lish console", time.Since(copyStart).Round(time.Minute), copyBudget.Round(time.Minute)))
			continue
		}
		s.fail(m.ID, fmt.Sprintf("the rescue copy did not power the instance off within %s. Open the instance's Lish console: if the copy command was never run, click Retry cutover and paste the fresh command it shows; if a copy is still running there, let it finish (the instance powers itself off), then Retry cutover. Last error: %s", copyBudget.Round(time.Minute), werr.Error()))
		return
	}
	s.dropCutoverStream(m.ID) // copy done: kill the token and hide the command now
	_ = s.st.AddEvent(sctx, m.ID, "info", fmt.Sprintf("image copied onto the local disk in %s; booting from the local disk", time.Since(copyStart).Round(time.Minute)))

	// 6) Boot from the local disk (sda) and confirm it comes up.
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

	// Re-enable Lassie now that the copy's power-off dance is done, so the migrated
	// production instance keeps the auto-reboot-on-crash watchdog (best-effort).
	if err := cl.SetWatchdog(ctx, inst.ID, true); err != nil {
		log.Printf("appliance: migration %d: could not re-enable shutdown watchdog on instance %d: %v", m.ID, inst.ID, err)
	}

	// No temporary volume was created (the image streamed straight from the
	// appliance's replication volume), so there is nothing to clean up here.
	_ = s.st.SetDiskArtifact(sctx, boot.ID, fmt.Sprintf("disk:%d", rawDisk.ID))
	_ = s.st.SetMigrationImage(sctx, m.ID, fmt.Sprintf("disk:%d", rawDisk.ID), inst.ID)
	_ = s.st.SetMigrationState(sctx, m.ID, api.MigLaunched, "")
	_ = s.st.SetMigrateFinished(sctx, m.ID)
	_ = s.st.AddEvent(sctx, m.ID, "info", fmt.Sprintf("migration complete: %q (id %d) is booting from its local disk on plan %s", instLabel, inst.ID, m.LinodeType))
}

// finalizeFile cuts over a FILE-transfer migration. The destination was already
// launched at "Start replication" and the agent has been copying the source's
// files STRAIGHT INTO it (direct, no appliance staging). Cutover just reboots
// the destination so it comes up running the migrated files, then marks the
// migration launched.
func (s *Server) finalizeFile(ctx context.Context, m api.Migration, req api.FinalizeRequest) {
	sctx := s.ctx
	cl, ok := s.linodeClient(sctx)
	if !ok || s.cfg.ApplianceLinodeID == 0 {
		_ = s.st.SetMigrationState(sctx, m.ID, api.MigImageReady,
			"Linode automation not configured; the copied files are staged on the appliance")
		_ = s.st.SetMigrateFinished(sctx, m.ID)
		return
	}
	instID := m.LaunchedID
	if v, ok := s.fileDests.Load(m.ID); ok {
		if d := v.(*fileDest); d.instanceID != 0 {
			instID = d.instanceID
		}
	}
	if instID == 0 {
		s.fail(m.ID, "the destination was never launched — click Start replication first so the destination comes up and receives your files, then cut over")
		return
	}
	// Reboot the destination so it boots cleanly into the migrated files (the
	// receiver copied them onto its live root while it idled).
	if err := cl.RebootInstance(ctx, instID); err != nil {
		s.fail(m.ID, fmt.Sprintf("could not reboot the destination (id %d): %v — reboot it in Cloud Manager and it will come up as your migrated server", instID, err))
		return
	}
	if err := cl.WaitInstanceStatus(ctx, instID, "running", 10*time.Minute); err != nil {
		if ctx.Err() != nil {
			return
		}
		_ = s.st.AddEvent(sctx, m.ID, "warn", "destination is slow to come back after reboot; check its Lish console")
	}
	s.dropFileDest(m.ID)
	_ = s.st.SetMigrationState(sctx, m.ID, api.MigLaunched, "")
	_ = s.st.SetMigrateFinished(sctx, m.ID)
	_ = s.st.AddEvent(sctx, m.ID, "info", fmt.Sprintf("migration complete: destination (id %d) rebooted into your migrated system. Power off the source now if you haven't. (You can remove the leftover vmrepl-receiver service on the destination: systemctl disable --now vmrepl-receiver.)", instID))
}

// randPassword returns a strong random password for a launched destination when
// the operator didn't supply one. It is never logged.
func randPassword() string {
	b := make([]byte, 18)
	_, _ = rand.Read(b)
	return "Vmr-" + base64.RawURLEncoding.EncodeToString(b)
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

// cutoverInstanceLabel is the launched instance's label: the operator's custom
// name from the cutover dialog — sanitized to Linode's charset, trimmed and
// capped at 64 chars — or the "<migration>-cutover" default when blank or
// unusable (fewer than 3 usable chars).
func cutoverInstanceLabel(custom, migName string) string {
	c := strings.Trim(sanitizeLabel(custom), "-_")
	if len(c) > 64 {
		c = strings.Trim(c[:64], "-_")
	}
	if len(c) < 3 {
		return cutoverName(migName)
	}
	return c
}

// cutoverVolumeLabelFor is the cutover volume's label (volume-boot): the
// operator's custom name — sanitized, capped at Linode's 32-char volume limit,
// keeping the per-disk suffix for multi-disk migrations — or the existing
// "<migration>-cutover" default when blank or unusable.
func cutoverVolumeLabelFor(custom, migName string, idx, total int) string {
	c := strings.Trim(sanitizeLabel(custom), "-_")
	if c == "" {
		return cutoverVolumeLabel(migName, idx, total)
	}
	suffix := ""
	if total > 1 {
		suffix = fmt.Sprintf("-%d", idx)
	}
	return fitLabel("", c, suffix)
}

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

	// Gated start + pause/resume signals for the console: the agent connection is
	// validated independently of any data flowing; replication is started, paused
	// and resumed explicitly; and a never-connecting agent is reported (softly)
	// once the post-install grace ends.
	v.AgentConnected = allAgentsConnected(m)
	hasRun := replicationHasRun(m)
	v.ReplicationActive = m.ReplicationEnabled
	v.ReplicationStarted = m.ReplicationEnabled || hasRun
	v.ReplicationPaused = !m.ReplicationEnabled && hasRun && replicationControllable(m.State)
	if !m.ReplicationEnabled && replicationControllable(m.State) {
		// The gate is off: either not started yet, or paused. Either way the
		// Start/Resume button is enabled once the connection is validated.
		v.CanReplicate = v.AgentConnected
		// "Connection failed" only applies pre-start (a paused migration's agent is
		// connected and simply held); never flag it once replication has run.
		if !hasRun && !v.AgentConnected && !m.EnrolledAt.IsZero() && time.Since(m.EnrolledAt) > agentConnectGrace {
			v.ConnectionFailed = true
		}
	}

	switch m.State {
	case api.MigCreated, api.MigAwaitingAgent:
		switch {
		case v.ReplicationPaused:
			v.Phase = "replication paused"
		case m.ReplicationEnabled:
			v.Phase = "starting replication"
		case v.AgentConnected:
			v.Phase = "agent connected — ready to start replication"
		case v.ConnectionFailed:
			v.Phase = "agent not detected"
		default:
			v.Phase = "waiting for agent"
		}
	case api.MigReplicating, api.MigReady:
		if v.ReplicationPaused {
			v.Phase = "replication paused"
			break
		}
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
	v.CutoverCopyCmd = s.cutoverCopyCmdFor(m.ID)
	v.CutoverFreezing = s.cutoverFreezingFor(m.ID)

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
	// The first pre-check differs by method: block methods provision a replication
	// volume ("Storage provisioned"); the file method provisions no block storage
	// and instead needs a destination OS image + plan chosen ("Destination ready").
	var first api.ValidationCheck
	if isFileMethod(m.BootTarget) {
		ready := m.OSImage != "" && m.LinodeType != ""
		// The destination Linode is not created yet — it launches when the operator
		// clicks Start replication. Say so, so a green tick isn't read as "the
		// instance already exists".
		detail := "OS image + plan chosen — Linode launches on Start"
		if !ready {
			detail = "choose a destination OS image and plan"
		}
		first = api.ValidationCheck{Name: "Destination configured", OK: ready, Detail: detail, Group: "pre"}
	} else {
		first = api.ValidationCheck{Name: "Storage provisioned", OK: allStorage, Detail: diskWord(storageOK) + " ready", Group: "pre"}
	}
	fullDetail := diskWord(fullDone) + " baselined"
	fullName := "Initial full sync complete"
	if isFileMethod(m.BootTarget) {
		fullDetail = map[bool]string{true: "files copied", false: "copying files"}[allFull]
		fullName = "Initial file copy complete"
	}
	return []api.ValidationCheck{
		// Pre-migration: environment/connectivity readiness while replicating.
		first,
		{Name: "Agent connected", OK: allAgents, Detail: diskWord(agentsSeen) + " checked in", Group: "pre"},
		{Name: fmt.Sprintf("Replication lag within %ds", s.cfg.RPOTargetSec), OK: lagOK, Detail: lagDetail2(anySync, rpoSec), Group: "pre"},
		// Migration: the gate that actually allows cutover.
		{Name: fullName, OK: allFull, Detail: fullDetail, Group: "migration"},
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

// isMigrationActive reports whether a migration is still in progress (created or
// running) rather than finished (image_ready / launched) or failed. Used to gate
// destructive account-level actions such as deleting the audit-log bucket.
func isMigrationActive(st api.MigrationState) bool {
	switch st {
	case api.MigImageReady, api.MigLaunched, api.MigFailed:
		return false
	default:
		return true
	}
}

// activeMigrationCount counts migrations that are still in progress (created or
// running). A "migration complete" (launched/image_ready) or failed migration
// is not active, so account-level actions (remove the Linode token, delete the
// audit bucket) may proceed once every remaining migration has finished.
func activeMigrationCount(migs []api.Migration) int {
	n := 0
	for _, m := range migs {
		if isMigrationActive(m.State) {
			n++
		}
	}
	return n
}

// canCloseMigration reports whether a migration may be "closed" (finished): only
// after it has launched (or its image is ready). Closing removes the temporary
// vmrep- replication volume while keeping the launched instance and its clone
// volumes — the migrated server. Anything still in progress (or failed) must be
// deleted instead.
func canCloseMigration(st api.MigrationState) bool {
	switch st {
	case api.MigImageReady, api.MigLaunched:
		return true
	default:
		return false
	}
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

// convertFailureNoRoot reports whether the boot-disk conversion failed because it
// found no root/OS filesystem on the replicated disk — i.e. the disk holds no
// operating system (typically swap or a data disk), which means the WRONG source
// device was selected. This is a user error, not an inconsistent copy, so it needs
// a different message and must never launch a would-be-grub> instance.
func convertFailureNoRoot(out string) bool {
	return strings.Contains(out, "could not locate a root filesystem")
}

// convertFailureMountIssue reports whether the conversion died while setting up
// the chroot because a pseudo-filesystem mount point (/proc, /sys, /dev or /run)
// was missing or not a directory ("mount point is not a directory"). The fsck has
// already passed by that point, so this is a conversion-ENVIRONMENT problem, not
// an inconsistent source: the remedy is to update the appliance (which now
// normalises those mount points) and retry — NOT to re-sync a quiesced source.
func convertFailureMountIssue(out string) bool {
	return strings.Contains(strings.ToLower(out), "mount point is not a directory")
}

// convertMountMsg explains a chroot-setup mount failure and its correct remedy
// (update the appliance + retry, not a re-sync).
const convertMountMsg = "boot disk conversion failed while preparing the chroot: a pseudo-filesystem mount point (/proc, /sys, /dev or /run) was missing or not a directory (\"mount point is not a directory\"). This is a conversion-environment issue, NOT an inconsistent source — the filesystem check already passed. Update the appliance to the latest build (the convert step now creates these mount points before mounting) and retry the cutover; re-syncing won't help. The replication volume is left intact for inspection."

// wrongDiskMsg guides the operator to replicate the disk that actually holds the
// OS root, used when the converted disk has no root filesystem.
const wrongDiskMsg = "the converted disk has no root/OS filesystem, so a launched instance would only reach a grub> prompt. Two common causes: (1) the WRONG source device was selected — a swap or data disk has no OS; on the source run `lsblk -f` (or `findmnt -no SOURCE /`) to find the disk mounted at `/`, then migrate that whole disk (e.g. /dev/sda or /dev/vda). Or (2) the initial full sync was INCOMPLETE — e.g. a stale change-tracking checkpoint from an earlier migration on the same source, so only part of the disk was copied; check that the initial full sync transferred roughly the disk's used size, and if not, run `sudo rm -f /var/lib/vmrepl-source-*.cbt` on the source (or the uninstall one-liner) and re-sync. Nothing was launched; delete this migration first."

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
// shrinkResultMB extracts the resulting filesystem size in MiB from a
// "vmrepl-shrink:" result line (e.g. "ok 81888M" or
// "failed resize2fs rc=1 fs=81920M ..."). Returns 0 when no size is present
// (e.g. "skipped ext4", "failed unmount", "failed e2fsck rc=4").
func shrinkResultMB(sr string) int {
	for _, tok := range strings.Fields(sr) {
		t := strings.TrimSuffix(strings.TrimPrefix(tok, "fs="), "M")
		if n, err := strconv.Atoi(t); err == nil && n > 0 {
			return n
		}
	}
	return 0
}

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

// diskCopyTimeout budgets how long the disk-mode cutover waits for the in-guest
// copy (volume → local disk, which ends by powering the instance off). A fixed
// 60 minutes failed real migrations — an ~80 GiB image at Block-Storage read
// speeds can take well over an hour — so the budget scales with the disk size
// at a conservative 10 MiB/s, plus 15 minutes of boot/fsck slack, floored at 1h
// and capped at 8h as a runaway guard.
func diskCopyTimeout(sizeBytes int64) time.Duration {
	d := 15*time.Minute + time.Duration(sizeBytes/(10<<20))*time.Second
	if d < time.Hour {
		return time.Hour
	}
	if d > 8*time.Hour {
		return 8 * time.Hour
	}
	return d
}

// isSourceDisconnect reports whether a session error means the SOURCE vanished
// mid-pass (powered off / rebooted / network cut). During cutover that is the
// expected consequence of "power off the source now", so the caller logs it as
// info instead of a red replication failure.
func isSourceDisconnect(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "stream closed before done") ||
		strings.Contains(msg, "connection reset by peer")
}

// checkAgentHello guards every receiver session for a disk: it verifies the
// agent's IDENTITY and its device GEOMETRY at the handshake, before the agent
// is recorded as connected and before any data can land on the target.
//
// Identity: agent mTLS certs are GLOBAL (every source ever enrolled keeps valid
// credentials) and receiver ports repeat across appliance reinstalls (port =
// base + disk id; ids restart with a fresh DB) — so a stale, never-uninstalled
// agent from an OLD enrollment, possibly on a different machine, can connect to
// a new migration's port and stream its disk into the fresh volume. Each
// enrollment therefore runs its agent with a unique job id (the enrollment-token
// prefix); a session with any other job id is refused. expectJob=="" (token
// unavailable) skips the identity check.
//
// Geometry: a reported device size grossly different from the declared disk
// size means the agent is reading the wrong disk; see deviceSizeMismatch.
func checkAgentHello(expectJob string, declaredSize int64, diskIdx int, dev0 string, h protocol.Hello) error {
	if expectJob != "" && h.JobID != expectJob {
		return fmt.Errorf("refusing session: the connecting agent (host %q, device %s) belongs to a DIFFERENT enrollment (job %q) than this migration's disk %d (%s). This usually means an old agent from a previous migration — possibly on another server — is still installed and pointed at this port, or the agent on the source predates this appliance's identity check. Run the uninstall one-liner on any old source machines, then re-run THIS migration's enrollment command on the intended source",
			h.SourceHostname, orDefault(h.DevicePath, "?"), orDefault(h.JobID, "none"), diskIdx, dev0)
	}
	if deviceSizeMismatch(declaredSize, h.DeviceSize) {
		return fmt.Errorf("refusing to replicate: the agent's %s is %s but this migration's disk %d (%s) was declared as %s — the agent is reading the WRONG DISK (a device that small/large is usually swap or a data disk, not the OS disk). On the source run `lsblk -f` (or `findmnt -no SOURCE /`) to see which disk holds the root filesystem `/`, then delete this migration and create a new one with that device and its real size",
			h.DevicePath, humanBytes(h.DeviceSize), diskIdx, dev0, humanBytes(declaredSize))
	}
	return nil
}

// diskFitError reports (as a ready-to-show message; "" = fits) whether the
// image cannot fit the instance's ACTUAL local disk. When the whole-disk ext
// shrink ran, its resulting size is authoritative; otherwise — partitioned or
// non-ext images (every stock cloud image) CANNOT be shrunk — the full source
// size must fit as-is. Without this, an oversized unshrinkable image failed at
// the very end of the rescue copy (dd: no space) and the cutover timed out
// instead of failing fast. An unknown disk size (0) never blocks.
func diskFitError(sizeBytes int64, shrunkMB, actualMB int, linodeType string) string {
	if actualMB <= 0 {
		return ""
	}
	if shrunkMB > 0 {
		if shrunkMB > actualMB {
			return fmt.Sprintf("image filesystem is %d MiB but the %s local disk is only %d MiB, so the copy cannot fit; recreate the migration on a larger plan", shrunkMB, linodeType, actualMB)
		}
		return ""
	}
	if sizeBytes > int64(actualMB)<<20 {
		return fmt.Sprintf("the source disk is %s but the %s local disk is only %d MiB, and this image cannot be shrunk (only whole-disk ext filesystems can — partitioned/cloud images cannot); pick a plan with a larger local disk, or use the volume-boot method which has no size squeeze", humanBytes(sizeBytes), linodeType, actualMB)
	}
	return ""
}

// deviceSizeMismatch reports whether the device size an agent declared in its
// Hello is grossly different from the size the migration's disk was created
// with — the signature of the agent reading the WRONG DISK (e.g. a ~512 MiB
// swap disk enrolled as an "80 GiB" migration). The tolerance is deliberately
// generous: operators enter sizes in whole GB rounded up and real disks run a
// little smaller than nominal, so only a reported size under HALF or over
// DOUBLE the declared size counts as a mismatch. Either size being unknown
// (<= 0) disables the check.
func deviceSizeMismatch(declared, reported int64) bool {
	if declared <= 0 || reported <= 0 {
		return false
	}
	return reported < declared/2 || reported > declared*2
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
