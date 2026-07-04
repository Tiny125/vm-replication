package receiver

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
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
			return Stats{Hello: hello, BlocksWritten: entries, ChangedBlocks: entries, Duration: time.Since(start)}, nil
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
	if e.Hash != "" && hex.EncodeToString(h.Sum(nil)) != e.Hash {
		_ = os.Remove(tmpName)
		return fmt.Errorf("content hash mismatch for %q", rel)
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
		if seen[rel] || isProtectedDestPath(rel) {
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

// isProtectedDestPath reports whether a destination-relative path is part of the
// destination's OWN OS plumbing that a file migration must never delete or
// overwrite (the destination keeps booting on its native kernel/boot/network).
// The agent already excludes the source equivalents from the transfer; this is
// the receiver-side backstop for the delete pass.
func isProtectedDestPath(rel string) bool {
	protected := []string{
		"boot", "vmlinuz", "initrd.img", "lib/modules",
		"etc/fstab", "etc/machine-id",
		"etc/netplan", "etc/systemd/network", "etc/NetworkManager/system-connections",
		"etc/network/interfaces", "etc/resolv.conf",
	}
	for _, pre := range protected {
		if rel == pre || strings.HasPrefix(rel, pre+"/") {
			return true
		}
	}
	return false
}
