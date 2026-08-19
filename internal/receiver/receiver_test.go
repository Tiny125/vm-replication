package receiver

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

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

// After Serve's context is cancelled (a cutover freeze), an in-flight session
// gets a bounded grace to finish and is then SEVERED — nothing may keep writing
// to a frozen image indefinitely. This drives a real session that hangs after
// its Hello and asserts Serve returns once the (shortened) grace expires.
func TestServeSeversHungSessionAfterGrace(t *testing.T) {
	oldGrace := DrainGrace
	DrainGrace = 100 * time.Millisecond
	defer func() { DrainGrace = oldGrace }()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(t.TempDir(), "target.img")
	ctx, cancel := context.WithCancel(context.Background())
	served := make(chan error, 1)
	go func() {
		served <- Serve(ctx, ln, target, "", false, nil, nil, nil, nil, nil, nil, nil)
	}()

	// "Agent": open a session, send a valid Hello, get accepted… then hang.
	c, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	w := bufio.NewWriter(c)
	if err := protocol.WriteJSON(w, protocol.MsgHello, protocol.Hello{
		ProtocolVersion: 1, BlockSize: 4096, DeviceSize: 4096, DevicePath: "/dev/sda",
	}); err != nil {
		t.Fatal(err)
	}
	_ = w.Flush()
	r := bufio.NewReader(c)
	if mt, _, err := protocol.ReadFrame(r); err != nil || mt != protocol.MsgHelloAck {
		t.Fatalf("expected hello-ack, got type %d err %v", mt, err)
	}

	cancel() // freeze: stop accepting; the hung session must be severed after the grace
	select {
	case <-served:
		// Serve returned — the hung session was severed and the loop exited.
	case <-time.After(5 * time.Second):
		t.Fatal("Serve did not sever the hung session after the drain grace")
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
		_, _ = Handle(srv, target, "", nil, want, nil, check, nil)
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

// A file-transfer session that the appliance REDIRECTS to the destination must
// not be reported as a completed pass. In direct-file mode the appliance hands
// the agent a DataTarget and applies nothing itself, so treating that handshake
// as a finished sync makes the console announce "initial file copy complete"
// (and unlock cutover) seconds after Start — while the agent is still streaming
// the first byte to the destination. An operator following the guided flow would
// then power the source off mid-copy.
//
// The redirect is a control exchange, exactly like the "hold" and
// "consistent resync" bounces, and must be surfaced the same way: no onComplete.
func TestRedirectedFileSessionIsNotACompletedPass(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	completed := make(chan Stats, 4)
	onComplete := func(s Stats) { completed <- s }
	// The appliance's redirect: destination is up, stream straight to it.
	fileTarget := func(protocol.Hello) (string, string, bool) {
		return "203.0.113.9:5999", "appliance.example", false
	}

	served := make(chan error, 1)
	go func() {
		served <- Serve(ctx, ln, t.TempDir(), "", false, onComplete, nil, nil, nil, nil, nil, fileTarget)
	}()

	c, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.Close()

	w := bufio.NewWriter(c)
	if err := protocol.WriteJSON(w, protocol.MsgHello, protocol.Hello{
		ProtocolVersion: 1, JobID: "job-1", SourceHostname: "src", Mode: protocol.ModeFile,
	}); err != nil {
		t.Fatalf("write hello: %v", err)
	}
	if err := w.Flush(); err != nil {
		t.Fatalf("flush hello: %v", err)
	}

	// The agent must be redirected to the destination.
	mt, payload, err := protocol.ReadFrame(bufio.NewReader(c))
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
	if ack.DataTarget != "203.0.113.9:5999" {
		t.Fatalf("expected a redirect to the destination, got ack %+v", ack)
	}
	c.Close()

	// Nothing was applied here, so no pass may be reported as complete.
	select {
	case s := <-completed:
		t.Fatalf("redirect handshake reported a completed pass (blocks=%d bytes=%d); "+
			"this is what makes the console announce a finished copy before any data has moved",
			s.BlocksWritten, s.BytesOnWire)
	case <-time.After(300 * time.Millisecond):
	}

	cancel()
	<-served
}

// Regression guard for the APPLIANCE-STAGING fallback (no Linode automation, so
// no destination instance): there the appliance IS the receiver, and a completed
// file pass must still be reported as one. Suppressing the redirect's bogus
// completion must not silence this path too.
func TestStagedFileSessionStillCompletes(t *testing.T) {
	root := t.TempDir()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	completed := make(chan Stats, 4)
	// No target => apply locally (the fallback).
	fileTarget := func(protocol.Hello) (string, string, bool) { return "", "", false }
	served := make(chan error, 1)
	go func() {
		served <- Serve(ctx, ln, root, filepath.Join(t.TempDir(), "f.manifest"), false,
			func(s Stats) { completed <- s }, nil, nil, nil, nil, nil, fileTarget)
	}()

	c, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	w, r := bufio.NewWriter(c), bufio.NewReader(c)
	must(t, protocol.WriteJSON(w, protocol.MsgHello, protocol.Hello{ProtocolVersion: 1, Mode: protocol.ModeFile}))
	must(t, w.Flush())
	mt, payload, err := protocol.ReadFrame(r)
	if err != nil || mt != protocol.MsgHelloAck {
		t.Fatalf("hello-ack: %v", err)
	}
	var ack protocol.HelloAck
	must(t, json.Unmarshal(payload, &ack))
	if !ack.Accepted {
		t.Fatalf("staged session must be accepted, got %+v", ack)
	}
	// One directory entry, then a COMPLETE done.
	must(t, protocol.WriteJSON(w, protocol.MsgFileEntry, protocol.FileEntry{Path: "d", Type: "dir", Mode: 0o755}))
	must(t, protocol.WriteJSON(w, protocol.MsgFileDone, protocol.FileDone{Complete: true, Entries: 1}))
	must(t, w.Flush())
	if _, _, err := protocol.ReadFrame(r); err != nil {
		t.Fatalf("done-ack: %v", err)
	}
	c.Close()

	select {
	case s := <-completed:
		if !s.Complete {
			t.Fatal("a fully-walked staged pass must report Complete=true")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the appliance-staging path must still report completed passes")
	}
	cancel()
	<-served
}

// Regression guard for BOTH block methods (volume boot and disk boot): they share
// Serve/onComplete with the file path. A block session must never consult the
// file redirect hook, and must still report completion.
func TestBlockSessionIgnoresFileTarget(t *testing.T) {
	const blockSize = 4096
	target := filepath.Join(t.TempDir(), "target.img")
	consulted := false
	fileTarget := func(protocol.Hello) (string, string, bool) {
		consulted = true
		return "203.0.113.9:5999", "appliance.example", false
	}
	c, srv := net.Pipe()
	done := make(chan Stats, 1)
	go func() {
		st, _ := Handle(srv, target, "", nil, nil, nil, nil, fileTarget)
		done <- st
	}()
	w, r := bufio.NewWriter(c), bufio.NewReader(c)
	// A block Hello sets no Mode.
	must(t, protocol.WriteJSON(w, protocol.MsgHello, protocol.Hello{
		ProtocolVersion: 1, BlockSize: blockSize, DeviceSize: blockSize,
		DevicePath: "/dev/sda", FullSync: true,
	}))
	must(t, w.Flush())
	mt, payload, err := protocol.ReadFrame(r)
	if err != nil || mt != protocol.MsgHelloAck {
		t.Fatalf("hello-ack: %v", err)
	}
	var ack protocol.HelloAck
	must(t, json.Unmarshal(payload, &ack))
	if !ack.Accepted {
		t.Fatalf("block session must be accepted, got %+v", ack)
	}
	if ack.DataTarget != "" {
		t.Fatalf("a block session must never be redirected, got DataTarget=%q", ack.DataTarget)
	}
	if consulted {
		t.Fatal("the file redirect hook must not be consulted for a block session")
	}
	c.Close()
	<-done
}

// Serve(once=true) must not treat a redirect as the completed session it was
// waiting for: nothing was applied, so it has to keep serving.
func TestServeOnceIgnoresRedirect(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	served := make(chan error, 1)
	go func() {
		served <- Serve(ctx, ln, t.TempDir(), "", true, nil, nil, nil, nil, nil, nil,
			func(protocol.Hello) (string, string, bool) { return "203.0.113.9:5999", "appliance.example", false })
	}()

	c, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	w := bufio.NewWriter(c)
	must(t, protocol.WriteJSON(w, protocol.MsgHello, protocol.Hello{ProtocolVersion: 1, Mode: protocol.ModeFile}))
	must(t, w.Flush())
	if _, _, err := protocol.ReadFrame(bufio.NewReader(c)); err != nil {
		t.Fatalf("read ack: %v", err)
	}
	c.Close()

	select {
	case err := <-served:
		t.Fatalf("Serve(once) returned on a redirect (%v); nothing was applied, so it must keep serving", err)
	case <-time.After(400 * time.Millisecond):
	}
	cancel()
	<-served
}
