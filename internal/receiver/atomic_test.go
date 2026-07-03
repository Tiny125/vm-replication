package receiver

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tiny125/vm-replication/internal/protocol"
)

// Delta passes must be applied ATOMICALLY: an interrupted pass (source powered
// off mid-stream, network drop, cutover freeze) must leave the target device
// EXACTLY as it was — never a partial update. Deltas stream blocks in ascending
// disk order, so a partial apply always tore ext4 metadata: the superblock and
// early group descriptors advanced to the new pass while everything later
// stayed old (migration "testest": backup-superblock recovery, mangled
// top-level directories, unbootable image). Blocks are therefore staged as they
// arrive and written to the device only after the agent's Done.
func TestDeltaPassIsAtomic(t *testing.T) {
	const blockSize = 4096
	devSize := int64(2 * blockSize)
	orig := bytes.Repeat([]byte{0xAA}, int(devSize))
	newBlock := bytes.Repeat([]byte{0xBB}, blockSize)

	// (a) Interrupted delta — one block sent, connection dropped WITHOUT Done:
	//     the device must be untouched and no staging file may linger.
	dir := t.TempDir()
	target := seedTarget(t, dir, orig)
	runSession(t, target, filepath.Join(dir, "t.cbt"), false, newBlock, false)
	if got, _ := os.ReadFile(target); !bytes.Equal(got, orig) {
		t.Fatal("an interrupted delta pass must not modify the device")
	}
	assertNoStaging(t, dir)

	// (b) Completed delta — same block followed by Done: applied exactly.
	dir = t.TempDir()
	target = seedTarget(t, dir, orig)
	runSession(t, target, filepath.Join(dir, "t.cbt"), false, newBlock, true)
	got, _ := os.ReadFile(target)
	if !bytes.Equal(got[:blockSize], newBlock) {
		t.Fatal("a completed delta pass must apply its blocks")
	}
	if !bytes.Equal(got[blockSize:], orig[blockSize:]) {
		t.Fatal("a completed delta pass must not touch unrelated blocks")
	}
	assertNoStaging(t, dir)

	// (c) Interrupted FULL SYNC — still applied directly (staging an 80 GiB
	//     baseline would double its I/O, and an incomplete baseline is harmless:
	//     full_sync_done is only set on completion and the next pass re-sends).
	dir = t.TempDir()
	target = seedTarget(t, dir, orig)
	runSession(t, target, filepath.Join(dir, "t.cbt"), true, newBlock, false)
	if got, _ := os.ReadFile(target); !bytes.Equal(got[:blockSize], newBlock) {
		t.Fatal("full syncs must keep applying directly (documented trade-off)")
	}
}

// seedTarget creates the target "device" file pre-filled with content.
func seedTarget(t *testing.T, dir string, content []byte) string {
	t.Helper()
	p := filepath.Join(dir, "target.img")
	if err := os.WriteFile(p, content, 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// assertNoStaging verifies no staging temp file was left behind in dir.
func assertNoStaging(t *testing.T, dir string) {
	t.Helper()
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if strings.Contains(e.Name(), "staging") {
			t.Errorf("staging file %s left behind", e.Name())
		}
	}
}

// runSession drives one real session against Handle: hello, one block at
// offset 0, then either a proper Done (sendDone) or an abrupt disconnect.
func runSession(t *testing.T, target, manifest string, fullSync bool, block []byte, sendDone bool) {
	t.Helper()
	cli, srv := net.Pipe()
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = Handle(srv, target, manifest, nil, nil, nil, nil)
	}()

	w := bufio.NewWriter(cli)
	if err := protocol.WriteJSON(w, protocol.MsgHello, protocol.Hello{
		ProtocolVersion: 1, BlockSize: len(block), DeviceSize: 2 * int64(len(block)),
		DevicePath: "/dev/sda", FullSync: fullSync,
	}); err != nil {
		t.Fatal(err)
	}
	_ = w.Flush()
	r := bufio.NewReader(cli)
	mt, payload, err := protocol.ReadFrame(r)
	if err != nil || mt != protocol.MsgHelloAck {
		t.Fatalf("hello-ack: type %d err %v", mt, err)
	}
	var ack protocol.HelloAck
	_ = json.Unmarshal(payload, &ack)
	if !ack.Accepted {
		t.Fatalf("session not accepted: %s", ack.Message)
	}

	hdr := protocol.BlockHeader{Offset: 0, RawLen: uint32(len(block)), Codec: protocol.CodecRaw, Hash: sha256.Sum256(block)}
	if err := protocol.WriteFrame(w, protocol.MsgBlock, protocol.EncodeBlock(hdr, block)); err != nil {
		t.Fatal(err)
	}
	_ = w.Flush()

	if sendDone {
		if err := protocol.WriteJSON(w, protocol.MsgDone, protocol.Done{ChangedBlocks: 1}); err != nil {
			t.Fatal(err)
		}
		_ = w.Flush()
		mt, payload, err = protocol.ReadFrame(r)
		if err != nil || mt != protocol.MsgDoneAck {
			t.Fatalf("done-ack: type %d err %v", mt, err)
		}
		var da protocol.DoneAck
		_ = json.Unmarshal(payload, &da)
		if !da.OK {
			t.Fatalf("done rejected: %s", da.Error)
		}
	}
	_ = cli.Close() // abrupt disconnect when !sendDone
	<-done
}
