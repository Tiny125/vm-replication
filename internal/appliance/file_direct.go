package appliance

import (
	"crypto/rand"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/tiny125/vm-replication/internal/api"
	"github.com/tiny125/vm-replication/internal/protocol"
	"github.com/tiny125/vm-replication/internal/transport"
)

// Direct file-transfer: the agent copies the source's files STRAIGHT INTO the
// launched destination Linode — nothing is staged on the appliance.
//
// Flow:
//   - When the operator starts replication, the appliance launches the
//     destination from the chosen OS image and injects cloud-init user-data that
//     downloads the receiver binary + the appliance's data-plane certs (both
//     token-gated) and runs the receiver on the destination, applying files to
//     "/". The destination presents the APPLIANCE's receiver cert, so the agent
//     needs no per-destination certificate — it just keeps -server-name pointed
//     at the appliance (Go verifies the cert against ServerName, not the dialed
//     IP).
//   - The appliance's control receiver, for a file session, returns a HelloAck
//     redirect (DataTarget) to the destination once its receiver is reachable,
//     or a Hold while it is still launching. The agent re-dials the destination
//     and streams there.

// destFilePort is the fixed port the destination's file receiver listens on.
const destFilePort = 5999

// fileDest tracks a migration's launched destination for direct file transfer.
type fileDest struct {
	instanceID int64
	ip         string
	ready      bool   // the destination's receiver is reachable on destFilePort
	label      string // the operator-chosen instance name
	token      string // the bootstrap token (also used by the manual-install command)
	failed     bool   // the launch failed; the operator can retry Create destination
	errMsg     string // failure reason (never contains the root password)
}

// directPass is what the appliance knows about a DIRECT file migration's copy
// passes for one disk. In direct mode the data goes straight from the agent to
// the destination, so the appliance observes no session completing: everything
// here comes from the agent's Hello report. Values are stored immutably (a fresh
// struct per update, like fileDest) because the console reads them from HTTP
// handlers while the receiver's accept loop writes them.
type directPass struct {
	agentReports   bool      // this agent build confirms its passes (Hello.ReportsPasses)
	streamingSince time.Time // when we first handed this agent a redirect
	lastSeq        int64     // sequence of the last pass we RECORDED (de-duplication)
	lastEntries    int64
	lastBytes      int64
	lastComplete   bool
	lastAt         time.Time
}

// destBootstrap authorizes a destination's one-time download of the receiver
// binary + certs during its cloud-init first boot.
type destBootstrap struct {
	migID   int64
	expires time.Time
}

// fileDataTarget is the receiver's FileTarget hook for a file migration: it
// redirects the agent to the launched destination, holds while it is still
// coming up, or (no automation) falls back to applying on the appliance.
func (s *Server) fileDataTarget(migID int64) (target, serverName string, hold bool) {
	if _, ok := s.linodeClient(s.ctx); !ok || s.cfg.ApplianceLinodeID == 0 {
		return "", "", false // no automation: appliance-staging fallback
	}
	v, ok := s.fileDests.Load(migID)
	if !ok {
		return "", "", true // launch pending / in progress
	}
	d := v.(*fileDest)
	if d.ip == "" || !d.ready {
		return "", "", true // still launching / booting / installing the receiver
	}
	return net.JoinHostPort(d.ip, fmt.Sprintf("%d", destFilePort)), s.cfg.PublicHost, false
}

// fileAutomation reports whether the appliance can launch a destination Linode
// (a Linode token is configured and the appliance knows its own instance id).
// Without it, the file method falls back to staging on the appliance.
func (s *Server) fileAutomation() bool {
	if s.st == nil {
		return false
	}
	if _, ok := s.linodeClient(s.ctx); !ok {
		return false
	}
	return s.cfg.ApplianceLinodeID != 0
}

