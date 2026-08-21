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
	"github.com/tiny125/vm-replication/internal/receiver"
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

// The pass record has to survive between agent runs (each run is a one-shot
// process), and a missing or corrupt record must degrade to "no pass reported"
// rather than failing the pass that is about to run.
func TestFilePassStateRoundTrip(t *testing.T) {
	manifest := filepath.Join(t.TempDir(), "disk0.cbt")

	// Nothing saved yet.
	if got := loadFilePass(manifest); got != (filePassState{}) {
		t.Fatalf("a missing record must load as the zero value, got %+v", got)
	}

	want := filePassState{Seq: 3, Complete: true, Entries: 99030, Bytes: 2 << 30, At: 1755600000, Target: "192.0.2.9:5999"}
	if err := want.save(manifest); err != nil {
		t.Fatalf("save: %v", err)
	}
	if got := loadFilePass(manifest); got != want {
		t.Fatalf("round trip:\n got %+v\nwant %+v", got, want)
	}

	// A corrupt record must not error out either.
	if err := os.WriteFile(filePassPath(manifest), []byte("{not json"), 0o600); err != nil {
		t.Fatalf("write corrupt: %v", err)
	}
	if got := loadFilePass(manifest); got != (filePassState{}) {
		t.Fatalf("a corrupt record must load as the zero value, got %+v", got)
	}
}

// The control hop carries the pass report; the destination hop must not (the
// destination records no baselines, and the report is not its business).
func TestFileHelloCarriesLastPassOnlyForControlHop(t *testing.T) {
	c := cfg{jobID: "job-1", root: "/"}
	last := filePassState{Seq: 4, Complete: true, Entries: 10, Bytes: 1 << 20, At: 99, Target: "192.0.2.9:5999"}

	ctl := fileHello(c, last)
	if !ctl.ReportsPasses {
		t.Fatal("a current agent must advertise ReportsPasses so the appliance can tell it apart from an old build")
	}
	if ctl.LastPassSeq != last.Seq || !ctl.LastPassComplete ||
		ctl.LastPassEntries != last.Entries || ctl.LastPassBytes != last.Bytes ||
		ctl.LastPassTarget != last.Target {
		t.Fatalf("control Hello lost the pass report: %+v", ctl)
	}
	if ctl.Mode != protocol.ModeFile {
		t.Fatalf("file Hello must set Mode=file, got %q", ctl.Mode)
	}

	dst := fileHello(c, filePassState{})
	if dst.LastPassSeq != 0 || dst.LastPassComplete || dst.LastPassTarget != "" {
		t.Fatalf("the destination hop must not carry a pass report: %+v", dst)
	}
}

// The agent's exclusions and the receiver's delete-pass protections must be the
// SAME list. They were two hand-maintained lists, each documented as mirroring
// the other, and they had drifted — which is how the prune came to delete
// /dev/null, /run/sshd and the receiver's own binary from a live destination.
//
// The only permitted divergence is the agent's own source-side checkpoints,
// which exist on the source and never on the destination.
func TestAgentAndReceiverExclusionsAgree(t *testing.T) {
	for _, rel := range []string{
		// runtime plumbing
		"proc", "sys", "dev", "dev/null", "run", "run/sshd", "tmp", "var/tmp", "var/run", "var/lock",
		// destination's own boot / identity / network
		"boot", "boot/grub/grub.cfg", "vmlinuz", "initrd.img", "lib/modules",
		"etc/fstab", "etc/machine-id", "etc/resolv.conf", "etc/netplan",
		"etc/systemd/network", "etc/NetworkManager/system-connections", "etc/network/interfaces",
		// never migrated
		"mnt", "media", "lost+found",
		// replication tooling
		"usr/local/bin/vmrepl-agent", "usr/local/bin/vmrepl-receiver",
		"etc/vm-repl", "etc/vmrepl",
		"etc/systemd/system/vmrepl-agent.service", "etc/systemd/system/vmrepl-agent.timer",
		"etc/systemd/system/vmrepl-receiver.service",
		// things that MUST migrate
		"etc/shadow", "etc/passwd", "etc/ssh/sshd_config", "root/.ssh/authorized_keys",
		"etc/hostname", "etc/systemd/system/app.service", "usr/bin/python3",
		"var/www/html/index.html", "opt/appdata/x.bin",
	} {
		agent := excludedFromFileCopy(rel)
		recv := receiver.IsProtectedDestPath(rel)
		if agent != recv {
			t.Errorf("%s: agent excludes=%v but receiver protects=%v — the lists must agree, or an excluded path gets DELETED on the destination", rel, agent, recv)
		}
	}

	// The one permitted divergence.
	const ckpt = "var/lib/vmrepl-source-abc123.cbt"
	if !excludedFromFileCopy(ckpt) {
		t.Errorf("%s must not be copied to the destination", ckpt)
	}
	if receiver.IsProtectedDestPath(ckpt) {
		t.Errorf("%s only exists on the source; the receiver need not protect it", ckpt)
	}
}
