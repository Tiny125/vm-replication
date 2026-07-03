package appliance

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"time"
)

// Disk-boot cutover, image streaming ("Variant B").
//
// A Linode local disk can only be written from INSIDE the instance, so the
// cutover boots the destination into RESCUE MODE (Finnix — always boots, never
// touches the migrated OS) with the blank local disk as /dev/sda, and the
// operator pastes ONE command in its Lish console. That command downloads a
// small script from the appliance which streams the converted image straight
// off the appliance's own replication volume onto /dev/sda, grows the root to
// fill the disk, and powers the instance off — the appliance's "copy done"
// signal, after which it boots the instance from the local disk.
//
// Streaming from the appliance's (fully hydrated) volume replaces the previous
// design — clone the volume, boot the MIGRATED OS from the clone, and rely on
// an injected systemd one-shot to copy and power off — which was slow (fresh
// clone reads) and fragile (required the migrated OS to boot and run the unit).
//
// Both endpoints are token-gated: the token is minted per cutover run, is
// unguessable (128-bit), expires with the copy budget, and is dropped as soon
// as the cutover finishes or fails.

// cutoverStream describes one authorized image download.
type cutoverStream struct {
	migID   int64
	path    string // device (or file-fallback image) to stream from
	bytes   int64  // exact byte count to send (the shrunk filesystem, not the whole volume)
	expires time.Time
}

// registerCutoverStream mints a token authorizing the image download for one
// migration's cutover and returns it.
func (s *Server) registerCutoverStream(migID int64, path string, bytes int64, ttl time.Duration) string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	tok := hex.EncodeToString(b)
	s.cutoverStreams.Store(tok, &cutoverStream{migID: migID, path: path, bytes: bytes, expires: time.Now().Add(ttl)})
	return tok
}

// lookupCutoverStream resolves a token, enforcing expiry.
func (s *Server) lookupCutoverStream(token string) (*cutoverStream, bool) {
	v, ok := s.cutoverStreams.Load(token)
	if !ok {
		return nil, false
	}
	st := v.(*cutoverStream)
	if time.Now().After(st.expires) {
		s.cutoverStreams.Delete(token)
		return nil, false
	}
	return st, true
}

// dropCutoverStream invalidates a migration's stream token(s) and console copy
// command — called when its cutover finishes, fails, or is cancelled.
func (s *Server) dropCutoverStream(migID int64) {
	s.cutoverStreams.Range(func(k, v any) bool {
		if v.(*cutoverStream).migID == migID {
			s.cutoverStreams.Delete(k)
		}
		return true
	})
	s.cutoverCmds.Delete(migID)
}

// setCutoverCopyCmd records the one-line Lish command the console shows while a
// disk-boot cutover waits for the operator to run the copy.
func (s *Server) setCutoverCopyCmd(migID int64, cmd string) { s.cutoverCmds.Store(migID, cmd) }

// cutoverCopyCmdFor returns that command, or "" when no copy is pending.
func (s *Server) cutoverCopyCmdFor(migID int64) string {
	if v, ok := s.cutoverCmds.Load(migID); ok {
		return v.(string)
	}
	return ""
}

// cutoverCopyCmd is the ONE line the operator pastes into the rescue'd
// instance's Lish console. Key-pinned like the enrollment command.
func (s *Server) cutoverCopyCmd(token string) string {
	return fmt.Sprintf("curl -fsSL %s'%s://%s:%d/cutover/copy.sh?token=%s' | sh",
		s.curlPinFlag(), s.scheme(), s.cfg.PublicHost, s.cfg.ConsolePort, token)
}

// rescueCopyScript is the script that command downloads and runs inside the
// Finnix rescue environment. It must stay /bin/sh-compatible.
func (s *Server) rescueCopyScript(token string, bytes int64) string {
	imageURL := fmt.Sprintf("%s://%s:%d/cutover/image?token=%s", s.scheme(), s.cfg.PublicHost, s.cfg.ConsolePort, token)
	return fmt.Sprintf(`#!/bin/sh
# vm-replication disk-boot cutover: stream the migrated image onto this
# instance's local disk (/dev/sda), grow the root, and power off. Run this in
# the RESCUE MODE Lish console of the cutover instance — nowhere else.
set -e
[ -b /dev/sda ] || { echo "vmrepl-cutover: /dev/sda not found - run this inside the RESCUE MODE cutover instance"; exit 1; }
echo "vmrepl-cutover: streaming %s onto /dev/sda from the appliance (live progress below)..."
curl -fsSN %s'%s' | dd of=/dev/sda bs=4M conv=fsync status=progress
sync
# Grow a whole-disk ext root to fill the local disk (offline; no-op otherwise).
if command -v resize2fs >/dev/null 2>&1; then
  e2fsck -fy /dev/sda >/dev/null 2>&1 || true
  resize2fs /dev/sda >/dev/null 2>&1 || true
fi
echo "vmrepl-cutover: copy complete - powering off; the appliance now boots this instance from its local disk"
sleep 2
poweroff
`, humanBytes(bytes), s.curlPinFlag(), imageURL)
}

// handleCutoverScript serves the rescue copy script (GET /cutover/copy.sh,
// token-gated — the rescue instance has no console session, the token IS the
// auth, exactly like the enrollment endpoints).
func (s *Server) handleCutoverScript(w http.ResponseWriter, r *http.Request) {
	token := r.URL.Query().Get("token")
	st, ok := s.lookupCutoverStream(token)
	if !ok {
		writeErr(w, http.StatusForbidden, "invalid or expired cutover token — use the exact command shown on the migration card (Retry cutover mints a new one)")
		return
	}
	w.Header().Set("Content-Type", "text/x-shellscript; charset=utf-8")
	_, _ = w.Write([]byte(s.rescueCopyScript(token, st.bytes)))
}

// handleCutoverImage streams the converted image (GET /cutover/image). Exactly
// st.bytes are sent — the shrunk filesystem, not the whole volume — with a
// Content-Length so the guest-side download can detect truncation.
func (s *Server) handleCutoverImage(w http.ResponseWriter, r *http.Request) {
	token := r.URL.Query().Get("token")
	st, ok := s.lookupCutoverStream(token)
	if !ok {
		writeErr(w, http.StatusForbidden, "invalid or expired cutover token")
		return
	}
	f, err := os.Open(st.path)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "cannot open the image device: "+err.Error())
		return
	}
	defer f.Close()
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Length", fmt.Sprintf("%d", st.bytes))
	if _, err := io.CopyN(w, f, st.bytes); err != nil {
		// Mid-stream failure: the connection is already committed, so just log —
		// the guest-side dd fails short and the operator re-pastes the command.
		log.Printf("appliance: cutover image stream for migration %d aborted: %v", st.migID, err)
	}
}
