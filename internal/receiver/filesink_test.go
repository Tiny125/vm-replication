package receiver

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"testing"

	"github.com/tiny125/vm-replication/internal/protocol"
)

// A file-transfer session must land the source's tree on the destination
// exactly: regular files with correct content (atomic), directories, symlinks,
// and — on a COMPLETE pass — removal of paths no longer present. Protected
// destination paths are never pruned. This drives real frames through Handle
// (over a pipe) into a temp output root and verifies the result.
func TestFileSessionRoundTrip(t *testing.T) {
	root := t.TempDir()
	// Pre-existing content: one file that will be deleted (not resent), and a
	// protected path that must survive the prune.
	must(t, os.WriteFile(filepath.Join(root, "stale.txt"), []byte("old"), 0o644))
	must(t, os.MkdirAll(filepath.Join(root, "boot"), 0o755))
	must(t, os.WriteFile(filepath.Join(root, "boot", "vmlinuz"), []byte("KERNEL"), 0o644))

	entries := []fileMsg{
		{entry: protocol.FileEntry{Path: "etc", Type: "dir", Mode: 0o755}},
		{entry: protocol.FileEntry{Path: "etc/app.conf", Type: "file", Mode: 0o644}, data: []byte("hello=world\n")},
		{entry: protocol.FileEntry{Path: "usr", Type: "dir", Mode: 0o755}},
		{entry: protocol.FileEntry{Path: "usr/big", Type: "file", Mode: 0o600}, data: bytesRepeat(0xAB, 3<<20)}, // multi-chunk
		{entry: protocol.FileEntry{Path: "link", Type: "symlink", Linkname: "etc/app.conf"}},
	}
	writeAndVerify(t, root, entries, true)

	// The complete pass must have pruned stale.txt but kept the protected boot file.
	if _, err := os.Stat(filepath.Join(root, "stale.txt")); !os.IsNotExist(err) {
		t.Error("a complete pass must delete files no longer sent")
	}
	if b, _ := os.ReadFile(filepath.Join(root, "boot", "vmlinuz")); string(b) != "KERNEL" {
		t.Error("protected destination path (boot/) must never be pruned")
	}
	// Content + symlink correctness.
	if b, _ := os.ReadFile(filepath.Join(root, "etc", "app.conf")); string(b) != "hello=world\n" {
		t.Errorf("etc/app.conf content wrong: %q", b)
	}
	if fi, _ := os.Lstat(filepath.Join(root, "usr", "big")); fi == nil || fi.Size() != 3<<20 {
		t.Error("multi-chunk file did not land at full size")
	}
	if tgt, _ := os.Readlink(filepath.Join(root, "link")); tgt != "etc/app.conf" {
		t.Errorf("symlink target wrong: %q", tgt)
	}
}

// With a FileTarget that returns a redirect, a file session must be answered
// with a HelloAck carrying DataTarget (the destination) and apply NOTHING
// locally. With hold=true it must return a Hold ack. This is the direct-to-
// destination path.
func TestFileSessionRedirect(t *testing.T) {
	root := t.TempDir()
	// Redirect case.
	ack := fileHelloAck(t, root, func(protocol.Hello) (string, string, bool) {
		return "10.0.0.5:5999", "appliance.example", false
	})
	if ack.Accepted || ack.DataTarget != "10.0.0.5:5999" || ack.DataServerName != "appliance.example" {
		t.Fatalf("redirect: got accepted=%v target=%q sni=%q", ack.Accepted, ack.DataTarget, ack.DataServerName)
	}
	if es, _ := os.ReadDir(root); len(es) != 0 {
		t.Error("a redirected session must not write anything locally")
	}
	// Hold case.
	ack = fileHelloAck(t, root, func(protocol.Hello) (string, string, bool) { return "", "", true })
	if ack.Accepted || !ack.Hold {
		t.Fatalf("hold: got accepted=%v hold=%v", ack.Accepted, ack.Hold)
	}
}

// fileHelloAck sends one file-mode Hello through Handle (with the given
// FileTarget) and returns the HelloAck.
func fileHelloAck(t *testing.T, root string, ft FileTarget) protocol.HelloAck {
	t.Helper()
	cli, srv := net.Pipe()
	go func() { _, _ = Handle(srv, root, "", nil, nil, nil, nil, ft) }()
	defer cli.Close()
	w := bufio.NewWriter(cli)
	r := bufio.NewReader(cli)
	_ = protocol.WriteJSON(w, protocol.MsgHello, protocol.Hello{ProtocolVersion: 1, Mode: protocol.ModeFile})
	_ = w.Flush()
	mt, payload, err := protocol.ReadFrame(r)
	if err != nil || mt != protocol.MsgHelloAck {
		t.Fatalf("expected hello-ack, got type %d err %v", mt, err)
	}
	var ack protocol.HelloAck
	_ = json.Unmarshal(payload, &ack)
	return ack
}

