package appliance

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/tiny125/vm-replication/internal/api"
	"github.com/tiny125/vm-replication/internal/blockdiff"
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
// migration's cutover and returns it. This is also the start of a fresh
// cutover COPY ATTEMPT (first try or a Retry after failure), so it resets
// this migration's copy-progress tracking (see resetCutoverCopy) to a clean
// "waiting for paste" state with the known total.
func (s *Server) registerCutoverStream(migID int64, path string, bytes int64, ttl time.Duration) string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	tok := hex.EncodeToString(b)
	expires := time.Now().Add(ttl)
	s.cutoverStreams.Store(tok, &cutoverStream{migID: migID, path: path, bytes: bytes, expires: expires})
	s.persistCutoverStream(migID, tok, path, bytes, expires, "")
	s.resetCutoverCopy(migID, bytes)
	return tok
}

// ---- Cutover copy progress: real feedback during the Lish paste ----
//
// A real user pasted the copy command into Lish and had no idea what was
// happening, whether to close the Weblish window, or what came next — the
// appliance HAD this information (the exact byte total, known before the
// paste; every byte it streams) and threw it away. cutoverCopyState tracks
// it so the console can show three real states: waiting for the paste,
// copying N% (with byte counts/elapsed/ETA), and finished.
//
// Kept in Server.cutoverCopy, a SEPARATE sync.Map from cutoverStreams/
// cutoverCmds: those are erased by dropCutoverStream the INSTANT the bytes
// finish sending (so the operator's Lish command stops working and the
// action-needed banner disappears) — which would erase "finished" before the
// console ever got to render it. This mirrors the existing
// setCutoverFreezing/cutoverFreezingFor pattern (a sync.Map of migID ->
// phase) rather than inventing a new shape.
//
// In-memory only, like the maps it mirrors: an appliance restart also kills
// the operator's in-flight pasted `sh`, so there is no valid percentage to
// resume — see cutoverCopyMarkInterrupted, called from restoreCutoverStream.
type cutoverCopyState struct {
	mu         sync.Mutex
	total      int64     // exact bytes expected this attempt (known before the paste)
	sent       int64     // bytes served so far THIS request/attempt
	attempt    int       // increments each time a fresh /cutover/image request starts (paste or re-paste)
	pastedAt   time.Time // handleCutoverScript was hit (script downloaded) — zero until then; set once, idempotent
	startedAt  time.Time // first byte of the CURRENT attempt was served — zero until then
	finishedAt time.Time // explicitly marked done (instance confirmed powered off) — zero until then

	// interrupted is set by cutoverCopyMarkInterrupted after an appliance
	// restart lost this migration's in-memory progress. The console must say
	// the copy was interrupted and needs a fresh paste — never fabricate a
	// resumed percentage.
	interrupted bool
}

// resetCutoverCopy starts a fresh copy-progress attempt for a migration: a
// clean "waiting for paste" state with the known total. Called by
// registerCutoverStream (every fresh cutover / Retry mints a new stream).
func (s *Server) resetCutoverCopy(migID, total int64) {
	s.cutoverCopy.Store(migID, &cutoverCopyState{total: total})
}

// cutoverCopyMarkInterrupted records that an appliance restart lost this
// migration's in-flight copy progress (the counter is in-memory only, and a
// restart also kills the pasted command's live HTTP connection, so the
// operator must re-paste regardless of how far the old process had gotten).
// Called from restoreCutoverStream when a persisted stream/cmd survives the
// restart (so a fresh paste will work) but the byte-progress that went with
// it cannot — the console must show "interrupted, please re-paste", never a
// resumed percentage.
func (s *Server) cutoverCopyMarkInterrupted(migID, total int64) {
	s.cutoverCopy.Store(migID, &cutoverCopyState{total: total, interrupted: true})
}

// cutoverCopyMarkPasted records that the copy SCRIPT was downloaded — i.e.
// the operator pasted the command. Idempotent (curl can retry the script
// fetch): it only flips pastedAt on the FIRST call, and that return value is
// what the caller uses to log exactly one "pasted" event instead of one per
// retry.
func (s *Server) cutoverCopyMarkPasted(migID int64) bool {
	v, ok := s.cutoverCopy.Load(migID)
	if !ok {
		return false // no attempt registered (unknown/expired) — nothing to mark
	}
	cs := v.(*cutoverCopyState)
	cs.mu.Lock()
	defer cs.mu.Unlock()
	if !cs.pastedAt.IsZero() {
		return false
	}
	cs.pastedAt = time.Now()
	return true
}

