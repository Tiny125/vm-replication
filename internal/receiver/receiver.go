// Package receiver implements the target side of replication: it accepts a
// block stream from an agent over a connection, verifies each block, and writes
// it at the correct offset on a target device (a raw disk / attached volume on
// the replication server, or a file image for testing).
//
// It is consumed both by the standalone `receiver` command and by the appliance
// control plane (controld), which embeds one listener per active migration.
package receiver

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"sync"
	"time"

	"github.com/tiny125/vm-replication/internal/blockdiff"
	"github.com/tiny125/vm-replication/internal/codec"
	"github.com/tiny125/vm-replication/internal/protocol"
)

// Stats summarizes a completed replication session.
type Stats struct {
	Hello         protocol.Hello
	BlocksWritten int64
	ChangedBlocks int64
	Duration      time.Duration
}

// Progress reports live apply progress during a session: written blocks so far
// out of the expected changed total (totalChanged is the session's full block
// count for a full sync; for deltas it is unknown until Done and reported as 0).
type Progress func(writtenBlocks, totalBlocks int64, fullSync bool)

// ConsistencyFunc decides, once the agent's Hello is in, whether to bounce this
// pass and ask the agent to re-read from a point-in-time snapshot (crash-
// consistent). It is consulted at cutover; returning false (or a nil func) lets
// the pass proceed normally. It is given the Hello so it can avoid re-requesting
// a pass the agent has already marked Consistent.
type ConsistencyFunc func(hello protocol.Hello) bool

// ReplicationGate decides, after a valid Hello, whether replication is enabled
// for this session. It is consulted before any device is opened or written.
// Returning false makes the receiver acknowledge the connection (HelloAck with
// Hold=true) so the appliance can show the agent as connected, but it does NOT
// apply data — it holds until the operator starts replication. A nil gate means
// always enabled (e.g. the standalone receiver). Implementations also use the
// call to record that the agent connected (a connection heartbeat).
type ReplicationGate func(hello protocol.Hello) bool

// HelloCheck validates the agent's Hello against what the session is EXPECTED
// to be, before anything is recorded, opened or written. Returning a non-nil
// error rejects the session with that message. The appliance uses it to verify
// both the agent's IDENTITY (its job id must match this migration's enrollment
// — agent certs are global and receiver ports repeat across appliance
// reinstalls, so a stale agent from an old enrollment on another machine can
// otherwise stream its disk into a new migration's volume) and its GEOMETRY
// (a device size grossly mismatching the declared disk means the wrong disk).
// Both fail at first contact instead of completing a bogus "full sync" that
// only fails much later at cutover. A nil check accepts every valid Hello.
type HelloCheck func(hello protocol.Hello) error

// errConsistentResync is returned by Handle when it deliberately bounced a live
// pass to request a crash-consistent resync. It is an expected control outcome,
// not a failure, so Serve does not surface it via onError.
var errConsistentResync = errors.New("receiver: crash-consistent resync requested")

// errReplicationHeld is returned by Handle when a valid agent connected but
// replication is not started yet, so the session was acknowledged and held
// without applying data. Like errConsistentResync it is an expected control
// outcome, not a failure.
var errReplicationHeld = errors.New("receiver: replication not started (connection held)")

// DrainGrace bounds how long an in-flight session may keep running after
// Serve's context is cancelled (e.g. a cutover freeze stopping this receiver).
// The session gets this long to finish cleanly — a completed pass ends at one
// consistent instant — and is then SEVERED, so nothing can keep writing to a
// frozen image indefinitely. A variable (not a const) so tests can shorten it.
var DrainGrace = 3 * time.Minute

