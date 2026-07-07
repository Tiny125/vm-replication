package main

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"

	"github.com/tiny125/vm-replication/internal/protocol"
)

// The file walk must never copy the destination's own boot/kernel/network
// plumbing, virtual filesystems, or the agent's own install — these are
// excluded at the source so the destination keeps booting on its native OS.
// (The receiver has an isProtectedDestPath backstop for the same set.)
func TestExcludedFromFileCopy(t *testing.T) {
	excluded := []string{
		"proc", "proc/1/stat", "sys/kernel", "dev/null", "run/lock", "tmp/x",
		"boot", "boot/grub/grub.cfg", "vmlinuz", "lib/modules/6.1.0/x.ko",
		"etc/fstab", "etc/machine-id", "etc/resolv.conf",
		"etc/netplan/01.yaml", "etc/systemd/network/10.network",
		"etc/NetworkManager/system-connections/x", "etc/network/interfaces",
		"usr/local/bin/vmrepl-agent", "etc/vm-repl/agent.crt",
		"var/lib/vmrepl-source-abc-disk0.cbt", "var/tmp/junk", "mnt/data", "lost+found",
	}
	for _, p := range excluded {
		if !excludedFromFileCopy(p) {
			t.Errorf("%q must be excluded from the file copy", p)
		}
	}
	kept := []string{
		"etc/hostname", "etc/ssh/sshd_config", "home/user/.bashrc",
		"var/www/index.html", "opt/app/bin/server", "usr/bin/python3",
		"etc/systemd/system/app.service", // unit files ARE copied (not etc/systemd/network)
		"srv/data.db", "root/.ssh/authorized_keys",
	}
	for _, p := range kept {
		if excludedFromFileCopy(p) {
			t.Errorf("%q must be copied (not excluded)", p)
		}
	}
}

// streamFileData must always put EXACTLY `size` bytes on the wire — the count
// the receiver was told to expect in the FileEntry — no matter how the file
// changes between the stat and the read. If it sent fewer/more, the receiver's
// per-file byte counter would desync and mis-frame the rest of the stream.
// It must also return the SHA-256 of exactly those streamed bytes, so the agent
// records in its manifest what the receiver actually stored (not a stale
// pre-read hash). This is the core of the live-file ("content hash mismatch")
// fix: a log/journal that grows or shrinks mid-copy must not corrupt the pass.
func TestStreamFileDataExactSizeAndHash(t *testing.T) {
	dir := t.TempDir()

	// Grow case: file is LARGER than the declared size (it was appended to after
	// we stat'd it). Only the first `size` bytes must be sent.
	grow := filepath.Join(dir, "grow")
	full := bytes.Repeat([]byte("A"), 150)
	if err := os.WriteFile(grow, full, 0o644); err != nil {
		t.Fatal(err)
	}
	assertStream(t, grow, 100, full[:100])

	// Shrink case: file is SMALLER than the declared size (it was truncated after
	// we stat'd it). The remainder must be zero-padded up to `size`.
	shrink := filepath.Join(dir, "shrink")
	short := bytes.Repeat([]byte("B"), 60)
	if err := os.WriteFile(shrink, short, 0o644); err != nil {
		t.Fatal(err)
	}
	want := make([]byte, 100)
	copy(want, short) // first 60 = 'B', last 40 = zero
	assertStream(t, shrink, 100, want)

	// Exact case: file matches the declared size.
	exact := filepath.Join(dir, "exact")
	body := []byte("hello=world\n")
	if err := os.WriteFile(exact, body, 0o644); err != nil {
		t.Fatal(err)
	}
	assertStream(t, exact, int64(len(body)), body)
}

// assertStream streams path at the given declared size and checks the bytes on
// the wire (concatenated MsgFileData payloads) and the returned hash both equal
// wantBytes.
func assertStream(t *testing.T, path string, size int64, wantBytes []byte) {
	t.Helper()
	var raw bytes.Buffer
	w := bufio.NewWriter(&raw)
	n, gotHash, err := streamFileData(w, path, size)
	if err != nil {
		t.Fatalf("streamFileData(%s): %v", path, err)
	}
	if err := w.Flush(); err != nil {
		t.Fatal(err)
	}
	if n != size {
		t.Errorf("%s: streamed %d bytes, want exactly %d", path, n, size)
	}
	onWire := decodeFileData(t, raw.Bytes())
	if !bytes.Equal(onWire, wantBytes) {
		t.Errorf("%s: on-wire bytes = %q, want %q", path, onWire, wantBytes)
	}
	sum := sha256.Sum256(wantBytes)
	if gotHash != hex.EncodeToString(sum[:]) {
		t.Errorf("%s: returned hash does not match streamed bytes", path)
	}
}

// decodeFileData concatenates the payloads of the MsgFileData frames in buf.
func decodeFileData(t *testing.T, buf []byte) []byte {
	t.Helper()
	r := bytes.NewReader(buf)
	var out []byte
	for r.Len() > 0 {
		mt, payload, err := protocol.ReadFrame(r)
		if err != nil {
			t.Fatalf("decode frame: %v", err)
		}
		if mt != protocol.MsgFileData {
			t.Fatalf("unexpected frame type %d", mt)
		}
		out = append(out, payload...)
	}
	return out
}