// cutoverCopyBeginRequest resets the byte counter at the start of EVERY
// /cutover/image request: a re-paste re-downloads the image from byte 0, so
// the percentage must restart there rather than accumulate past 100% across
// attempts. Also bumps the attempt counter and clears any stale finishedAt
// from a previous (interrupted) attempt.
func (s *Server) cutoverCopyBeginRequest(migID int64) {
	v, ok := s.cutoverCopy.Load(migID)
	if !ok {
		return
	}
	cs := v.(*cutoverCopyState)
	cs.mu.Lock()
	defer cs.mu.Unlock()
	cs.sent = 0
	cs.attempt++
	cs.startedAt = time.Now()
	cs.finishedAt = time.Time{}
}

// cutoverCopyAddBytes records n more bytes served for the CURRENT attempt.
func (s *Server) cutoverCopyAddBytes(migID, n int64) {
	v, ok := s.cutoverCopy.Load(migID)
	if !ok {
		return
	}
	cs := v.(*cutoverCopyState)
	cs.mu.Lock()
	defer cs.mu.Unlock()
	cs.sent += n
}

// cutoverCopyMarkFinished marks the copy step as fully done. Called from
// migrations.go once the instance is confirmed POWERED OFF (the true
// "copy done" signal — the pasted script's last action) — NOT merely once
// all bytes have been handed to the network, which only proves the
// appliance's send side is done. dropCutoverStream already erased the
// stream/cmd by the time this runs, which is exactly why this state lives
// in its own map.
func (s *Server) cutoverCopyMarkFinished(migID int64) {
	v, ok := s.cutoverCopy.Load(migID)
	if !ok {
		return
	}
	cs := v.(*cutoverCopyState)
	cs.mu.Lock()
	defer cs.mu.Unlock()
	cs.finishedAt = time.Now()
}

// cutoverCopyView is a point-in-time, lock-free snapshot for the console/API
// view and for building activity-log/failure messages.
type cutoverCopyView struct {
	Phase       string // "waiting" | "copying" | "finished"
	SentBytes   int64
	TotalBytes  int64
	Attempt     int
	ElapsedSecs int64
	ETASecs     int64 // -1 when unknown
	Interrupted bool
}

// cutoverCopyViewFor snapshots a migration's current copy progress. ok is
// false only when no copy has ever been registered for this migration (e.g.
// it hasn't reached the disk-boot copy step yet, or never will — volume
// boot).
func (s *Server) cutoverCopyViewFor(migID int64) (cutoverCopyView, bool) {
	v, ok := s.cutoverCopy.Load(migID)
	if !ok {
		return cutoverCopyView{}, false
	}
	cs := v.(*cutoverCopyState)
	cs.mu.Lock()
	defer cs.mu.Unlock()
	out := cutoverCopyView{
		SentBytes:   cs.sent,
		TotalBytes:  cs.total,
		Attempt:     cs.attempt,
		Interrupted: cs.interrupted,
		ETASecs:     -1,
	}
	switch {
	case !cs.finishedAt.IsZero():
		out.Phase = "finished"
		if !cs.startedAt.IsZero() {
			out.ElapsedSecs = int64(cs.finishedAt.Sub(cs.startedAt).Seconds())
		}
	case !cs.startedAt.IsZero():
		out.Phase = "copying"
		elapsed := time.Since(cs.startedAt)
		out.ElapsedSecs = int64(elapsed.Seconds())
		if cs.sent > 0 && cs.total > cs.sent && elapsed.Seconds() > 0 {
			rate := float64(cs.sent) / elapsed.Seconds()
			if rate > 0 {
				out.ETASecs = int64(float64(cs.total-cs.sent) / rate)
			}
		}
	default:
		out.Phase = "waiting"
	}
	return out, true
}