// createFileDestination launches the destination for a file migration with an
// operator-chosen label + root password, once. It replaces the old auto-launch:
// the operator creates the destination explicitly (so they can log into it), and
// Start replication stays gated until the destination's receiver is ready.
// Returns an error if it cannot begin (wrong method, no automation, already up).
func (s *Server) createFileDestination(m api.Migration, label, rootPass string) error {
	if !isFileMethod(m.BootTarget) {
		return fmt.Errorf("not a file-transfer migration")
	}
	if !s.fileAutomation() {
		return fmt.Errorf("no Linode automation: add a Linode API token in Settings to launch a destination")
	}
	if v, ok := s.fileDests.Load(m.ID); ok {
		if d := v.(*fileDest); !d.failed {
			return fmt.Errorf("a destination instance already exists for this migration")
		}
	}
	if label == "" {
		label = cutoverInstanceLabel("", m.Name)
	}
	if rootPass == "" {
		// Fall back to a strong generated password if the operator left it blank.
		rootPass = randPassword()
	}
	// Mark launching so the console reflects it immediately and a second click is a
	// no-op. rootPass is passed to the launch goroutine only — never stored here.
	s.fileDests.Store(m.ID, &fileDest{label: label})
	go s.launchFileDestination(m, label, rootPass)
	return nil
}

// launchFileDestination creates the destination from the OS image with cloud-init
// that installs + runs the receiver, then polls until its receiver port answers.
// rootPass is used only for the create call and is never logged or persisted.
func (s *Server) launchFileDestination(m api.Migration, label, rootPass string) {
	ctx := s.ctx
	cl, ok := s.linodeClient(ctx)
	if !ok {
		return
	}
	region := s.cfg.Region
	if inst, err := cl.GetInstance(ctx, s.cfg.ApplianceLinodeID); err == nil && inst.Region != "" {
		region = inst.Region
	}

	// Token-gated bootstrap so the destination can pull the receiver binary + certs.
	tok := s.registerDestBootstrap(m.ID, 6*time.Hour)
	userData := s.destCloudInit(tok)

	inst, err := cl.CreateInstanceFromImageUserData(ctx, label, region, m.LinodeType, m.OSImage, rootPass, userData)
	if err != nil {
		// Mark the destination failed (don't fail the whole migration) so the
		// operator can fix the cause and retry "Create destination instance".
		s.fileDests.Store(m.ID, &fileDest{label: label, token: tok, failed: true, errMsg: err.Error()})
		_ = s.st.AddEvent(ctx, m.ID, "error", "could not launch the destination instance: "+err.Error())
		return
	}
	s.fileDests.Store(m.ID, &fileDest{instanceID: inst.ID, label: label, token: tok})
	_ = s.st.SetMigrationImage(ctx, m.ID, "file:direct", inst.ID)
	_ = s.st.AddEvent(ctx, m.ID, "info", fmt.Sprintf("launched destination %q (id %d) from image %s on plan %s — installing the file receiver", label, inst.ID, m.OSImage, m.LinodeType))

	// Wait for the instance to run, learn its public IP, then poll its receiver
	// port until it answers.
	if err := cl.WaitInstanceStatus(ctx, inst.ID, "running", 10*time.Minute); err != nil {
		_ = s.st.AddEvent(ctx, m.ID, "warn", "destination is slow to boot; still waiting: "+err.Error())
	}
	ip := ""
	for i := 0; i < 40 && ip == ""; i++ {
		if got, err := cl.GetInstance(ctx, inst.ID); err == nil {
			for _, a := range got.IPv4 {
				if a != "" && !isPrivateIP(a) {
					ip = a
					break
				}
			}
		}
		if ip == "" {
			time.Sleep(5 * time.Second)
		}
	}
	if ip == "" {
		s.fileDests.Store(m.ID, &fileDest{instanceID: inst.ID, label: label, token: tok, failed: true, errMsg: "could not determine the destination's public IP"})
		_ = s.st.AddEvent(ctx, m.ID, "warn", "could not determine the destination's public IP yet")
		return
	}
	d := &fileDest{instanceID: inst.ID, ip: ip, label: label, token: tok}
	s.fileDests.Store(m.ID, d)

	// Watch the receiver port until it answers. The watch NEVER gives up while
	// the migration still tracks this destination: after the fast window it warns
	// once (the automatic cloud-init install has probably stalled — the card shows
	// the manual install command) and keeps checking, so a receiver brought up
	// LATE by the manual paste still unlocks Start replication.
	addr := net.JoinHostPort(ip, fmt.Sprintf("%d", destFilePort))
	s.watchFileDest(m.ID, inst.ID, addr, 15*time.Minute, 10*time.Second, 30*time.Second)
}