// Serve accepts connections on ln and applies each to devicePath until ctx is
// cancelled or the listener closes. onComplete (if non-nil) is called after each
// successful session; onProgress (if non-nil) is called periodically during a
// session. If once is true, Serve returns after the first successful session.
// On cancellation Serve stops accepting immediately; a session already in
// flight gets DrainGrace to finish and is then severed.
func Serve(ctx context.Context, ln net.Listener, devicePath, manifestPath string, once bool, onComplete func(Stats), onProgress Progress, onError func(error), requestConsistent ConsistencyFunc, replicationGate ReplicationGate, helloCheck HelloCheck) error {
	var connMu sync.Mutex
	var active net.Conn
	go func() {
		<-ctx.Done()
		_ = ln.Close()
		time.Sleep(DrainGrace)
		connMu.Lock()
		if active != nil {
			_ = active.Close() // sever a session that outlived the drain grace
		}
		connMu.Unlock()
	}()
	for {
		conn, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return err
		}
		connMu.Lock()
		active = conn
		connMu.Unlock()
		stats, herr := Handle(conn, devicePath, manifestPath, onProgress, requestConsistent, replicationGate, helloCheck)
		connMu.Lock()
		active = nil
		connMu.Unlock()
		switch {
		case errors.Is(herr, errConsistentResync):
			// Expected: we asked the agent to re-read crash-consistently. Not an error.
			log.Printf("receiver: asked %s to re-read crash-consistently for cutover", conn.RemoteAddr())
		case errors.Is(herr, errReplicationHeld):
			// Expected: agent connected, but replication isn't started yet. Connection
			// recorded by the gate; nothing applied. Not an error.
			log.Printf("receiver: %s connected; holding (replication not started)", conn.RemoteAddr())
		case herr != nil:
			log.Printf("receiver: session from %s ended with error: %v", conn.RemoteAddr(), herr)
			if onError != nil {
				onError(herr)
			}
		default:
			if onComplete != nil {
				onComplete(stats)
			}
		}
		if once && herr == nil {
			return nil
		}
	}
}

