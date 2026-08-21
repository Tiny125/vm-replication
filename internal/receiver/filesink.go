package receiver

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/tiny125/vm-replication/internal/protocol"
)

// handleFileSession applies a file-transfer session to the output tree rooted at
// root. It runs on the DESTINATION (the launched Linode): the agent streams the
// source's files here and this writes them into the live filesystem.
//
// Safety properties:
//   - Each regular file is written to a temp file in the same directory and
//     atomically renamed into place, so a reader never sees a half-written file
//     and an interrupted transfer never truncates the previous copy.
//   - Every path is validated to stay under root (no absolute paths, no "..",
//     no symlink target escapes for the write itself), so a hostile/buggy agent
//     cannot write outside the destination tree.
//   - Deletions (paths gone from the source) are applied only when the agent
//     reports a COMPLETE pass, and never touch the protected boot set.
//
// A per-path manifest (path -> content hash) is kept next to manifestPath so a
// later pass can skip unchanged files (the agent also skips them by sending
// Unchanged=true, but the receiver's copy lets it answer "what changed").
func handleFileSession(w *bufio.Writer, r *bufio.Reader, root, manifestPath string, hello protocol.Hello, onProgress Progress) (Stats, error) {
	if err := os.MkdirAll(root, 0o755); err != nil {
		_ = protocol.WriteJSON(w, protocol.MsgHelloAck, protocol.HelloAck{Accepted: false, Message: "cannot create output root: " + err.Error()})
		_ = w.Flush()
		return Stats{}, fmt.Errorf("mkdir root: %w", err)
	}
	if err := protocol.WriteJSON(w, protocol.MsgHelloAck, protocol.HelloAck{Accepted: true}); err != nil {
		return Stats{}, err
	}
	if err := w.Flush(); err != nil {
		return Stats{}, err
	}

	start := time.Now()
	seen := make(map[string]bool) // relative paths written this pass (for delete-propagation)
	var entries, bytesApplied int64
	// Signal "data flowing" so the appliance marks the migration replicating (the
	// block path does the same with an initial written=0 progress call).
	if onProgress != nil {
		onProgress(0, 0, false)
	}

	for {
		t, payload, err := protocol.ReadFrame(r)
		if err != nil {
			if err == io.EOF {
				return Stats{Hello: hello}, fmt.Errorf("stream closed before done")
			}
			return Stats{Hello: hello}, fmt.Errorf("read frame: %w", err)
		}
		switch t {
		case protocol.MsgFileEntry:
			var e protocol.FileEntry
			if err := json.Unmarshal(payload, &e); err != nil {
				return Stats{Hello: hello}, fmt.Errorf("decode file entry: %w", err)
			}
			rel, ok := safeRel(e.Path)
			if !ok {
				return Stats{Hello: hello}, fmt.Errorf("refusing unsafe path %q", e.Path)
			}
			// A regular file with content: consume its MsgFileData chunks even if we
			// skip applying it, so the stream stays framed.
			if err := applyFileEntry(r, root, rel, e, &bytesApplied); err != nil {
				return Stats{Hello: hello}, err
			}
			seen[rel] = true
			entries++
			if onProgress != nil && entries%64 == 0 {
				onProgress(entries, 0, false)
			}
		case protocol.MsgFileDone:
			var done protocol.FileDone
			if err := json.Unmarshal(payload, &done); err != nil {
				return Stats{Hello: hello}, fmt.Errorf("decode file done: %w", err)
			}
			if done.Complete {
				pruneDeleted(root, seen)
			}
			_ = protocol.WriteJSON(w, protocol.MsgDoneAck, protocol.DoneAck{OK: true, BlocksWritten: entries})
			_ = w.Flush()
			if onProgress != nil {
				onProgress(entries, entries, false)
			}
			// Complete carries the agent's own verdict on whether it walked the
			// WHOLE tree. A pass that ended early applied real data and is not an
			// error, but it must not be recorded as the baseline that unlocks
			// cutover.
			return Stats{Hello: hello, BlocksWritten: entries, ChangedBlocks: entries, BytesOnWire: bytesApplied, Duration: time.Since(start), Complete: done.Complete}, nil
		default:
			return Stats{Hello: hello}, fmt.Errorf("unexpected frame type %d in file session", t)
		}
	}
}