// watchFileDest polls addr until the destination's receiver answers, marking
// the tracked fileDest ready (and telling the operator). It polls every
// fastEvery during fastWindow, then warns once and continues every slowEvery
// indefinitely. It exits when the receiver is ready, the server shuts down, or
// the migration stops tracking this instance (deleted / relaunched).
func (s *Server) watchFileDest(migID, instID int64, addr string, fastWindow, fastEvery, slowEvery time.Duration) {
	ctx := s.ctx
	// Readiness is proven by a CA-verified TLS handshake, not a bare TCP accept
	// (see tlsProbeDest). Tests inject destProbe to avoid real network I/O.
	probe := s.destProbe
	if probe == nil {
		probe = s.tlsProbeDest
	}
	fastUntil := time.Now().Add(fastWindow)
	warned := fastWindow <= 0 // a resumed watch (no fast window) needs no stall warning
	for {
		if ctx != nil && ctx.Err() != nil {
			return
		}
		v, ok := s.fileDests.Load(migID)
		if !ok {
			return // migration deleted/closed
		}
		d := v.(*fileDest)
		if d.instanceID != instID {
			return // destination was relaunched; a newer watch owns it
		}
		if probe(addr) {
			s.fileDests.Store(migID, &fileDest{instanceID: instID, ip: d.ip, label: d.label, token: d.token, ready: true})
			_ = s.st.AddEvent(ctx, migID, "info", fmt.Sprintf("destination %s is ready to receive — you can now Start replication", addr))
			return
		}
		wait := fastEvery
		if time.Now().After(fastUntil) {
			if !warned {
				warned = true
				_ = s.st.AddEvent(ctx, migID, "warn", fmt.Sprintf("the destination's file receiver has not come up on %s automatically (the image/region may lack cloud-init/Metadata). Run the manual install command shown on the card in the instance's Lish console — the console keeps watching and unlocks Start replication as soon as the receiver answers.", addr))
			}
			wait = slowEvery
		}
		time.Sleep(wait)
	}
}

// tlsProbeDest reports whether the destination's file RECEIVER answers at addr.
// A bare TCP accept is not proof — a crash-looping service (or anything else
// squatting the port) accepts briefly without ever serving, and marking that
// "ready" would redirect the agent into a dead port. So the probe performs a
// TLS handshake with the appliance's own data-plane identity: the destination
// serves the APPLIANCE's receiver certificate, so a handshake verified against
// our CA — completed, or answered with a TLS alert after our side verified the
// server — proves the receiver itself is up. Without loadable data-plane certs
// it falls back to plain TCP reachability.
func (s *Server) tlsProbeDest(addr string) bool {
	cfg, err := transport.ClientConfig(s.cfg.TLS, s.cfg.PublicHost)
	if err != nil {
		c, derr := net.DialTimeout("tcp", addr, 5*time.Second)
		if derr != nil {
			return false
		}
		_ = c.Close()
		return true
	}
	conn, err := tls.DialWithDialer(&net.Dialer{Timeout: 5 * time.Second}, "tcp", addr, cfg)
	if err == nil {
		_ = conn.Close()
		return true
	}
	// A "remote error: tls: …" alert arrives AFTER our side verified the server's
	// certificate chain — the receiver answered, it merely disliked something
	// about the session. Anything else (refused, timeout, not TLS, a foreign
	// certificate) is not our receiver.
	return strings.Contains(err.Error(), "remote error: tls:")
}

// fileDestsToResume selects the active file migrations whose destination was
// already launched (persisted LaunchedID) — after an appliance restart their
// in-memory tracking is gone and the watch must be rebuilt for each.
func fileDestsToResume(migs []api.Migration) []api.Migration {
	var out []api.Migration
	for _, m := range migs {
		if !isFileMethod(m.BootTarget) || m.LaunchedID == 0 {
			continue
		}
		switch m.State {
		case api.MigCreated, api.MigAwaitingAgent, api.MigReplicating, api.MigReady:
			out = append(out, m)
		}
	}
	return out
}

