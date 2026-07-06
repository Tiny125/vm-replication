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

// excludedFromFileCopy reports whether a source-relative path must NOT be
// copied: virtual/pseudo filesystems, transient dirs, the destination's own
// boot/kernel/network plumbing (it keeps booting on its native OS), and the
// agent's own install. These mirror the receiver's isProtectedDestPath backstop.
func excludedFromFileCopy(rel string) bool {
	excluded := []string{
		// Virtual / transient.
		"proc", "sys", "dev", "run", "tmp", "mnt", "media", "lost+found",
		"var/tmp", "var/run", "var/lock",
		// Destination keeps its own kernel + boot + modules.
		"boot", "vmlinuz", "vmlinuz.old", "initrd.img", "initrd.img.old", "lib/modules",
		// Destination keeps its own identity + network config.
		"etc/fstab", "etc/machine-id", "etc/resolv.conf",
		"etc/netplan", "etc/systemd/network",
		"etc/NetworkManager/system-connections", "etc/network/interfaces",
		// The agent's own install (never copy it to the destination).
		"usr/local/bin/vmrepl-agent", "etc/vm-repl",
	}
	for _, e := range excluded {
		if rel == e || strings.HasPrefix(rel, e+"/") {
			return true
		}
	}
	// The agent's checkpoints live in /var/lib.
	if strings.HasPrefix(rel, "var/lib/vmrepl-source-") {
		return true
	}
	return false
}

// dialFileSession opens an mTLS connection to target (verifying it against
// serverName), sends a file-mode Hello, and returns the connection, buffered
// reader/writer, and the receiver's HelloAck.
func dialFileSession(c cfg, target, serverName string) (*tls.Conn, *bufio.Writer, *bufio.Reader, protocol.HelloAck, error) {
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
	host, _ := os.Hostname()
	if err := protocol.WriteJSON(w, protocol.MsgHello, protocol.Hello{
		ProtocolVersion: 1, JobID: c.jobID, SourceHostname: host,
		DevicePath: c.root, Mode: protocol.ModeFile,
	}); err != nil {
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

	// Connect to the CONTROL receiver (the appliance). It either holds (not
	// started), accepts directly (appliance-staging fallback), or REDIRECTS us to
	// the launched destination Linode to stream straight into it.
	conn, w, r, ack, err := dialFileSession(c, c.target, c.serverName)
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
		conn, w, r, ack, err = dialFileSession(c, ack.DataTarget, sni)
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
		// Stay on the root filesystem: skip anything on a different device.
		if st, ok := info.Sys().(*syscall.Stat_t); ok && rootDev != 0 && uint64(st.Dev) != rootDev {
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
			n, derr := streamFileData(w, p, e.Size)
			if derr != nil {
				return derr
			}
			bytesWire += n
		}
		return nil
	})
	complete := walkErr == nil
	if walkErr != nil {
		log.Printf("agent: file walk ended early: %v", walkErr)
	}

	if err := protocol.WriteJSON(w, protocol.MsgFileDone, protocol.FileDone{
		Complete: complete, Entries: entries, BytesOnWire: bytesWire,
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
		sum, err := hashFile(abs)
		if err != nil {
			return e, false, err
		}
		e.Hash = sum
		next[rel] = sum
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

func hashFile(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// streamFileData sends a regular file's content as MsgFileData chunks totalling
// exactly size bytes.
func streamFileData(w *bufio.Writer, path string, size int64) (int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	buf := make([]byte, fileChunk)
	var sent int64
	for sent < size {
		n, rerr := f.Read(buf)
		if n > 0 {
			if err := protocol.WriteFrame(w, protocol.MsgFileData, buf[:n]); err != nil {
				return sent, err
			}
			sent += int64(n)
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			return sent, rerr
		}
	}
	return sent, nil
}
