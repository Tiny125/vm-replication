// Command receiver is the target-side daemon. It listens over mutually-
// authenticated TLS, accepts a block stream from the agent, verifies each
// block's SHA-256, and writes it at the correct offset on the target device
// (a raw disk on a Linode booted in Rescue Mode, or a file image for testing).
//
// On a clean finish it fsyncs the device and writes a target-side manifest so
// the apply is durable and auditable.
package main

import (
	"bufio"
	"crypto/sha256"
	"crypto/tls"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"time"

	"github.com/tiny125/vm-replication/internal/blockdiff"
	"github.com/tiny125/vm-replication/internal/codec"
	"github.com/tiny125/vm-replication/internal/protocol"
	"github.com/tiny125/vm-replication/internal/transport"
)

func main() {
	var (
		listen   = flag.String("listen", ":4444", "listen address")
		device   = flag.String("device", "", "target block device or image file (required)")
		manifest = flag.String("manifest", "", "path to write the applied manifest (default <device-basename>.cbt)")
		once     = flag.Bool("once", false, "exit after the first completed session (handy for tests)")
		certFile = flag.String("cert", "receiver.crt", "receiver certificate PEM")
		keyFile  = flag.String("key", "receiver.key", "receiver private key PEM")
		caFile   = flag.String("ca", "ca.crt", "CA certificate PEM")
	)
	flag.Parse()

	if *device == "" {
		flag.Usage()
		log.Fatal("receiver: --device is required")
	}
	if *manifest == "" {
		*manifest = defaultManifestPath(*device)
	}

	tlsCfg, err := transport.ServerConfig(transport.Files{CertFile: *certFile, KeyFile: *keyFile, CAFile: *caFile})
	if err != nil {
		log.Fatalf("receiver: %v", err)
	}
	ln, err := tls.Listen("tcp", *listen, tlsCfg)
	if err != nil {
		log.Fatalf("receiver: listen: %v", err)
	}
	defer ln.Close()
	log.Printf("receiver listening on %s, applying to %s", *listen, *device)

	for {
		conn, err := ln.Accept()
		if err != nil {
			log.Printf("accept: %v", err)
			continue
		}
		err = handle(conn, *device, *manifest)
		if err != nil {
			log.Printf("session from %s ended with error: %v", conn.RemoteAddr(), err)
		}
		if *once {
			if err == nil {
				return
			}
			log.Fatalf("receiver: session failed in --once mode: %v", err)
		}
	}
}

func handle(conn net.Conn, devicePath, manifestPath string) error {
	defer conn.Close()
	r := bufio.NewReaderSize(conn, 1<<20)
	w := bufio.NewWriterSize(conn, 1<<16)

	// First frame must be Hello.
	t, payload, err := protocol.ReadFrame(r)
	if err != nil {
		return fmt.Errorf("read hello: %w", err)
	}
	if t != protocol.MsgHello {
		return fmt.Errorf("expected hello, got frame type %d", t)
	}
	var hello protocol.Hello
	if err := json.Unmarshal(payload, &hello); err != nil {
		return fmt.Errorf("decode hello: %w", err)
	}
	log.Printf("session: job=%q source=%q device=%q size=%d block=%d full=%v",
		hello.JobID, hello.SourceHostname, hello.DevicePath, hello.DeviceSize, hello.BlockSize, hello.FullSync)

	dev, err := blockdiff.OpenDeviceWrite(devicePath, hello.DeviceSize)
	if err != nil {
		_ = protocol.WriteJSON(w, protocol.MsgHelloAck, protocol.HelloAck{Accepted: false, Message: err.Error()})
		_ = w.Flush()
		return fmt.Errorf("open target: %w", err)
	}
	defer dev.Close()

	if err := protocol.WriteJSON(w, protocol.MsgHelloAck, protocol.HelloAck{Accepted: true}); err != nil {
		return err
	}
	if err := w.Flush(); err != nil {
		return err
	}

	// Track applied hashes so we can persist a durable target manifest.
	applied := blockdiff.NewManifest(hello.DeviceSize, hello.BlockSize)
	start := time.Now()
	var written int64

	for {
		t, payload, err := protocol.ReadFrame(r)
		if err != nil {
			if errors.Is(err, io.EOF) {
				return fmt.Errorf("stream closed before done")
			}
			return fmt.Errorf("read frame: %w", err)
		}
		switch t {
		case protocol.MsgBlock:
			if err := applyBlock(dev, applied, payload); err != nil {
				_ = protocol.WriteJSON(w, protocol.MsgDoneAck, protocol.DoneAck{OK: false, Error: err.Error()})
				_ = w.Flush()
				return err
			}
			written++

		case protocol.MsgDone:
			var done protocol.Done
			if err := json.Unmarshal(payload, &done); err != nil {
				return fmt.Errorf("decode done: %w", err)
			}
			// Durability: flush data to the device before acknowledging.
			if err := dev.Sync(); err != nil {
				_ = protocol.WriteJSON(w, protocol.MsgDoneAck, protocol.DoneAck{OK: false, Error: err.Error()})
				_ = w.Flush()
				return fmt.Errorf("sync target: %w", err)
			}
			if err := applied.Save(manifestPath); err != nil {
				log.Printf("warning: could not save target manifest: %v", err)
			}
			if err := protocol.WriteJSON(w, protocol.MsgDoneAck, protocol.DoneAck{OK: true, BlocksWritten: written}); err != nil {
				return err
			}
			if err := w.Flush(); err != nil {
				return err
			}
			log.Printf("applied %d blocks (%d expected changed) in %s",
				written, done.ChangedBlocks, time.Since(start).Round(time.Millisecond))
			return nil

		default:
			return fmt.Errorf("unexpected frame type %d", t)
		}
	}
}

func applyBlock(dev *blockdiff.Device, applied *blockdiff.Manifest, payload []byte) error {
	hdr, raw, err := protocol.DecodeBlock(payload)
	if err != nil {
		return err
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
	// Record the applied fingerprint in the manifest.
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