// resumeFileDestWatches rebuilds destination tracking after an appliance
// restart: for each active file migration with a launched destination it mints
// a fresh bootstrap token (so the manual install command works again),
// re-discovers the instance's IP, and restarts the readiness watch.
func (s *Server) resumeFileDestWatches() {
	if !s.fileAutomation() {
		return
	}
	migs, err := s.st.ListMigrations(s.ctx)
	if err != nil {
		return
	}
	for _, m := range fileDestsToResume(migs) {
		if _, loaded := s.fileDests.LoadOrStore(m.ID, &fileDest{instanceID: m.LaunchedID, token: s.registerDestBootstrap(m.ID, 6*time.Hour)}); loaded {
			continue // already tracked
		}
		_ = s.st.AddEvent(s.ctx, m.ID, "info", "appliance restarted — resuming the destination readiness watch")
		go func(m api.Migration) {
			cl, ok := s.linodeClient(s.ctx)
			if !ok {
				return
			}
			ip := ""
			for i := 0; i < 20 && ip == ""; i++ {
				if got, err := cl.GetInstance(s.ctx, m.LaunchedID); err == nil {
					for _, a := range got.IPv4 {
						if a != "" && !isPrivateIP(a) {
							ip = a
							break
						}
					}
				}
				if ip == "" {
					time.Sleep(5 * time.Second)
				}
			}
			if ip == "" {
				return
			}
			if v, ok := s.fileDests.Load(m.ID); ok {
				d := v.(*fileDest)
				s.fileDests.Store(m.ID, &fileDest{instanceID: d.instanceID, ip: ip, label: d.label, token: d.token})
			}
			s.watchFileDest(m.ID, m.LaunchedID, net.JoinHostPort(ip, fmt.Sprintf("%d", destFilePort)), 0, 30*time.Second, 30*time.Second)
		}(m)
	}
}

// destStatusFor computes the console-facing destination status for a migration.
func (s *Server) destStatusFor(m api.Migration) (state string, linodeID int64, ip, errMsg, manualCmd string) {
	if !isFileMethod(m.BootTarget) {
		return "", 0, "", "", ""
	}
	if !s.fileAutomation() {
		return "fallback", 0, "", "", ""
	}
	v, ok := s.fileDests.Load(m.ID)
	if !ok {
		return "none", 0, "", "", ""
	}
	d := v.(*fileDest)
	if d.token != "" {
		manualCmd = s.destManualInstallCmd(d.token)
	}
	switch {
	case d.failed:
		return "failed", d.instanceID, d.ip, d.errMsg, manualCmd
	case d.ready:
		return "ready", d.instanceID, d.ip, "", manualCmd
	case d.ip != "":
		return "installing", d.instanceID, d.ip, "", manualCmd
	default:
		return "launching", d.instanceID, d.ip, "", manualCmd
	}
}

// destReady reports whether the migration's destination receiver is reachable
// (so Start replication may proceed). True for the no-automation fallback.
func (s *Server) destReady(m api.Migration) bool {
	if !isFileMethod(m.BootTarget) || !s.fileAutomation() {
		return true
	}
	if v, ok := s.fileDests.Load(m.ID); ok {
		return v.(*fileDest).ready
	}
	return false
}

// destManualInstallCmd builds the copy-paste command the operator runs in the
// destination's Lish console (as root) to install + start the file receiver when
// the automatic cloud-init install stalls.
func (s *Server) destManualInstallCmd(token string) string {
	base := fmt.Sprintf("%s://%s:%d", s.scheme(), s.cfg.PublicHost, s.cfg.ConsolePort)
	return fmt.Sprintf("curl -fsSL %s'%s/dest/install.sh?token=%s' | sudo bash", s.curlPinFlag(), base, token)
}

// registerDestBootstrap mints a token letting the destination download the
// receiver binary + certs during first boot.
func (s *Server) registerDestBootstrap(migID int64, ttl time.Duration) string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	tok := hex.EncodeToString(b)
	s.destBootstraps.Store(tok, &destBootstrap{migID: migID, expires: time.Now().Add(ttl)})
	return tok
}