// Handle processes a single accepted connection: negotiates the Hello, applies
// every block to devicePath, fsyncs, and writes the applied manifest.
// onProgress (optional) is invoked at session start and every progressEvery
// applied blocks.
func Handle(conn net.Conn, devicePath, manifestPath string, onProgress Progress, requestConsistent ConsistencyFunc, replicationGate ReplicationGate, helloCheck HelloCheck) (Stats, error) {
	defer conn.Close()
	r := bufio.NewReaderSize(conn, 1<<20)
	w := bufio.NewWriterSize(conn, 1<<16)

	t, payload, err := protocol.ReadFrame(r)
	if err != nil {
		return Stats{}, fmt.Errorf("read hello: %w", err)
	}
	if t != protocol.MsgHello {
		return Stats{}, fmt.Errorf("expected hello, got frame type %d", t)
	}
	var hello protocol.Hello
	if err := json.Unmarshal(payload, &hello); err != nil {
		return Stats{}, fmt.Errorf("decode hello: %w", err)
	}
	// Identity/geometry check: reject an agent that is replicating the WRONG
	// THING — a session from another (older) enrollment, or a device whose size
	// grossly mismatches what the migration declared — with a clear reason. It
	// runs FIRST: before the quiesce branch (a rogue must not spoof a quiesce
	// failure either), before the gate records the agent as connected/validated,
	// and before any data can land on the target.
	if helloCheck != nil {
		if cerr := helloCheck(hello); cerr != nil {
			_ = protocol.WriteJSON(w, protocol.MsgHelloAck, protocol.HelloAck{Accepted: false, Message: cerr.Error()})
			_ = w.Flush()
			return Stats{Hello: hello}, cerr
		}
	}
	// Quiesce-failure report: not a data session. The agent couldn't capture a
	// crash-consistent image (e.g. the root could not be remounted read-only) and is
	// telling us so the appliance can fail the cutover fast. It carries no valid
	// device geometry, so ack and return BEFORE Validate / the consistency bounce /
	// any device write — surfaced to the caller via onComplete(Stats{Hello}).
	if hello.QuiesceError != "" {
		log.Printf("receiver: agent %q reports it could not quiesce for cutover: %s", hello.SourceHostname, hello.QuiesceError)
		_ = protocol.WriteJSON(w, protocol.MsgHelloAck, protocol.HelloAck{Accepted: false, Message: "quiesce failure noted"})
		_ = w.Flush()
		return Stats{Hello: hello}, nil
	}
	// Validate before allocating/opening anything: a buggy or hostile (but
	// authenticated) agent must not be able to crash or OOM the receiver.
	if err := hello.Validate(); err != nil {
		_ = protocol.WriteJSON(w, protocol.MsgHelloAck, protocol.HelloAck{Accepted: false, Message: err.Error()})
		_ = w.Flush()
		return Stats{}, err
	}
	// Replication gate: a validated agent has connected. If the operator hasn't
	// started replication yet, acknowledge the connection (so the console shows it
	// as connected) but HOLD — do not open or write the target — and return. The
	// gate also records the connection. Checked before the consistency bounce and
	// before opening the device so nothing is touched until replication is started.
	if replicationGate != nil && !replicationGate(hello) {
		_ = protocol.WriteJSON(w, protocol.MsgHelloAck, protocol.HelloAck{
			Accepted: false,
			Hold:     true,
			Message:  "connection validated; replication not started yet",
		})
		_ = w.Flush()
		return Stats{Hello: hello}, errReplicationHeld
	}
	// Cutover quiesce: if a crash-consistent snapshot is requested for this disk
	// and the agent isn't already sending one, bounce the pass and ask it to
	// re-read from a point-in-time snapshot. We reject (rather than apply) so the
	// live "smear" never lands on the target right before we clone it.
	if requestConsistent != nil && requestConsistent(hello) {
		_ = protocol.WriteJSON(w, protocol.MsgHelloAck, protocol.HelloAck{
			Accepted:         false,
			ConsistentResync: true,
			Message:          "crash-consistent resync requested for cutover",
		})
		_ = w.Flush()
		return Stats{}, errConsistentResync
	}
	// File-transfer session: write the source's files into the output root
	// (devicePath is a directory here, not a block device). This is a wholly
	// separate data path from the block loop below; block sessions never reach it.
	if hello.Mode == protocol.ModeFile {
		return handleFileSession(w, r, devicePath, manifestPath, hello, onProgress)
	}

	log.Printf("receiver: session job=%q source=%q device=%q size=%d block=%d full=%v consistent=%v",
		hello.JobID, hello.SourceHostname, hello.DevicePath, hello.DeviceSize, hello.BlockSize, hello.FullSync, hello.Consistent)

	dev, err := blockdiff.OpenDeviceWrite(devicePath, hello.DeviceSize)
	if err != nil {
		_ = protocol.WriteJSON(w, protocol.MsgHelloAck, protocol.HelloAck{Accepted: false, Message: err.Error()})
		_ = w.Flush()
		return Stats{}, fmt.Errorf("open target: %w", err)
	}
	defer dev.Close()

	if err := protocol.WriteJSON(w, protocol.MsgHelloAck, protocol.HelloAck{Accepted: true}); err != nil {
		return Stats{}, err
	}
	if err := w.Flush(); err != nil {
		return Stats{}, err
	}

	applied := blockdiff.NewManifest(hello.DeviceSize, hello.BlockSize)
	start := time.Now()
	var written int64

	// Delta passes are applied ATOMICALLY: blocks are validated and STAGED to a
	// sidecar file as they arrive, and only written to the device after the
	// agent's Done confirms the pass is complete. An interrupted delta (source
	// powered off mid-pass, network drop, freeze) is discarded WHOLE, so the
	// target always holds the last complete pass. Without this, a partial delta
	// tore the target badly: deltas stream in ascending disk order, so the ext4
	// superblock and early group metadata advanced to the new pass while later
	// blocks stayed old (backup-superblock recovery, mangled top-level dirs,
	// unbootable images). Full syncs still apply directly — staging would double
	// the I/O and disk footprint of an entire baseline, and an incomplete
	// baseline is harmless (full_sync_done is only set on completion and the
	// next pass re-sends it).
	var stage *stageFile
	if !hello.FullSync {
		var serr error
		if stage, serr = newStageFile(manifestPath); serr != nil {
			_ = protocol.WriteJSON(w, protocol.MsgHelloAck, protocol.HelloAck{Accepted: false, Message: "cannot stage the delta pass: " + serr.Error()})
			_ = w.Flush()
			return Stats{}, fmt.Errorf("create staging file: %w", serr)
		}
		defer stage.Discard() // no-op once applied; removes the file on any abort
	}

	// Live progress: a full sync sends every block, so totalBlocks is the
	// expected count and lets the console compute percent + ETA.
	const progressEvery = 16 // every 64 MiB at the 4 MiB default block size
	totalBlocks := blockdiff.NumBlocks(hello.DeviceSize, hello.BlockSize)
	if onProgress != nil {
		onProgress(0, totalBlocks, hello.FullSync)
	}

	for {
		t, payload, err := protocol.ReadFrame(r)
		if err != nil {
			if errors.Is(err, io.EOF) {
				return Stats{}, fmt.Errorf("stream closed before done")
			}
			return Stats{}, fmt.Errorf("read frame: %w", err)
		}
		switch t {
		case protocol.MsgBlock:
			var berr error
			if stage != nil {
				berr = stageBlock(stage, applied, payload)
			} else {
				berr = applyBlock(dev, applied, payload)
			}
			if berr != nil {
				_ = protocol.WriteJSON(w, protocol.MsgDoneAck, protocol.DoneAck{OK: false, Error: berr.Error()})
				_ = w.Flush()
				return Stats{}, berr
			}
			written++
			if onProgress != nil && written%progressEvery == 0 {
				onProgress(written, totalBlocks, hello.FullSync)
			}

		case protocol.MsgDone:
			var done protocol.Done
			if err := json.Unmarshal(payload, &done); err != nil {
				return Stats{}, fmt.Errorf("decode done: %w", err)
			}
			// The pass is complete: for a staged delta, NOW write it to the device.
			if stage != nil {
				if err := stage.Apply(dev); err != nil {
					_ = protocol.WriteJSON(w, protocol.MsgDoneAck, protocol.DoneAck{OK: false, Error: err.Error()})
					_ = w.Flush()
					return Stats{}, fmt.Errorf("apply staged pass: %w", err)
				}
			}
			if err := dev.Sync(); err != nil {
				_ = protocol.WriteJSON(w, protocol.MsgDoneAck, protocol.DoneAck{OK: false, Error: err.Error()})
				_ = w.Flush()
				return Stats{}, fmt.Errorf("sync target: %w", err)
			}
			if manifestPath != "" {
				if err := applied.Save(manifestPath); err != nil {
					log.Printf("receiver: warning: could not save manifest: %v", err)
				}
			}
			if err := protocol.WriteJSON(w, protocol.MsgDoneAck, protocol.DoneAck{OK: true, BlocksWritten: written}); err != nil {
				return Stats{}, err
			}
			if err := w.Flush(); err != nil {
				return Stats{}, err
			}
			if onProgress != nil {
				onProgress(totalBlocks, totalBlocks, hello.FullSync) // session complete
			}
			stats := Stats{Hello: hello, BlocksWritten: written, ChangedBlocks: done.ChangedBlocks, Duration: time.Since(start)}
			log.Printf("receiver: applied %d blocks (%d expected) in %s", written, done.ChangedBlocks, stats.Duration.Round(time.Millisecond))
			return stats, nil

		default:
			return Stats{}, fmt.Errorf("unexpected frame type %d", t)
		}
	}
}

