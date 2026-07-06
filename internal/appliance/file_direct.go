package appliance

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net"
	"net/http"
	"os"
	"time"

	"github.com/tiny125/vm-replication/internal/api"
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
	ready      bool // the destination's receiver is reachable on destFilePort
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

// ensureFileDestination launches the destination for a file migration (once)
// and, in the background, waits for its receiver to come up so fileDataTarget
// starts redirecting the agent to it. Idempotent per migration.
func (s *Server) ensureFileDestination(m api.Migration) {
	if !isFileMethod(m.BootTarget) {
		return
	}
	if _, ok := s.linodeClient(s.ctx); !ok || s.cfg.ApplianceLinodeID == 0 {
		return // no automation: the appliance-staging fallback handles it
	}
	if _, loaded := s.fileDests.LoadOrStore(m.ID, &fileDest{}); loaded {
		return // already launching/launched
	}
	go s.launchFileDestination(m)
}

// launchFileDestination creates the destination from the OS image with cloud-init
// that installs + runs the receiver, then polls until its receiver port answers.
func (s *Server) launchFileDestination(m api.Migration) {
	ctx := s.ctx
	cl, ok := s.linodeClient(ctx)
	if !ok {
		return
	}
	label := cutoverInstanceLabel("", m.Name)
	region := s.cfg.Region
	if inst, err := cl.GetInstance(ctx, s.cfg.ApplianceLinodeID); err == nil && inst.Region != "" {
		region = inst.Region
	}
	// A strong root password so the operator can log in (reset in Cloud Manager);
	// never logged.
	rootPass := randPassword()

	// Token-gated bootstrap so the destination can pull the receiver binary + certs.
	tok := s.registerDestBootstrap(m.ID, 6*time.Hour)
	userData := s.destCloudInit(tok)

	inst, err := cl.CreateInstanceFromImageUserData(ctx, label, region, m.LinodeType, m.OSImage, rootPass, userData)
	if err != nil {
		s.fileDests.Delete(m.ID)
		s.fail(m.ID, "launch destination: "+err.Error())
		return
	}
	s.fileDests.Store(m.ID, &fileDest{instanceID: inst.ID})
	_ = s.st.SetMigrationImage(ctx, m.ID, "file:direct", inst.ID)
	_ = s.st.AddEvent(ctx, m.ID, "info", fmt.Sprintf("launched destination %q (id %d) from image %s on plan %s — installing the file receiver, then the agent will copy straight into it", label, inst.ID, m.OSImage, m.LinodeType))

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
		_ = s.st.AddEvent(ctx, m.ID, "warn", "could not determine the destination's public IP yet; will keep the agent holding")
		return
	}
	s.fileDests.Store(m.ID, &fileDest{instanceID: inst.ID, ip: ip})

	// Poll the receiver port: cloud-init needs a few minutes to install + start it.
	addr := net.JoinHostPort(ip, fmt.Sprintf("%d", destFilePort))
	deadline := time.Now().Add(15 * time.Minute)
	for time.Now().Before(deadline) {
		if ctx.Err() != nil {
			return
		}
		c, err := net.DialTimeout("tcp", addr, 5*time.Second)
		if err == nil {
			_ = c.Close()
			s.fileDests.Store(m.ID, &fileDest{instanceID: inst.ID, ip: ip, ready: true})
			_ = s.st.AddEvent(ctx, m.ID, "info", fmt.Sprintf("destination %s is ready to receive — the agent will now copy your files straight into it", addr))
			return
		}
		time.Sleep(10 * time.Second)
	}
	_ = s.st.AddEvent(ctx, m.ID, "warn", fmt.Sprintf("the destination's file receiver did not come up on %s within 15m. Check its Lish console (cloud-init log: /var/log/vmrepl-dest.log). If the image lacks cloud-init/metadata support, the receiver can't auto-install — tell us and we'll add a manual paste fallback.", addr))
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
exec >>/var/log/vmrepl-dest.log 2>&1
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
`, pin, base, token, pin, base, token, destFilePort, destFilePort)
	return script
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