func (s *Server) lookupDestBootstrap(token string) (*destBootstrap, bool) {
	v, ok := s.destBootstraps.Load(token)
	if !ok {
		return nil, false
	}
	d := v.(*destBootstrap)
	if time.Now().After(d.expires) {
		s.destBootstraps.Delete(token)
		return nil, false
	}
	return d, true
}

// destCloudInit builds the cloud-init user-data (a bash script; Linode base64s
// it) that installs and runs the file receiver on the destination.
func (s *Server) destCloudInit(token string) string {
	base := fmt.Sprintf("%s://%s:%d", s.scheme(), s.cfg.PublicHost, s.cfg.ConsolePort)
	pin := s.curlPinFlag()
	script := fmt.Sprintf(`#!/bin/bash
# vm-replication destination bootstrap: install + run the file receiver so the
# source agent can copy straight into this instance.
set -e
# Show progress in the terminal AND keep the log — an operator pasting this in
# Lish must see what happens (it used to swallow all output into the log only).
exec > >(tee -a /var/log/vmrepl-dest.log) 2>&1
echo "vmrepl-dest: bootstrap starting $(date)"
BIN=/usr/local/bin/vmrepl-receiver
ETC=/etc/vmrepl
mkdir -p "$ETC"
curl -fsSL %s'%s/dest/receiver?token=%s' -o "$BIN"
chmod 0755 "$BIN"
for f in ca.crt receiver.crt receiver.key ; do
  curl -fsSL %s'%s/dest/cert?token=%s&name='"$f" -o "$ETC/$f"
done
chmod 600 "$ETC/receiver.key"
cat >/etc/systemd/system/vmrepl-receiver.service <<UNIT
[Unit]
Description=vm-replication file receiver (destination)
After=network-online.target
Wants=network-online.target
[Service]
ExecStart=$BIN -listen :%d -device / -mode file -cert $ETC/receiver.crt -key $ETC/receiver.key -ca $ETC/ca.crt
Restart=always
[Install]
WantedBy=multi-user.target
UNIT
systemctl daemon-reload
systemctl enable --now vmrepl-receiver.service
echo "vmrepl-dest: receiver started on :%d"
echo "vmrepl-dest: done — the migration console will detect the receiver within ~30 seconds and unlock Start replication."
`, pin, base, token, pin, base, token, destFilePort, destFilePort)
	return script
}

// handleCreateDestination launches a file-transfer migration's destination
// instance with an operator-chosen label + root password (POST
// /api/v1/migrations/{id}/destination). The root password is used only to create
// the instance and is never logged or persisted.
func (s *Server) handleCreateDestination(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	m, err := s.st.Migration(r.Context(), id)
	if err != nil {
		writeErr(w, http.StatusNotFound, "not found")
		return
	}
	var req api.CreateDestinationRequest
	_ = json.NewDecoder(r.Body).Decode(&req)
	if err := s.createFileDestination(m, sanitizeLabel(req.Label), req.RootPassword); err != nil {
		writeErr(w, http.StatusConflict, err.Error())
		return
	}
	_ = s.st.AddEvent(r.Context(), id, "info", "creating the destination instance (operator) — installing the file receiver, then Start replication unlocks once it is ready")
	writeJSON(w, http.StatusAccepted, map[string]string{"status": "launching"})
}

// handleDestInstall serves the receiver-install script (GET /dest/install.sh,
// token-gated) so the operator can run it manually in the destination's Lish
// console when cloud-init doesn't auto-install the receiver.
func (s *Server) handleDestInstall(w http.ResponseWriter, r *http.Request) {
	tok := r.URL.Query().Get("token")
	if _, ok := s.lookupDestBootstrap(tok); !ok {
		writeErr(w, http.StatusForbidden, "invalid or expired bootstrap token")
		return
	}
	w.Header().Set("Content-Type", "text/x-shellscript; charset=utf-8")
	_, _ = w.Write([]byte(s.destCloudInit(tok)))
}

// handleDestReceiver serves the receiver binary to a booting destination (GET
// /dest/receiver, token-gated).
func (s *Server) handleDestReceiver(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.lookupDestBootstrap(r.URL.Query().Get("token")); !ok {
		writeErr(w, http.StatusForbidden, "invalid or expired bootstrap token")
		return
	}
	if s.cfg.ReceiverBinary == "" {
		writeErr(w, http.StatusServiceUnavailable, "receiver binary not configured on the appliance")
		return
	}
	http.ServeFile(w, r, s.cfg.ReceiverBinary)
}