// cutoverCopyProgressSummary renders a short human summary of a copy's
// progress, used both for periodic activity-log events during the wait and
// for the copy-budget-timeout failure message — so a timeout says exactly
// how far the copy actually got instead of a generic "it didn't finish".
func cutoverCopyProgressSummary(cv cutoverCopyView) string {
	switch cv.Phase {
	case "copying":
		msg := fmt.Sprintf("copying — %s of %s", humanBytes(cv.SentBytes), humanBytes(cv.TotalBytes))
		if cv.TotalBytes > 0 {
			msg = fmt.Sprintf("copying — %.1f%% (%s of %s)", float64(cv.SentBytes)/float64(cv.TotalBytes)*100, humanBytes(cv.SentBytes), humanBytes(cv.TotalBytes))
		}
		if cv.ElapsedSecs > 0 {
			msg += fmt.Sprintf(", elapsed %s", (time.Duration(cv.ElapsedSecs) * time.Second).Round(time.Second))
		}
		if cv.ETASecs >= 0 {
			msg += fmt.Sprintf(", ETA ~%s", (time.Duration(cv.ETASecs) * time.Second).Round(time.Second))
		}
		return msg
	case "finished":
		return "the copy finished; finishing up (attaching data volumes and booting)"
	default: // "waiting", or no copy registered yet
		return "still waiting for the copy command to be pasted in the instance's Lish console"
	}
}

// ---- F-24: survive an appliance restart mid-cutover -----------------------
//
// A disk-boot cutover parks in `migrating` with a live rescue instance while
// it waits for the operator to paste the Lish copy command. Before this, the
// token (registerCutoverStream) and the command text (setCutoverCopyCmd)
// lived ONLY in memory, so restarting applianced during that wait (an
// upgrade, a crash, a service restart) dropped both — measured live:
//
//	state: migrating | phase: finalizing (convert + clone)
//	copy cmd present: False        <- the card no longer shows a command
//	launched id: 103564624         <- the rescue instance is still running
//
// leaving the migration unfinishable with nothing explaining why (recovery
// was Stop-then-Start, undiscoverable from the UI).
//
// persistedCutoverStream is the durable, JSON-encoded copy of a
// cutoverStream plus its console copy command, stored per-migration in the
// existing settings table (no schema migration / new table). It is written on
// register/setCmd and read back at startup by restoreCutoverStream.
//
// Security note: the token is a bearer credential (anyone holding it can
// download the migrated image via /cutover/image), so this now stores it at
// rest in the appliance's SQLite database. That is an accepted, deliberate
// trade-off — the same database already holds the Linode API token at rest
// (see SetLinodeToken) — made so a restart can recover an in-flight cutover
// instead of stranding it.
type persistedCutoverStream struct {
	Token   string `json:"token"`
	Path    string `json:"path"`
	Bytes   int64  `json:"bytes"`
	Expires int64  `json:"expires"` // unix seconds
	Cmd     string `json:"cmd"`     // the Lish copy command shown on the card; "" until setCutoverCopyCmd runs
}

// cutoverStreamSettingKey is the settings-table key holding one migration's
// persisted cutover stream (see persistedCutoverStream).
func cutoverStreamSettingKey(migID int64) string {
	return fmt.Sprintf("cutover_stream:%d", migID)
}

// persistCutoverStream writes (or updates) the durable copy of a migration's
// cutover stream so restoreCutoverStream can rebuild it after a restart. A nil
// store (bare Server{} used by unit tests that only exercise the in-memory
// registry) is a deliberate no-op, not an error.
func (s *Server) persistCutoverStream(migID int64, token, path string, bytes int64, expires time.Time, cmd string) {
	if s.st == nil {
		return
	}
	rec := persistedCutoverStream{Token: token, Path: path, Bytes: bytes, Expires: expires.Unix(), Cmd: cmd}
	b, err := json.Marshal(rec)
	if err != nil {
		log.Printf("appliance: persist cutover stream for migration %d: %v", migID, err)
		return
	}
	if err := s.st.SetSetting(s.ctx, cutoverStreamSettingKey(migID), string(b)); err != nil {
		log.Printf("appliance: persist cutover stream for migration %d: %v", migID, err)
	}
}