// applyBlock validates one block frame and writes it straight to the device
// (full-sync path; delta passes go through stageBlock instead).
func applyBlock(dev *blockdiff.Device, applied *blockdiff.Manifest, payload []byte) error {
	hdr, raw, err := decodeCheckBlock(applied, payload)
	if err != nil {
		return err
	}
	if err := writeAt(dev, raw, hdr.Offset); err != nil {
		return fmt.Errorf("write block at %d: %w", hdr.Offset, err)
	}
	return nil
}

// stageBlock validates one block frame and appends it to the staging file; it
// is written to the device only when the whole pass has arrived (stage.Apply).
func stageBlock(stage *stageFile, applied *blockdiff.Manifest, payload []byte) error {
	hdr, raw, err := decodeCheckBlock(applied, payload)
	if err != nil {
		return err
	}
	return stage.add(hdr.Offset, raw)
}

// decodeCheckBlock decodes, bounds-checks, decompresses and hash-verifies one
// block frame, and records its fingerprint in the applied manifest. It never
// touches the device — the caller either writes the block directly (full sync)
// or stages it (delta).
func decodeCheckBlock(applied *blockdiff.Manifest, payload []byte) (protocol.BlockHeader, []byte, error) {
	hdr, raw, err := protocol.DecodeBlock(payload)
	if err != nil {
		return hdr, nil, err
	}
	// Bound the declared block before allocating/decompressing: RawLen can never
	// legitimately exceed the negotiated block size, and the write must land
	// inside the device.
	if hdr.RawLen == 0 || int64(hdr.RawLen) > int64(applied.BlockSize) {
		return hdr, nil, fmt.Errorf("block at %d: raw length %d exceeds block size %d", hdr.Offset, hdr.RawLen, applied.BlockSize)
	}
	if hdr.Offset < 0 || hdr.Offset+int64(hdr.RawLen) > applied.DeviceSize {
		return hdr, nil, fmt.Errorf("block at %d (len %d) is out of device bounds %d", hdr.Offset, hdr.RawLen, applied.DeviceSize)
	}
	if hdr.Codec == protocol.CodecFlate {
		raw, err = codec.Decompress(raw, int(hdr.RawLen))
		if err != nil {
			return hdr, nil, fmt.Errorf("decompress block at %d: %w", hdr.Offset, err)
		}
	}
	if err := hdr.Validate(raw); err != nil {
		return hdr, nil, err
	}
	idx := hdr.Offset / int64(applied.BlockSize)
	if idx >= 0 && idx < int64(len(applied.Hashes)) {
		applied.Hashes[idx] = sha256.Sum256(raw)
	}
	return hdr, raw, nil
}

func writeAt(dev *blockdiff.Device, p []byte, off int64) error {
	for len(p) > 0 {
		n, err := dev.WriteAt(p, off)
		if err != nil {
			return err
		}
		p = p[n:]
		off += int64(n)
	}
	return nil
}