// A per-file content-hash mismatch must NOT fail the whole pass. Live files
// (logs, journals) can change between the agent's hash and its streaming read,
// so the streamed bytes won't match the entry's advertised hash. The receiver
// must keep the freshest streamed bytes (they are exactly Size, mTLS-intact),
// warn, and carry on — the pass still completes and the file lands. Before this
// fix, one racy log file aborted the entire migration ("content hash mismatch").
func TestFileSessionToleratesHashRace(t *testing.T) {
	root := t.TempDir()
	err := runFileSession(t, root, []fileMsg{
		{entry: protocol.FileEntry{Path: "var", Type: "dir", Mode: 0o755}},
		{entry: protocol.FileEntry{Path: "var/log", Type: "dir", Mode: 0o755}},
		// Wrong advertised hash, but a well-formed, correctly-sized data stream.
		{entry: protocol.FileEntry{Path: "var/log/access.log", Type: "file", Mode: 0o644, Hash: "0000deadbeef"}, data: []byte("racy log line\n")},
	}, true)
	if err != nil {
		t.Fatalf("a hash race must not fail the pass, got: %v", err)
	}
	if b, _ := os.ReadFile(filepath.Join(root, "var", "log", "access.log")); string(b) != "racy log line\n" {
		t.Errorf("streamed bytes must still land on a hash race, got %q", b)
	}
}

// A hostile agent must not escape the output root.
func TestFileSessionRejectsUnsafePaths(t *testing.T) {
	for _, bad := range []string{"/etc/passwd", "../escape", "a/../../b"} {
		root := t.TempDir()
		err := runFileSession(t, root, []fileMsg{{entry: protocol.FileEntry{Path: bad, Type: "file", Mode: 0o644}, data: []byte("x")}}, true)
		if err == nil {
			t.Errorf("path %q must be rejected", bad)
		}
	}
}

// An INCOMPLETE pass (Complete=false) must not prune anything.
func TestFileSessionIncompleteKeepsFiles(t *testing.T) {
	root := t.TempDir()
	must(t, os.WriteFile(filepath.Join(root, "keep.txt"), []byte("keep"), 0o644))
	writeAndVerify(t, root, []fileMsg{
		{entry: protocol.FileEntry{Path: "new.txt", Type: "file", Mode: 0o644}, data: []byte("new")},
	}, false)
	if _, err := os.Stat(filepath.Join(root, "keep.txt")); err != nil {
		t.Error("an incomplete pass must not delete existing files")
	}
}

type fileMsg struct {
	entry protocol.FileEntry
	data  []byte
}

// writeAndVerify runs a session and fails on any error.
func writeAndVerify(t *testing.T, root string, msgs []fileMsg, complete bool) {
	t.Helper()
	if err := runFileSession(t, root, msgs, complete); err != nil {
		t.Fatalf("file session: %v", err)
	}
}

// runFileSession drives a full file session into Handle over a pipe and returns
// the receiver's terminal error (nil on success).
func runFileSession(t *testing.T, root string, msgs []fileMsg, complete bool) error {
	t.Helper()
	cli, srv := net.Pipe()
	errc := make(chan error, 1)
	go func() {
		_, e := Handle(srv, root, filepath.Join(t.TempDir(), "f.manifest"), nil, nil, nil, nil, nil)
		errc <- e
	}()
	w := bufio.NewWriter(cli)
	r := bufio.NewReader(cli)

	if err := protocol.WriteJSON(w, protocol.MsgHello, protocol.Hello{ProtocolVersion: 1, Mode: protocol.ModeFile}); err != nil {
		return err
	}
	_ = w.Flush()
	mt, payload, err := protocol.ReadFrame(r)
	if err != nil || mt != protocol.MsgHelloAck {
		return errOr(err, "no hello-ack")
	}
	var ack protocol.HelloAck
	_ = json.Unmarshal(payload, &ack)
	if !ack.Accepted {
		cli.Close()
		return errString("not accepted: " + ack.Message)
	}
	for _, m := range msgs {
		e := m.entry
		if e.Type == "file" && !e.Unchanged {
			e.Size = int64(len(m.data))
			// Respect an explicitly-set (possibly wrong) hash so tests can
			// exercise the mismatch path; otherwise compute the correct one.
			if e.Hash == "" {
				sum := sha256.Sum256(m.data)
				e.Hash = hex.EncodeToString(sum[:])
			}
		}
		if err := protocol.WriteJSON(w, protocol.MsgFileEntry, e); err != nil {
			return err
		}
		if e.Type == "file" && !e.Unchanged {
			if err := protocol.WriteFrame(w, protocol.MsgFileData, m.data); err != nil {
				return err
			}
		}
		if err := w.Flush(); err != nil {
			return err
		}
	}
	if err := protocol.WriteJSON(w, protocol.MsgFileDone, protocol.FileDone{Complete: complete, Entries: int64(len(msgs))}); err != nil {
		return err
	}
	_ = w.Flush()
	// The receiver either DoneAcks (success) or the goroutine returns an error.
	mt, _, rerr := protocol.ReadFrame(r)
	cli.Close()
	handleErr := <-errc
	if handleErr != nil {
		return handleErr
	}
	if rerr == nil && mt != protocol.MsgDoneAck {
		return errString("expected done-ack")
	}
	return nil
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
func bytesRepeat(b byte, n int) []byte {
	out := make([]byte, n)
	for i := range out {
		out[i] = b
	}
	return out
}
func errOr(err error, msg string) error {
	if err != nil {
		return err
	}
	return errString(msg)
}

type errString string

func (e errString) Error() string { return string(e) }