// restoreCutoverStream rebuilds a migration's in-memory cutover stream + copy
// command from the durable settings-store copy, called from
// StartActiveReceivers so a disk-boot cutover waiting on the operator's Lish
// paste survives an appliance restart. Returns false — and clears the
// persisted entry — when there is nothing persisted, it can't be decoded, or
// its expiry has already passed: an expired token would just 403 the moment
// it's pasted, so restoring it would show a copy command that can never work.
func (s *Server) restoreCutoverStream(migID int64) bool {
	if s.st == nil {
		return false
	}
	v, ok, err := s.st.GetSetting(s.ctx, cutoverStreamSettingKey(migID))
	if err != nil || !ok {
		return false
	}
	var rec persistedCutoverStream
	if err := json.Unmarshal([]byte(v), &rec); err != nil {
		log.Printf("appliance: restore cutover stream for migration %d: %v", migID, err)
		_ = s.st.DeleteSetting(s.ctx, cutoverStreamSettingKey(migID))
		return false
	}
	expires := time.Unix(rec.Expires, 0)
	if !time.Now().Before(expires) {
		// Past its budget — restoring it would only show a copy command whose
		// token 403s immediately. Clear it so the console and the state machine
		// agree there is nothing left to resume.
		_ = s.st.DeleteSetting(s.ctx, cutoverStreamSettingKey(migID))
		return false
	}
	s.cutoverStreams.Store(rec.Token, &cutoverStream{migID: migID, path: rec.Path, bytes: rec.Bytes, expires: expires})
	if rec.Cmd != "" {
		s.cutoverCmds.Store(migID, rec.Cmd)
	}
	// The copy command survives the restart, but any in-flight copy's
	// progress does not (in-memory only, and the restart also killed the
	// pasted `sh`'s live HTTP connection) — the console must say the copy
	// was interrupted and needs a fresh paste, never fabricate how far the
	// old process had gotten.
	s.cutoverCopyMarkInterrupted(migID, rec.Bytes)
	return true
}

// restoreCutoverState is called from StartActiveReceivers for every migration
// found in `migrating` at startup. Only disk-boot cutover ever streams via a
// Lish copy command, so a volume-boot migration (or anything mid-finalize
// before a stream would even exist yet) is left alone. When a stream can be
// restored, the console gets its copy command back and the operator is told
// so. When it can't AND a rescue instance is already running (LaunchedID
// set), this is the F-24 safety net: name the situation and the exact
// recovery action on the migration's own activity log — matching the tone of
// the restart message in StartActiveReceivers (say what happened, and
// reassure/point at the fix) — instead of leaving the operator to discover a
// vanished copy command with no explanation.
func (s *Server) restoreCutoverState(m api.Migration) {
	if m.BootTarget != api.BootTargetDisk {
		return
	}
	if s.restoreCutoverStream(m.ID) {
		_ = s.st.AddEvent(s.ctx, m.ID, "info",
			"the appliance service restarted — the pending rescue-mode copy command has been restored below. If you already pasted the previous one, paste this one instead (the old token stopped working).")
		return
	}
	if m.LaunchedID != 0 {
		_ = s.st.AddEvent(s.ctx, m.ID, "warn",
			"the appliance service restarted while this migration was mid-cutover, and its rescue-mode copy command could not be recovered (it had already expired, or was never issued yet). This card cannot finish the cutover as-is: click Stop, then Start the migration again to launch a fresh cutover from a clean state.")
	}
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
// command — called when its cutover finishes, fails, or is cancelled. Also
// clears the durable copy (see persistCutoverStream): once a cutover is done,
// a later restart must not resurrect it.
func (s *Server) dropCutoverStream(migID int64) {
	s.cutoverStreams.Range(func(k, v any) bool {
		if v.(*cutoverStream).migID == migID {
			s.cutoverStreams.Delete(k)
		}
		return true
	})
	s.cutoverCmds.Delete(migID)
	if s.st != nil {
		_ = s.st.DeleteSetting(s.ctx, cutoverStreamSettingKey(migID))
	}
}

// setCutoverCopyCmd records the one-line Lish command the console shows while a
// disk-boot cutover waits for the operator to run the copy, and fills the Cmd
// field into the persisted record registerCutoverStream already wrote (the
// command text isn't known yet at register time).
func (s *Server) setCutoverCopyCmd(migID int64, cmd string) {
	s.cutoverCmds.Store(migID, cmd)
	if s.st == nil {
		return
	}
	v, ok, err := s.st.GetSetting(s.ctx, cutoverStreamSettingKey(migID))
	if err != nil || !ok {
		return
	}
	var rec persistedCutoverStream
	if err := json.Unmarshal([]byte(v), &rec); err != nil {
		log.Printf("appliance: update persisted cutover command for migration %d: %v", migID, err)
		return
	}
	rec.Cmd = cmd
	b, err := json.Marshal(rec)
	if err != nil {
		log.Printf("appliance: update persisted cutover command for migration %d: %v", migID, err)
		return
	}
	if err := s.st.SetSetting(s.ctx, cutoverStreamSettingKey(migID), string(b)); err != nil {
		log.Printf("appliance: update persisted cutover command for migration %d: %v", migID, err)
	}
}

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
	return fmt.Sprintf("curl -fsSL %s'%s/cutover/copy.sh?token=%s' | sh",
		s.curlPinFlag(), s.consoleBase(), token)
}