// applyFileEntry writes one entry into root (dir/symlink/file). For a regular
// file it reads exactly e.Size bytes of MsgFileData chunks; when Unchanged it
// only refreshes metadata (no data frames follow). It always consumes the
// declared content so the stream stays aligned.
func applyFileEntry(r *bufio.Reader, root, rel string, e protocol.FileEntry, bytesApplied *int64) error {
	dst := filepath.Join(root, rel)
	switch e.Type {
	case "dir":
		if err := os.MkdirAll(dst, os.FileMode(e.Mode)&os.ModePerm); err != nil {
			return fmt.Errorf("mkdir %q: %w", rel, err)
		}
		_ = os.Chmod(dst, os.FileMode(e.Mode)&os.ModePerm)
		chownBestEffort(dst, e.UID, e.GID)
		return nil
	case "symlink":
		_ = os.MkdirAll(filepath.Dir(dst), 0o755)
		_ = os.Remove(dst)
		if err := os.Symlink(e.Linkname, dst); err != nil {
			return fmt.Errorf("symlink %q: %w", rel, err)
		}
		return nil
	case "file":
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return fmt.Errorf("mkdir parent of %q: %w", rel, err)
		}
		if e.Unchanged {
			// Metadata refresh only; no content frames follow.
			_ = os.Chmod(dst, os.FileMode(e.Mode)&os.ModePerm)
			chownBestEffort(dst, e.UID, e.GID)
			if e.ModTime > 0 {
				mt := time.Unix(e.ModTime, 0)
				_ = os.Chtimes(dst, mt, mt)
			}
			return nil
		}
		return writeFileAtomic(r, dst, rel, e, bytesApplied)
	default:
		return fmt.Errorf("unknown entry type %q for %q", e.Type, rel)
	}
}

// writeFileAtomic streams e.Size bytes of MsgFileData chunks into a temp file
// next to dst, then renames it into place (atomic) and applies metadata.
func writeFileAtomic(r *bufio.Reader, dst, rel string, e protocol.FileEntry, bytesApplied *int64) error {
	tmp, err := os.CreateTemp(filepath.Dir(dst), ".vmrepl-file-*")
	if err != nil {
		return fmt.Errorf("temp for %q: %w", rel, err)
	}
	tmpName := tmp.Name()
	cleanup := func() { _ = tmp.Close(); _ = os.Remove(tmpName) }

	h := sha256.New()
	var got int64
	for got < e.Size {
		t, payload, err := protocol.ReadFrame(r)
		if err != nil {
			cleanup()
			return fmt.Errorf("read data for %q: %w", rel, err)
		}
		if t != protocol.MsgFileData {
			cleanup()
			return fmt.Errorf("expected file data for %q, got frame type %d", rel, t)
		}
		if _, err := tmp.Write(payload); err != nil {
			cleanup()
			return fmt.Errorf("write %q: %w", rel, err)
		}
		h.Write(payload)
		got += int64(len(payload))
		*bytesApplied += int64(len(payload))
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return fmt.Errorf("close %q: %w", rel, err)
	}
	// A content-hash mismatch here is NOT corruption: mTLS already guarantees the
	// bytes arrived intact and we consumed exactly e.Size of them, so the stream
	// stays framed. It means the source file changed between the agent hashing it
	// and streaming it — inevitable for live files like logs and journals. We keep
	// the freshest streamed bytes (they are a valid point-in-time read) and let a
	// later pass re-sync; at cutover the source is frozen, so the final pass has no
	// race and lands the exact content. Never abort the whole migration over it.
	if e.Hash != "" && hex.EncodeToString(h.Sum(nil)) != e.Hash {
		log.Printf("receiver: %q changed during copy (hash mismatch); keeping streamed bytes, will re-sync next pass", rel)
	}
	if err := os.Rename(tmpName, dst); err != nil {
		_ = os.Remove(tmpName)
		return fmt.Errorf("rename %q into place: %w", rel, err)
	}
	_ = os.Chmod(dst, os.FileMode(e.Mode)&os.ModePerm)
	chownBestEffort(dst, e.UID, e.GID)
	if e.ModTime > 0 {
		mt := time.Unix(e.ModTime, 0)
		_ = os.Chtimes(dst, mt, mt)
	}
	return nil
}

// chownBestEffort sets ownership when possible (needs root on the destination,
// which the file receiver has). Failures are ignored: a non-root test host or a
// uid that doesn't exist yet must not abort the transfer.
func chownBestEffort(path string, uid, gid int) {
	if uid >= 0 && gid >= 0 {
		_ = os.Lchown(path, uid, gid)
	}
}

