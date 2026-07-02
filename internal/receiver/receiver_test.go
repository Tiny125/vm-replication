package receiver

import (
	"bufio"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
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

// TestHelloCheckReject verifies the device-identity guard: a HelloCheck that
// returns an error (e.g. the agent's device size grossly mismatches the disk the
// migration declared — the "replicated a 512 MiB swap disk into an 80 GiB
// migration" failure) must reject the session in the HelloAck with that message,
// BEFORE any data is applied. A passing check must leave the session accepted.
func TestHelloCheckReject(t *testing.T) {
	const blockSize = 4096
	target := filepath.Join(t.TempDir(), "target.img")
	h := protocol.Hello{
		ProtocolVersion: 1, BlockSize: blockSize, DeviceSize: blockSize,
		DevicePath: "/dev/sda", Consistent: true,
	}

	// Failing check: session must be refused with the check's message.
	reject := func(hello protocol.Hello) error {
		return errors.New("agent device size mismatch: wrong source disk")
	}
	ack := exchangeHelloCheck(t, target, h, nil, reject)
	if ack.Accepted {
		t.Fatal("expected the session to be rejected by the hello check")
	}
	if !strings.Contains(ack.Message, "wrong source disk") {
		t.Fatalf("rejection message %q should carry the check's reason", ack.Message)
	}
	// Nothing may have been written to the target.
	if _, err := os.Stat(target); !os.IsNotExist(err) {
		t.Errorf("target %s must not be created for a rejected session", target)
	}

	// Passing check: session proceeds normally.
	ack = exchangeHelloCheck(t, target, h, nil, func(protocol.Hello) error { return nil })
	if !ack.Accepted {
		t.Fatalf("expected the session to be accepted with a passing check: %s", ack.Message)
	}

	// The check must run BEFORE the quiesce-failure branch too: a rogue agent
	// must not be able to spoof a "could not quiesce" report (which would fail a
	// cutover fast) any more than it can deliver data.
	rogueQuiesce := h
	rogueQuiesce.QuiesceError = "spoofed: root busy"
	ack = exchangeHelloCheck(t, target, rogueQuiesce, nil, reject)
	if ack.Accepted || !strings.Contains(ack.Message, "wrong source disk") {
		t.Fatalf("quiesce report from a rejected agent must be refused, got accepted=%v msg=%q", ack.Accepted, ack.Message)
	}
}

// exchangeHello drives Handle over an in-memory pipe: it sends one Hello and
// returns the receiver's HelloAck (then drops the connection).
func exchangeHello(t *testing.T, target string, h protocol.Hello, want ConsistencyFunc) protocol.HelloAck {
	return exchangeHelloCheck(t, target, h, want, nil)
}

func exchangeHelloCheck(t *testing.T, target string, h protocol.Hello, want ConsistencyFunc, check HelloCheck) protocol.HelloAck {
	t.Helper()
	c, srv := net.Pipe()
	go func() {
		_, _ = Handle(srv, target, "", nil, want, nil, check)
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