// handleDestCert serves one of the appliance's data-plane cert files to a
// booting destination (GET /dest/cert?name=, token-gated).
func (s *Server) handleDestCert(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.lookupDestBootstrap(r.URL.Query().Get("token")); !ok {
		writeErr(w, http.StatusForbidden, "invalid or expired bootstrap token")
		return
	}
	var path string
	switch r.URL.Query().Get("name") {
	case "ca.crt":
		path = s.cfg.TLS.CAFile
	case "receiver.crt":
		path = s.cfg.TLS.CertFile
	case "receiver.key":
		path = s.cfg.TLS.KeyFile
	default:
		writeErr(w, http.StatusBadRequest, "unknown cert file")
		return
	}
	data, err := os.ReadFile(path)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "cert unavailable")
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	_, _ = w.Write(data)
}

// dropFileDest forgets a migration's destination tracking (on delete/close).
func (s *Server) dropFileDest(migID int64) {
	s.fileDests.Delete(migID)
	s.destBootstraps.Range(func(k, v any) bool {
		if v.(*destBootstrap).migID == migID {
			s.destBootstraps.Delete(k)
		}
		return true
	})
	// Forget this migration's pass reports too: they describe a destination that
	// no longer exists, and a relaunched one must earn its own baseline.
	if m, err := s.st.Migration(s.ctx, migID); err == nil {
		for _, d := range m.Disks {
			s.directPasses.Delete(d.ID)
		}
	}
}

// directPassFor returns the recorded pass state for a disk (zero value if none).
func (s *Server) directPassFor(diskID int64) directPass {
	if v, ok := s.directPasses.Load(diskID); ok {
		return *(v.(*directPass))
	}
	return directPass{}
}

// diskBaselined reports whether a disk has already recorded its initial full
// sync. Used to emit the "copy complete" event exactly once.
func (s *Server) diskBaselined(migID, diskID int64) bool {
	m, err := s.st.Migration(s.ctx, migID)
	if err != nil {
		return false
	}
	for _, d := range m.Disks {
		if d.ID == diskID {
			return d.FullSyncDone
		}
	}
	return false
}

// noteDirectStreaming records that we just handed this disk's agent a redirect
// (so the console can say the first pass is streaming) and announces the direct
// copy ONCE per destination — the operator must be able to see that the
// appliance deliberately withholds the "copy complete" verdict until the agent
// confirms a finished pass.
func (s *Server) noteDirectStreaming(migID, diskID int64, target string, h protocol.Hello) {
	p := s.directPassFor(diskID)
	first := p.streamingSince.IsZero()
	if first {
		p.streamingSince = time.Now()
	}
	p.agentReports = h.ReportsPasses
	s.directPasses.Store(diskID, &p)
	if first {
		_ = s.st.AddEvent(s.ctx, migID, "info", fmt.Sprintf(
			"the agent is copying files straight to the destination %s — nothing is staged here, so the copy is reported COMPLETE only when the agent confirms a finished pass (checked every ~60s). Do not power off the source until the \"Initial file copy complete\" check is green.", target))
	}
}

// fileCopyDetail is the honest status line for a file migration's cutover gate.
// In DIRECT mode the appliance never sees the data, so "not baselined" has to say
// what is actually happening — a first pass streaming to the destination (which
// can take hours on a large source), an agent too old to confirm it, or a walk
// that ended early — instead of a bare "copying files" that looks like progress.
func (s *Server) fileCopyDetail(m api.Migration, allFull bool) string {
	if allFull {
		return "files copied"
	}
	if !s.fileAutomation() {
		return "copying files" // appliance-staging fallback: wording unchanged
	}
	var p directPass
	for _, d := range m.Disks {
		if q := s.directPassFor(d.ID); !q.streamingSince.IsZero() {
			p = q
		}
	}
	switch {
	case p.streamingSince.IsZero():
		return "copying files"
	case !p.agentReports:
		return "the source agent is an older build that cannot confirm the copy finished — re-run the enrollment command on the source to update it (no re-copy: the delta checkpoint is kept)"
	case p.lastSeq > 0 && !p.lastComplete:
		return fmt.Sprintf("the last pass ended early (%d items) — cutover needs ONE complete pass; the agent retries every ~60s", p.lastEntries)
	default:
		return fmt.Sprintf("first copy pass streaming to the destination (%s so far) — it must finish before cutover", time.Since(p.streamingSince).Round(time.Minute))
	}
}