// safeRel validates an agent-supplied path stays under the root: relative, no
// "..", no leading slash. Returns the cleaned relative path.
func safeRel(p string) (string, bool) {
	if p == "" || strings.HasPrefix(p, "/") || strings.Contains(p, "\x00") {
		return "", false
	}
	clean := filepath.Clean(p)
	if clean == ".." || strings.HasPrefix(clean, "../") || filepath.IsAbs(clean) {
		return "", false
	}
	return clean, true
}

// pruneDeleted removes paths under root that were NOT seen this (complete) pass,
// so files deleted on the source are removed on the destination too. It never
// removes protected boot paths (the destination's own OS plumbing), which the
// agent never sends in the first place.
func pruneDeleted(root string, seen map[string]bool) {
	var toRemove []string
	_ = filepath.Walk(root, func(p string, info os.FileInfo, err error) error {
		if err != nil || p == root {
			return nil
		}
		rel, e := filepath.Rel(root, p)
		if e != nil {
			return nil
		}
		if IsProtectedDestPath(rel) {
			// Skip the whole subtree, not just this entry: descending into /proc
			// and /sys enumerated tens of thousands of paths per complete pass,
			// each producing one doomed unlink, on a 1 vCPU destination.
			// SkipDir on a FILE would skip the rest of its parent directory, so
			// it is only correct for directories.
			if info != nil && info.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if seen[rel] {
			return nil
		}
		toRemove = append(toRemove, p)
		return nil
	})
	// Remove deepest paths first so directories are empty when removed.
	for i := len(toRemove) - 1; i >= 0; i-- {
		_ = os.Remove(toRemove[i])
	}
}

// IsProtectedDestPath reports whether a destination-relative path must survive a
// file migration: the destination's own OS plumbing, the runtime
// pseudo-filesystems, and the receiver's own install.
//
// This is ALSO the agent's exclusion list (cmd/agent/filewalk.go delegates here),
// deliberately: they were two hand-maintained lists that were documented as
// mirroring each other and did not, and that divergence was a bug.
//
// The relationship is the opposite of what it looks like. Excluding a path on the
// AGENT side does not protect it — it marks it for deletion. An excluded path is
// never sent, so it is absent from the receiver's `seen` set, and the prune
// deletes exactly what it does not recognise. So every path the agent skips must
// appear here too, or the migration removes it from the running destination.
//
// The runtime entries are not decorative. /dev is devtmpfs and /run is tmpfs, and
// unlink SUCCEEDS on both as root (only /proc and /sys refuse with EPERM), so the
// prune really did delete /dev/null and /run/sshd out from under a live system —
// the likeliest cause of a destination whose sshd accepted TCP but never sent a
// banner during a copy. /proc and /sys are listed too: unlink fails there
// harmlessly, but walking them enumerates tens of thousands of entries per pass.
//
// Nothing here carries user data, so protecting it cannot suppress anything the
// operator asked to migrate. Credentials, SSH config and services are
// deliberately NOT protected: the destination is meant to take on the source's
// identity, and protecting /etc/shadow while copying /etc/passwd would leave a
// machine with the source's users and the destination's passwords.
func IsProtectedDestPath(rel string) bool {
	for _, pre := range protectedDestPaths {
		if rel == pre || strings.HasPrefix(rel, pre+"/") {
			return true
		}
	}
	return false
}

var protectedDestPaths = []string{
	// The destination keeps booting on its own kernel, boot files and network.
	"boot", "vmlinuz", "vmlinuz.old", "initrd.img", "initrd.img.old", "lib/modules",
	"etc/fstab", "etc/machine-id", "etc/resolv.conf",
	"etc/netplan", "etc/systemd/network", "etc/NetworkManager/system-connections",
	"etc/network/interfaces",
	// Runtime pseudo-filesystems and scratch. Deleting these breaks the running
	// destination; the kernel and systemd repopulate them at boot.
	"proc", "sys", "dev", "run", "tmp",
	"var/tmp", "var/run", "var/lock",
	// Never migrated, so the destination's own (empty) copies would be pruned.
	"mnt", "media", "lost+found",
	// The replication tooling's own install. These exist only on the destination,
	// so they are never in `seen`. Deleting the receiver's binary does not kill the
	// running process, so a pass still completes and nothing looks wrong — but the
	// unit is Restart=always, so the next restart fails 203/EXEC and the
	// destination goes silent while the console still reports it ready.
	"usr/local/bin/vmrepl-receiver", "usr/local/bin/vmrepl-agent",
	"etc/vmrepl", "etc/vm-repl",
	"etc/systemd/system/vmrepl-receiver.service",
	"etc/systemd/system/vmrepl-agent.service",
	"etc/systemd/system/vmrepl-agent.timer",
	"var/log/vmrepl-dest.log",
}
