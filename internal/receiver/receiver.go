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

// errConsistentResync is returned by Handle when it deliberately bounced a live
// pass to request a crash-consistent resync. It is an expected control outcome,
// not a failure, so Serve does not surface it via onError.
var errConsistentResync = errors.New("receiver: crash-consistent resync requested")

// Serve accepts connections on ln and applies each to devicePath until ctx is
// cancelled or the listener closes. onComplete (if non-nil) is called after each
// successful session; onProgress (if non-nil) is called periodically during a
// session. If once is true, Serve returns after the first successful session.
func Serve(ctx context.Context, ln net.Listener, devicePath, manifestPath string, once bool, onComplete func(Stats), onProgress Progress, onError func(error), requestConsistent ConsistencyFunc) error {
	go func() {
		<-ctx.Done()
		_ = ln.Close()
	}()
	for {
		conn, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return err
		}
		stats, herr := Handle(conn, devicePath, manifestPath, onProgress, requestConsistent)
		switch {
		case errors.Is(herr, errConsistentResync):
			// Expected: we asked the agent to re-read crash-consistently. Not an error.
			log.Printf("receiver: asked %s to re-read crash-consistently for cutover", conn.RemoteAddr())
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
func Handle(conn net.Conn, devicePath, manifestPath string, onProgress Progress, requestConsistent ConsistencyFunc) (Stats, error) {
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
	// Validate before allocating/opening anything: a buggy or hostile (but
	// authenticated) agent must not be able to crash or OOM the receiver.
	if err := hello.Validate(); err != nil {
		_ = protocol.WriteJSON(w, protocol.MsgHelloAck, protocol.HelloAck{Accepted: false, Message: err.Error()})
		_ = w.Flush()
		return Stats{}, err
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
			if err := applyBlock(dev, applied, payload); err != nil {
				_ = protocol.WriteJSON(w, protocol.MsgDoneAck, protocol.DoneAck{OK: false, Error: err.Error()})
				_ = w.Flush()
				return Stats{}, err
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

func applyBlock(dev *blockdiff.Device, applied *blockdiff.Manifest, payload []byte) error {
	hdr, raw, err := protocol.DecodeBlock(payload)
	if err != nil {
		return err
	}
	// Bound the declared block before allocating/decompressing: RawLen can never
	// legitimately exceed the negotiated block size, and the write must land
	// inside the device.
	if hdr.RawLen == 0 || int64(hdr.RawLen) > int64(applied.BlockSize) {
		return fmt.Errorf("block at %d: raw length %d exceeds block size %d", hdr.Offset, hdr.RawLen, applied.BlockSize)
	}
	if hdr.Offset < 0 || hdr.Offset+int64(hdr.RawLen) > applied.DeviceSize {
		return fmt.Errorf("block at %d (len %d) is out of device bounds %d", hdr.Offset, hdr.RawLen, applied.DeviceSize)
	}
	if hdr.Codec == protocol.CodecFlate {
		raw, err = codec.Decompress(raw, int(hdr.RawLen))
		if err != nil {
			return fmt.Errorf("decompress block at %d: %w", hdr.Offset, err)
		}
	}
	if err := hdr.Validate(raw); err != nil {
		return err
	}
	if err := writeAt(dev, raw, hdr.Offset); err != nil {
		return fmt.Errorf("write block at %d: %w", hdr.Offset, err)
	}
	idx := hdr.Offset / int64(applied.BlockSize)
	if idx >= 0 && idx < int64(len(applied.Hashes)) {
		applied.Hashes[idx] = sha256.Sum256(raw)
	}
	return nil
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