// selectTargetDiskFunc is the POSIX-sh disk-selection logic used by the
// rescue copy script below. F-23: Finnix does not reliably enumerate the
// destination's local disk as /dev/sda — on the same migration, consecutive
// rescue boots put it at /dev/sdb and then /dev/sdg, with the unused slots
// showing up as zero-byte, read-only devices ahead of it. Hardcoding any node
// name is therefore not a fix; instead we identify the disk by its
// properties: it must be a whole disk that is writable and whose size is
// close to the image the appliance is about to stream (the appliance already
// knows that byte count exactly).
//
// Defined as its own shell function with no fmt.Sprintf placeholders inside
// it (the numeric arguments are passed in as positional parameters at call
// time), so cutover_stream_test.go can lift this exact text out, drop it into
// a standalone script alongside a fake `lsblk`, and exercise the selection
// logic directly — without having to string-match the assembled script or
// stand up a real rescue environment.
const selectTargetDiskFunc = `
# select_target_disk EXPECT_BYTES
#
# Finds the one local disk to write the migrated image onto, using
# "lsblk -bdno NAME,SIZE,RO" (byte-exact sizes, no header row; -d already
# restricts the listing to whole disks, never partitions).
#
# A candidate must be:
#   - not sr*/zram*/loop* (optical/zram/loop devices lsblk -d can still list)
#   - writable (RO == 0)
#   - at least EXPECT_BYTES in size, so the image fits. The plan's disk is
#     normally BIGGER than the image; only a disk too small to hold it is
#     disqualified.
#
# On exactly one candidate: prints "/dev/NAME" on stdout and returns 0.
# On zero or more than one candidate: prints nothing on stdout, prints every
# disk seen (name, size, ro) plus what was being looked for on stderr, and
# returns 1. It never guesses.
select_target_disk() {
  expect=$1
  selected=""
  count=0
  seen=""
  while read -r name size ro; do
    [ -n "$name" ] || continue
    seen="$seen
  /dev/$name size=$size ro=$ro"
    case "$name" in
      sr*|zram*|loop*) continue ;;
    esac
    [ "$ro" = "0" ] || continue
    [ "$size" -ge "$expect" ] || continue
    selected="/dev/$name"
    count=$((count + 1))
  done <<EOF
$(lsblk -bdno NAME,SIZE,RO)
EOF
  if [ "$count" -ne 1 ]; then
    echo "vmrepl-cutover: could not identify the target disk - looking for exactly one writable whole disk of at least ${expect} bytes, found ${count}. Disks seen:${seen}" >&2
    return 1
  fi
  echo "$selected"
  return 0
}
`

