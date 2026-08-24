package main

import (
	"bufio"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/tiny125/vm-replication/internal/api"
	"github.com/tiny125/vm-replication/internal/protocol"
	"github.com/tiny125/vm-replication/internal/receiver"
	"github.com/tiny125/vm-replication/internal/transport"
)

// fileChunk is the content chunk size for MsgFileData frames.
const fileChunk = 1 << 20 // 1 MiB

// fileManifest is the agent's per-path content checkpoint: path -> content
// SHA-256. It lets a later pass skip files whose content is unchanged (only
// metadata is refreshed) — the file-transfer analog of the block CBT manifest.
type fileManifest map[string]string

func loadFileManifest(path string) fileManifest {
	m := fileManifest{}
	b, err := os.ReadFile(path)
	if err != nil {
		return m
	}
	_ = json.Unmarshal(b, &m)
	return m
}

func (m fileManifest) save(path string) error {
	b, err := json.Marshal(m)
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// filePassState is the agent's record of its LAST COMPLETED file pass. Each
// agent run is a ONE-SHOT process (systemd timer, every 60s), so this has to
// live on disk to survive to the next run. In direct mode the appliance never
// sees the file data, so this record — replayed in the next Hello — is the only
// way the console can honestly report that the copy finished. It sits next to
// the file manifest under /var/lib/vmrepl-source-*, which excludedFromFileCopy
// already skips, so it is never copied to the destination.
type filePassState struct {
	Seq      int64  `json:"seq"`      // monotonic; the appliance de-duplicates on it
	Complete bool   `json:"complete"` // the whole tree was walked
	Entries  int64  `json:"entries"`
	Bytes    int64  `json:"bytes"`
	At       int64  `json:"at"`     // unix seconds the pass finished
	Target   string `json:"target"` // host:port this pass was streamed to
}

// filePassPath is the sidecar path for the pass record, derived from the
// manifest path so both share the migration's token-scoped name.
func filePassPath(manifest string) string { return manifest + ".pass" }

// loadFilePass returns the last recorded pass, or the zero value when there is
// none or it is unreadable — a missing or corrupt record must never fail a pass,
// it just means the appliance has nothing to credit yet.
func loadFilePass(manifest string) filePassState {
	var p filePassState
	b, err := os.ReadFile(filePassPath(manifest))
	if err != nil {
		return filePassState{}
	}
	if err := json.Unmarshal(b, &p); err != nil {
		return filePassState{}
	}
	return p
}

// save writes the pass record atomically (tmp + rename), the same idiom the
// manifest uses, so a crash mid-write cannot leave a torn record.
func (p filePassState) save(manifest string) error {
	b, err := json.Marshal(p)
	if err != nil {
		return err
	}
	path := filePassPath(manifest)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// fileHello builds the file-mode Hello, carrying the last completed pass so the
// appliance can record an honest baseline in direct mode (where it never sees
// the data itself). Pass the zero filePassState for the destination hop — the
// destination has no use for the report.
func fileHello(c cfg, last filePassState) protocol.Hello {
	host, _ := os.Hostname()
	return protocol.Hello{
		ProtocolVersion: 1, JobID: c.jobID, SourceHostname: host,
		DevicePath: c.root, Mode: protocol.ModeFile,
		ReportsPasses:    true,
		LastPassSeq:      last.Seq,
		LastPassComplete: last.Complete,
		LastPassEntries:  last.Entries,
		LastPassBytes:    last.Bytes,
		LastPassAt:       last.At,
		LastPassTarget:   last.Target,
	}
}

// excludedFromFileCopy reports whether a source-relative path must NOT be
// copied. It delegates to receiver.IsProtectedDestPath so the agent's exclusions
// and the receiver's delete-pass protections are ONE list.
//
// They must be the same list, because excluding a path here does not merely fail
// to copy it — it marks it for DELETION on the destination. An excluded path is
// never sent, so it is absent from the receiver's `seen` set, and the prune
// removes everything it does not recognise. Two hand-maintained lists documented
// as mirroring each other had already drifted apart, which is how the prune came
// to delete /dev/null, /run/sshd and the receiver's own binary on a live
// destination.
func excludedFromFileCopy(rel string) bool {
	if receiver.IsProtectedDestPath(rel) {
		return true
	}
	// The agent's own checkpoints, which are specific to the source side.
	return strings.HasPrefix(rel, "var/lib/vmrepl-source-")
}

// dialFileSession opens an mTLS connection to target (verifying it against
// serverName), sends the given file-mode Hello, and returns the connection,
// buffered reader/writer, and the receiver's HelloAck. The Hello is a parameter
// because the control hop carries the last-pass report while the destination
// hop does not.
func dialFileSession(c cfg, target, serverName string, hello protocol.Hello) (*tls.Conn, *bufio.Writer, *bufio.Reader, protocol.HelloAck, error) {
	tlsCfg, err := transport.ClientConfig(c.tls, serverName)
	if err != nil {
		return nil, nil, nil, protocol.HelloAck{}, err
	}
	conn, err := tls.Dial("tcp", target, tlsCfg)
	if err != nil {
		return nil, nil, nil, protocol.HelloAck{}, fmt.Errorf("dial receiver: %w", err)
	}
	w := bufio.NewWriterSize(conn, 1<<20)
	r := bufio.NewReaderSize(conn, 1<<16)
	if err := protocol.WriteJSON(w, protocol.MsgHello, hello); err != nil {
		conn.Close()
		return nil, nil, nil, protocol.HelloAck{}, fmt.Errorf("send hello: %w", err)
	}
	if err := w.Flush(); err != nil {
		conn.Close()
		return nil, nil, nil, protocol.HelloAck{}, err
	}
	ack, err := expectAck(r)
	if err != nil {
		conn.Close()
		return nil, nil, nil, protocol.HelloAck{}, err
	}
	return conn, w, r, ack, nil
}

// replicateFiles performs one file-transfer pass: connect, send a file-mode
// Hello, walk the root (staying on the root filesystem, skipping excluded
// paths), stream each file/dir/symlink, then commit the manifest. It reuses the
// same mTLS, JobID identity, and Hold gating as the block path.
func replicateFiles(c cfg) (syncResult, error) {
	res := syncResult{mode: api.SyncFull, startedAt: time.Now()}
	prev := loadFileManifest(c.manifest)
	next := fileManifest{}
	// The pass we last completed. It rides the CONTROL Hello so the appliance can
	// baseline a direct migration, where it never sees the file data itself.
	last := loadFilePass(c.manifest)
	// Where this pass's data actually goes; overwritten if we get redirected.
	dataTarget := c.target

	// Connect to the CONTROL receiver (the appliance). It either holds (not
	// started), accepts directly (appliance-staging fallback), or REDIRECTS us to
	// the launched destination Linode to stream straight into it.
	conn, w, r, ack, err := dialFileSession(c, c.target, c.serverName, fileHello(c, last))
	if err != nil {
		return res, err
	}
	if ack.Hold {
		conn.Close()
		log.Printf("agent: connection validated — replication not started yet (or destination still launching); will retry")
		return res, nil
	}
	if ack.DataTarget != "" {
		// Direct-to-destination: re-dial the destination and stream there. The
		// server name stays the appliance's cert SAN — the destination presents
		// the appliance's receiver cert, so verification passes without a
		// per-destination certificate.
		conn.Close()
		sni := ack.DataServerName
		if sni == "" {
			sni = c.serverName
		}
		log.Printf("agent: streaming files directly to the destination %s", ack.DataTarget)
		dataTarget = ack.DataTarget
		// The destination has no use for the pass report — only the appliance
		// records baselines — so the destination hop carries a bare Hello.
		conn, w, r, ack, err = dialFileSession(c, ack.DataTarget, sni, fileHello(c, filePassState{}))
		if err != nil {
			return res, fmt.Errorf("dial destination %s: %w", ack.DataTarget, err)
		}
		if ack.Hold {
			conn.Close()
			log.Printf("agent: destination not ready to receive yet; will retry")
			return res, nil
		}
	}
	defer conn.Close()
	if !ack.Accepted {
		return res, fmt.Errorf("receiver rejected file session: %s", ack.Message)
	}

	// The root's device id, so we stay on the root filesystem (don't descend into
	// other mounted filesystems — those are separate migrations).
	var rootDev uint64
	if fi, err := os.Lstat(c.root); err == nil {
		if st, ok := fi.Sys().(*syscall.Stat_t); ok {
			rootDev = uint64(st.Dev)
		}
	}

	var entries, bytesWire int64
	var skipped skippedMounts
	walkErr := filepath.WalkDir(c.root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			log.Printf("agent: skip %q: %v", p, err)
			if d != nil && d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		rel, rerr := filepath.Rel(c.root, p)
		if rerr != nil || rel == "." {
			return nil
		}
		if excludedFromFileCopy(rel) {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		info, ierr := d.Info()
		if ierr != nil {
			return nil
		}
		// Stay on the root filesystem: skip anything on a different device — but
		// RECORD it. Skipping silently is how a source's mounted data volume
		// vanished from a migration that reported success (F-09).
		if st, ok := info.Sys().(*syscall.Stat_t); ok && rootDev != 0 && uint64(st.Dev) != rootDev {
			skipped.add(rel)
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		e, sendData, herr := entryFor(p, rel, info, prev, next)
		if herr != nil {
			log.Printf("agent: skip %q: %v", rel, herr)
			return nil
		}
		if err := protocol.WriteJSON(w, protocol.MsgFileEntry, e); err != nil {
			return err
		}
		entries++
		if sendData {
			n, streamedHash, derr := streamFileData(w, p, e.Size)
			if derr != nil {
				return derr
			}
			// Record what the receiver actually stored (the hash of the exact bytes
			// we streamed), not the pre-read hash — a live file may have changed
			// between the two, and the manifest must reflect the delivered content
			// so the next pass's unchanged-skip is correct.
			next[rel] = streamedHash
			bytesWire += n
		}
		return nil
	})
	complete := walkErr == nil
	if walkErr != nil {
		log.Printf("agent: file walk ended early: %v", walkErr)
	}

	// Say what was left behind. The operator is about to be told the copy is
	// complete and then invited to power the source off; if data sat on another
	// filesystem, this is their only chance to notice.
	if warn := skipped.warning(); warn != "" {
		log.Printf("agent: WARNING %s", warn)
	}
	if err := protocol.WriteJSON(w, protocol.MsgFileDone, protocol.FileDone{
		Complete: complete, Entries: entries, BytesOnWire: bytesWire,
		SkippedMounts: skipped.list(),
	}); err != nil {
		return res, err
	}
	if err := w.Flush(); err != nil {
		return res, err
	}
	dack, err := expectDoneAck(r)
	if err != nil {
		return res, err
	}
	if !dack.OK {
		return res, fmt.Errorf("receiver rejected file pass: %s", dack.Error)
	}

	// Commit the checkpoint only after the receiver confirmed the pass.
	if complete {
		if err := next.save(c.manifest); err != nil {
			log.Printf("agent: warning: could not save file manifest: %v", err)
		}
	}
	// Record what this pass delivered so the NEXT Hello can tell the appliance.
	// In direct mode nothing else can: the file data never touches it. Written
	// only after the receiver confirmed the pass (DoneAck OK), and it records the
	// TARGET so the appliance can refuse to credit a pass that landed on a
	// destination it no longer streams to. An INCOMPLETE pass is recorded too —
	// honestly, with Complete=false — so the console can say the walk ended early
	// instead of silently waiting.
	if err := (filePassState{
		Seq: last.Seq + 1, Complete: complete, Entries: entries,
		Bytes: bytesWire, At: time.Now().Unix(), Target: dataTarget,
	}).save(c.manifest); err != nil {
		log.Printf("agent: warning: could not save the file pass record: %v", err)
	}
	res.finishedAt = time.Now()
	res.total, res.changed, res.bytes = entries, entries, bytesWire
	log.Printf("agent: file pass complete: %d entries, %s on wire in %s", entries, humanBytes(bytesWire), res.finishedAt.Sub(res.startedAt).Round(time.Millisecond))
	return res, nil
}

// entryFor builds the FileEntry for one path and records its hash in next. It
// returns sendData=true when the receiver needs the file's content (a changed
// or new regular file). Symlinks and dirs never send data; a regular file whose
// content hash matches the previous manifest is marked Unchanged.
func entryFor(abs, rel string, info os.FileInfo, prev, next fileManifest) (protocol.FileEntry, bool, error) {
	st, _ := info.Sys().(*syscall.Stat_t)
	uid, gid := -1, -1
	if st != nil {
		uid, gid = int(st.Uid), int(st.Gid)
	}
	e := protocol.FileEntry{
		Path: rel, Mode: uint32(info.Mode().Perm()), UID: uid, GID: gid,
		ModTime: info.ModTime().Unix(),
	}
	switch {
	case info.IsDir():
		e.Type = "dir"
		return e, false, nil
	case info.Mode()&os.ModeSymlink != 0:
		target, err := os.Readlink(abs)
		if err != nil {
			return e, false, err
		}
		e.Type, e.Linkname = "symlink", target
		return e, false, nil
	case info.Mode().IsRegular():
		e.Type, e.Size = "file", info.Size()
		sum, err := hashFilePrefix(abs, e.Size)
		if err != nil {
			return e, false, err
		}
		e.Hash = sum
		next[rel] = sum // provisional; overwritten with the streamed hash if we send data
		if prev[rel] == sum {
			e.Unchanged = true
			return e, false, nil // content unchanged; metadata only
		}
		return e, true, nil
	default:
		// Sockets, device nodes, fifos: skip (recreated at runtime by the OS).
		return e, false, fmt.Errorf("unsupported file type")
	}
}

// hashFilePrefix hashes exactly the first size bytes of a file (the same window
// streamFileData sends). Hashing to EOF instead would read a different amount
// than we stream for a file being appended to, guaranteeing a spurious mismatch;
// bounding it to the stat'd size keeps the delta decision consistent with the
// wire. A file that shrank below size yields EOF early — we hash what's there.
func hashFilePrefix(path string, size int64) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.CopyN(h, f, size); err != nil && err != io.EOF {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// streamFileData sends a regular file's content as MsgFileData chunks totalling
// EXACTLY size bytes — the length the receiver was told to expect in the
// FileEntry — and returns the SHA-256 of exactly those streamed bytes.
//
// Sending exactly size bytes is what keeps a live file from derailing the pass:
// the receiver frames the file purely by byte count, so if a log grew after we
// stat'd it we send only the first size bytes, and if it was truncated we
// zero-pad up to size. Either way the stream stays aligned. The returned hash
// lets the caller record in its manifest what the receiver actually stored,
// rather than a stale pre-read hash of a since-changed file.
func streamFileData(w *bufio.Writer, path string, size int64) (int64, string, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, "", err
	}
	defer f.Close()
	h := sha256.New()
	buf := make([]byte, fileChunk)
	var sent int64
	for sent < size {
		want := size - sent
		if want > int64(len(buf)) {
			want = int64(len(buf))
		}
		n, rerr := f.Read(buf[:want])
		if n > 0 {
			chunk := buf[:n]
			if err := protocol.WriteFrame(w, protocol.MsgFileData, chunk); err != nil {
				return sent, "", err
			}
			h.Write(chunk)
			sent += int64(n)
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			return sent, "", rerr
		}
	}
	// The file shrank below its stat'd size mid-copy: pad with zeros so the
	// receiver still gets exactly size bytes and the stream stays framed.
	if sent < size {
		zeros := make([]byte, fileChunk)
		for sent < size {
			want := size - sent
			if want > int64(len(zeros)) {
				want = int64(len(zeros))
			}
			if err := protocol.WriteFrame(w, protocol.MsgFileData, zeros[:want]); err != nil {
				return sent, "", err
			}
			h.Write(zeros[:want])
			sent += want
		}
	}
	return sent, hex.EncodeToString(h.Sum(nil)), nil
}