// recordDirectFilePass records a completed direct-mode copy pass reported by the
// agent in its Hello. It is the ONLY thing that can baseline a direct file
// migration: nothing else on the appliance can know the destination has the
// files, because the data never passes through here.
//
// Guards, in order:
//   - file sessions only (a block Hello never reaches the redirect anyway);
//   - a destination must exist — in the appliance-staging fallback there is none
//     and onComplete owns the baseline, so we must not double-count;
//   - the report must name the destination we are CURRENTLY streaming to. A pass
//     that landed on a destination since deleted or relaunched says nothing about
//     the new one;
//   - only a COMPLETE pass (the agent walked the whole tree) baselines; an
//     incomplete pass records its stats and warns;
//   - each pass is credited once: the agent repeats its last pass in EVERY Hello,
//     so without the sequence guard a string of failures would keep re-stamping
//     last_sync_at and make the RPO look healthy.
func (s *Server) recordDirectFilePass(migID, diskID int64, dev0 string, h protocol.Hello) {
	if h.Mode != protocol.ModeFile {
		return
	}
	v, ok := s.fileDests.Load(migID)
	if !ok {
		return // fallback / no destination: onComplete owns the baseline
	}
	d := v.(*fileDest)
	p := s.directPassFor(diskID)
	p.agentReports = h.ReportsPasses
	defer func() { s.directPasses.Store(diskID, &p) }()

	// Untrusted counters from an (authenticated) agent: ignore nonsense rather
	// than writing it to the store.
	if h.LastPassSeq <= 0 || h.LastPassEntries < 0 || h.LastPassBytes < 0 {
		return // nothing reported (older agent), or no pass has finished yet
	}
	if d.ip == "" || h.LastPassTarget != net.JoinHostPort(d.ip, strconv.Itoa(destFilePort)) {
		return // the report is about a different receiver; it proves nothing here
	}
	if h.LastPassSeq <= p.lastSeq {
		return // already recorded
	}
	wasBaselined := s.diskBaselined(migID, diskID)
	p.lastSeq, p.lastEntries, p.lastBytes = h.LastPassSeq, h.LastPassEntries, h.LastPassBytes
	p.lastComplete, p.lastAt = h.LastPassComplete, time.Unix(h.LastPassAt, 0)

	if err := s.st.RecordDiskSync(s.ctx, migID, diskID, h.LastPassComplete,
		h.LastPassEntries, h.LastPassEntries, h.LastPassBytes); err != nil {
		log.Printf("appliance: record direct file pass (migration %d disk %d): %v", migID, diskID, err)
		return
	}
	switch {
	case h.LastPassComplete && !wasBaselined:
		_ = s.st.AddEvent(s.ctx, migID, "info", fmt.Sprintf(
			"file copy complete: %d items (%s) copied to the destination %s — ready to cut over",
			h.LastPassEntries, humanBytes(h.LastPassBytes), d.ip))
	case !h.LastPassComplete:
		_ = s.st.AddEvent(s.ctx, migID, "warn", fmt.Sprintf(
			"the file copy pass ended before the whole source tree was walked (%d items, %s) — the copy is NOT complete; the agent retries on its next pass (~60s)",
			h.LastPassEntries, humanBytes(h.LastPassBytes)))
	}
}

// isPrivateIP reports whether a is an RFC1918 / link-local address (so we pick
// the destination's PUBLIC IPv4 for the agent to reach).
func isPrivateIP(a string) bool {
	ip := net.ParseIP(a)
	if ip == nil {
		return true
	}
	return ip.IsPrivate() || ip.IsLoopback() || ip.IsLinkLocalUnicast()
}