// rescueCopyScript is the script that command downloads and runs inside the
// Finnix rescue environment. It must stay /bin/sh-compatible.
func (s *Server) rescueCopyScript(token string, bytes int64) string {
	imageURL := fmt.Sprintf("%s/cutover/image?token=%s", s.consoleBase(), token)
	// TOLERANCE_BYTES: Linode disk sizes are rounded to a whole MB, so a
	// freshly created disk can legitimately land up to just under 1 MiB away
	// from the image byte count; 2 MiB gives that a little headroom without
	// being loose enough to risk matching the wrong disk.
	return fmt.Sprintf(`#!/bin/sh
# vm-replication disk-boot cutover: stream the migrated image onto this
# instance's local disk, grow the root, and power off. Run this in the RESCUE
# MODE Lish console of the cutover instance — nowhere else.
#
# The local disk is NOT assumed to be /dev/sda: Finnix does not reliably
# enumerate it there (see F-23), so it is identified below by its properties
# (writable whole disk, size matching the image) instead of by name.
set -e
%s
TARGET=$(select_target_disk %d) || exit 1
echo "vmrepl-cutover: selected $TARGET as the target disk (writable whole disk large enough for the %s image)"
echo "vmrepl-cutover: streaming %s onto $TARGET from the appliance (live progress below)..."
curl -fsSN %s'%s' | dd of=$TARGET bs=4M conv=fsync status=progress
sync
# Grow a whole-disk ext root to fill the local disk (offline; no-op otherwise).
if command -v resize2fs >/dev/null 2>&1; then
  e2fsck -fy $TARGET >/dev/null 2>&1 || true
  resize2fs $TARGET >/dev/null 2>&1 || true
fi
echo "vmrepl-cutover: copy complete - powering off; the appliance now boots this instance from its local disk"
sleep 2
poweroff
`, selectTargetDiskFunc, bytes, humanBytes(bytes), humanBytes(bytes), s.curlPinFlag(), imageURL)
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
	// Record that the paste happened — idempotently, since curl can retry the
	// script fetch — so the console can move off "waiting for paste" and the
	// activity log gets exactly one "pasted" event, not one per retry.
	if s.cutoverCopyMarkPasted(st.migID) && s.st != nil {
		_ = s.st.AddEvent(s.ctx, st.migID, "info", "the copy command was pasted in the instance's Lish console — streaming the image now")
	}
	w.Header().Set("Content-Type", "text/x-shellscript; charset=utf-8")
	_, _ = w.Write([]byte(s.rescueCopyScript(token, st.bytes)))
}

// cutoverImageChunkBytes is how much handleCutoverImage streams per io.CopyN
// call (see streamImageChunked): small enough to report progress well before
// a multi-GiB transfer finishes, but large enough that each call still
// qualifies for the zero-copy sendfile fast path (io.CopyN from an *os.File
// to an http.ResponseWriter that implements io.ReaderFrom) — wrapping the
// writer in a plain counting io.Writer would silently disable that for a
// 30 GiB transfer. Var, not const, so tests can shrink it and exercise many
// chunks without serving a real 32 MiB+ file.
var cutoverImageChunkBytes int64 = 32 << 20

// streamImageChunked copies exactly total bytes from src to dst in
// chunkSize-sized io.CopyN calls, invoking onChunk with the number of bytes
// each call actually sent (so a caller can track progress incrementally
// instead of learning the total only after everything has been sent).
func streamImageChunked(dst io.Writer, src io.Reader, total, chunkSize int64, onChunk func(int64)) error {
	remaining := total
	for remaining > 0 {
		n := chunkSize
		if remaining < n {
			n = remaining
		}
		written, err := io.CopyN(dst, src, n)
		if written > 0 && onChunk != nil {
			onChunk(written)
		}
		remaining -= written
		if err != nil {
			return err
		}
	}
	return nil
}

// handleCutoverImage streams the converted image (GET /cutover/image). Exactly
// st.bytes are sent — the shrunk filesystem, not the whole volume — with a
// Content-Length so the guest-side download can detect truncation.
//
// The byte count is reset at the START of every request (cutoverCopyBeginRequest)
// so a re-paste restarts the reported percentage instead of exceeding 100% by
// accumulating across attempts, and streamed in cutoverImageChunkBytes slices
// so the console can show real mid-transfer progress instead of only
// learning the transfer happened after the fact.
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
	// Drop the kernel's cached pages for this device before reading a byte.
	// machine-convert.sh mounted this same volume and rewrote it (chroot, GRUB,
	// fstab, agent removal); a page cached from before those writes would be
	// streamed to the destination in their place, producing a boot image that
	// silently lacks the conversion. Non-fatal by design — a stale read is a
	// risk, refusing to stream is a certain failure.
	if ierr := blockdiff.InvalidatePageCache(f); ierr != nil {
		log.Printf("appliance: cutover image stream for migration %d: could not drop cached pages for %s (%v) — the streamed image may be stale", st.migID, st.path, ierr)
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Length", fmt.Sprintf("%d", st.bytes))
	s.cutoverCopyBeginRequest(st.migID)
	err = streamImageChunked(w, f, st.bytes, cutoverImageChunkBytes, func(n int64) {
		s.cutoverCopyAddBytes(st.migID, n)
		if s.testAfterCutoverChunk != nil {
			s.testAfterCutoverChunk()
		}
	})
	if err != nil {
		// Mid-stream failure: the connection is already committed, so just log —
		// the guest-side dd fails short and the operator re-pastes the command.
		log.Printf("appliance: cutover image stream for migration %d aborted: %v", st.migID, err)
	}
}
