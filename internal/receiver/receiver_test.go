package receiver

import (
	"bufio"
	"crypto/sha256"
	"encoding/json"
	"net"
	"path/filepath"
	"testing"

	"github.com/tiny125/vm-replication/internal/blockdiff"
	"github.com/tiny125/vm-replication/internal/protocol"
)

func TestApplyBlockBounds(t *testing.T) {
	const blockSize = 4096
	const devSize = int64(3 * blockSize)

	path := filepath.Join(t.TempDir(), "target.img")
	dev, err := blockdiff.OpenDeviceWrite(path, devSize)
	if err != nil {
		t.Fatalf("OpenDeviceWrite: %v", err)
	}
	defer dev.Close()
	applied := blockdiff.NewManifest(devSize, blockSize)

	// Valid block at offset blockSize.
	good := make([]byte, blockSize)
	for i := range good {
		good[i] = byte(i)
	}
	validHdr := protocol.BlockHeader{Offset: blockSize, RawLen: blockSize, Codec: protocol.CodecRaw, Hash: sha256.Sum256(good)}
	if err := applyBlock(dev, applied, protocol.EncodeBlock(validHdr, good)); err != nil {
		t.Fatalf("valid block rejected: %v", err)
	}
	readback := make([]byte, blockSize)
	if _, err := dev.ReadAt(readback, blockSize); err != nil {
		t.Fatalf("readback: %v", err)
	}
	if string(readback) != string(good) {
		t.Errorf("written block does not match")
	}

	// RawLen larger than the block size must be rejected before allocation.
	tooBig := protocol.BlockHeader{Offset: 0, RawLen: blockSize + 1, Codec: protocol.CodecRaw}
	if err := applyBlock(dev, applied, protocol.EncodeBlock(tooBig, good)); err == nil {
		t.Error("expected rejection of oversized RawLen")
	}

	// Hostile ~4 GiB RawLen with a tiny payload must be rejected (no huge alloc).
	huge := protocol.BlockHeader{Offset: 0, RawLen: 0xFFFFFFFF, Codec: protocol.CodecFlate}
	if err := applyBlock(dev, applied, protocol.EncodeBlock(huge, []byte{0x01, 0x02})); err == nil {
		t.Error("expected rejection of hostile RawLen")
	}

	// A write that runs past the end of the device must be rejected.
	oob := protocol.BlockHeader{Offset: devSize, RawLen: blockSize, Codec: protocol.CodecRaw, Hash: sha256.Sum256(good)}
	if err := applyBlock(dev, applied, protocol.EncodeBlock(oob, good)); err == nil {
		t.Error("expected rejection of out-of-bounds offset")
	}

	// Negative offset must be rejected.
	neg := protocol.BlockHeader{Offset: -1, RawLen: blockSize, Codec: protocol.CodecRaw, Hash: sha256.Sum256(good)}
	if err := applyBlock(dev, applied, protocol.EncodeBlock(neg, good)); err == nil {
		t.Error("expected rejection of negative offset")
	}
}

// TestConsistentResyncRequest verifies the cutover quiesce handshake: a live
// (non-consistent) pass is bounced with ConsistentResync set, while a pass the
// agent already marked Consistent is accepted normally.
func TestConsistentResyncRequest(t *testing.T) {
	const blockSize = 4096
	target := filepath.Join(t.TempDir(), "target.img")

	hello := func(consistent bool) protocol.Hello {
		return protocol.Hello{
			ProtocolVersion: 1, BlockSize: blockSize, DeviceSize: blockSize,
			DevicePath: "/dev/sda", Consistent: consistent,
		}
	}

	// requestConsistent mimics the appliance asking for a crash-consistent pass.
	want := func(h protocol.Hello) bool { return !h.Consistent }

	// Live pass: expect a bounce (Accepted=false, ConsistentResync=true).
	ack := exchangeHello(t, target, hello(false), want)
	if ack.Accepted || !ack.ConsistentResync {
		t.Fatalf("live pass: got accepted=%v resync=%v, want accepted=false resync=true", ack.Accepted, ack.ConsistentResync)
	}

	// Consistent pass: must be accepted, not re-requested.
	ack = exchangeHello(t, target, hello(true), want)
	if !ack.Accepted || ack.ConsistentResync {
		t.Fatalf("consistent pass: got accepted=%v resync=%v, want accepted=true resync=false", ack.Accepted, ack.ConsistentResync)
	}
}

// exchangeHello drives Handle over an in-memory pipe: it sends one Hello and
// returns the receiver's HelloAck (then drops the connection).
func exchangeHello(t *testing.T, target string, h protocol.Hello, want ConsistencyFunc) protocol.HelloAck {
	t.Helper()
	c, srv := net.Pipe()
	go func() {
		_, _ = Handle(srv, target, "", nil, want, nil)
	}()
	defer c.Close()

	w := bufio.NewWriter(c)
	if err := protocol.WriteJSON(w, protocol.MsgHello, h); err != nil {
		t.Fatalf("write hello: %v", err)
	}
	if err := w.Flush(); err != nil {
		t.Fatalf("flush hello: %v", err)
	}
	r := bufio.NewReader(c)
	mt, payload, err := protocol.ReadFrame(r)
	if err != nil {
		t.Fatalf("read ack: %v", err)
	}
	if mt != protocol.MsgHelloAck {
		t.Fatalf("got frame type %d, want hello-ack", mt)
	}
	var ack protocol.HelloAck
	if err := json.Unmarshal(payload, &ack); err != nil {
		t.Fatalf("decode ack: %v", err)
	}
	return ack
}
