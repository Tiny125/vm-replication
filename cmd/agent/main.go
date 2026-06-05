// Command agent is the source-side replication agent. It reads a local block
// device (or file image), computes a per-block SHA-256 fingerprint, and streams
// only the blocks that changed since the last run to a target receiver over
// mutually-authenticated TLS.
//
// The first run (or --full) ships every block; subsequent runs consult the
// on-disk manifest checkpoint and ship only deltas. Run it on a timer (systemd
// timer / cron) to get continuous, near-RPO replication.
package main

import (
	"bufio"
	"crypto/sha256"
	"crypto/tls"
	"flag"
	"fmt"
	"log"
	"os"
	"time"

	"github.com/tiny125/vm-replication/internal/blockdiff"
	"github.com/tiny125/vm-replication/internal/codec"
	"github.com/tiny125/vm-replication/internal/protocol"
	"github.com/tiny125/vm-replication/internal/transport"
)

func main() {
	var (
		device     = flag.String("device", "", "source block device or image file (required)")
		target     = flag.String("target", "", "receiver address host:port (required)")
		serverName = flag.String("server-name", "", "expected receiver cert SAN (defaults to target host)")
		manifest   = flag.String("manifest", "", "path to CBT manifest checkpoint (default <device-basename>.cbt)")
		jobID      = flag.String("job", "default", "job identifier")
		blockSize  = flag.Int("block-size", blockdiff.DefaultBlockSize, "block granularity in bytes")
		full       = flag.Bool("full", false, "force a full sync (ignore existing manifest)")
		compress   = flag.Bool("compress", true, "DEFLATE-compress blocks on the wire")
		certFile   = flag.String("cert", "agent.crt", "agent certificate PEM")
		keyFile    = flag.String("key", "agent.key", "agent private key PEM")
		caFile     = flag.String("ca", "ca.crt", "CA certificate PEM")
	)
	flag.Parse()

	if *device == "" || *target == "" {
		flag.Usage()
		log.Fatal("agent: --device and --target are required")
	}
	if *manifest == "" {
		*manifest = defaultManifestPath(*device)
	}
	sni := *serverName
	if sni == "" {
		sni = hostOf(*target)
	}

	if err := run(cfg{
		device:     *device,
		target:     *target,
		serverName: sni,
		manifest:   *manifest,
		jobID:      *jobID,
		blockSize:  *blockSize,
		full:       *full,
		compress:   *compress,
		tls:        transport.Files{CertFile: *certFile, KeyFile: *keyFile, CAFile: *caFile},
	}); err != nil {
		log.Fatalf("agent: %v", err)
	}
}

type cfg struct {
	device, target, serverName, manifest, jobID string
	blockSize                                    int
	full, compress                               bool
	tls                                          transport.Files
}

func run(c cfg) error {
	dev, err := blockdiff.OpenDeviceRead(c.device)
	if err != nil {
		return fmt.Errorf("open source: %w", err)
	}
	defer dev.Close()

	// Load the previous checkpoint unless a full sync is forced.
	var prev *blockdiff.Manifest
	if !c.full {
		prev, err = blockdiff.LoadManifest(c.manifest)
		if err != nil {
			return fmt.Errorf("load manifest: %w", err)
		}
	}
	fullSync := !prev.Matches(dev.Size, c.blockSize)
	if fullSync && prev != nil {
		log.Printf("manifest geometry changed; promoting to full sync")
	}

	next := blockdiff.NewManifest(dev.Size, c.blockSize)
	total := blockdiff.NumBlocks(dev.Size, c.blockSize)

	// Connect to the receiver over mTLS.
	tlsCfg, err := transport.ClientConfig(c.tls, c.serverName)
	if err != nil {
		return err
	}
	conn, err := tls.Dial("tcp", c.target, tlsCfg)
	if err != nil {
		return fmt.Errorf("dial receiver: %w", err)
	}
	defer conn.Close()
	w := bufio.NewWriterSize(conn, 1<<20)
	r := bufio.NewReaderSize(conn, 1<<16)

	host, _ := os.Hostname()
	if err := protocol.WriteJSON(w, protocol.MsgHello, protocol.Hello{
		ProtocolVersion: 1,
		JobID:           c.jobID,
		SourceHostname:  host,
		DevicePath:      c.device,
		DeviceSize:      dev.Size,
		BlockSize:       c.blockSize,
		FullSync:        fullSync,
	}); err != nil {
		return fmt.Errorf("send hello: %w", err)
	}
	if err := w.Flush(); err != nil {
		return err
	}
	if err := expectAck(r); err != nil {
		return err
	}

	start := time.Now()
	buf := make([]byte, c.blockSize)
	var changed, bytesWire int64

	for i := int64(0); i < total; i++ {
		blen := blockdiff.BlockLen(dev.Size, c.blockSize, i)
		off := i * int64(c.blockSize)
		if _, err := readBlockAt(dev, buf[:blen], off); err != nil {
			return fmt.Errorf("read block %d: %w", i, err)
		}
		sum := sha256.Sum256(buf[:blen])
		next.Hashes[i] = sum

		if !fullSync && prev.Hashes[i] == sum {
			continue // unchanged since checkpoint
		}

		payload := buf[:blen]
		cdc := protocol.CodecRaw
		if c.compress {
			if z, zerr := codec.Compress(payload); zerr == nil && len(z) < blen {
				payload, cdc = z, protocol.CodecFlate
			}
		}
		hdr := protocol.BlockHeader{Offset: off, RawLen: uint32(blen), Codec: cdc, Hash: sum}
		if err := protocol.WriteFrame(w, protocol.MsgBlock, protocol.EncodeBlock(hdr, payload)); err != nil {
			return fmt.Errorf("send block %d: %w", i, err)
		}
		changed++
		bytesWire += int64(len(payload))
	}

	if err := protocol.WriteJSON(w, protocol.MsgDone, protocol.Done{
		TotalBlocks:   total,
		ChangedBlocks: changed,
		BytesOnWire:   bytesWire,
	}); err != nil {
		return fmt.Errorf("send done: %w", err)
	}
	if err := w.Flush(); err != nil {
		return err
	}

	ack, err := expectDoneAck(r)
	if err != nil {
		return err
	}
	if !ack.OK {
		return fmt.Errorf("receiver reported failure: %s", ack.Error)
	}

	// Only commit the new checkpoint once the receiver confirms the apply.
	if err := next.Save(c.manifest); err != nil {
		return fmt.Errorf("save manifest: %w", err)
	}

	mode := "delta"
	if fullSync {
		mode = "full"
	}
	dur := time.Since(start)
	log.Printf("%s sync complete: %d/%d blocks changed, %s on wire in %s (%.1f MiB/s)",
		mode, changed, total, humanBytes(bytesWire), dur.Round(time.Millisecond),
		float64(bytesWire)/(1024*1024)/dur.Seconds())
	return nil
}

func readBlockAt(dev *blockdiff.Device, p []byte, off int64) (int, error) {
	n := 0
	for n < len(p) {
		m, err := dev.ReadAt(p[n:], off+int64(n))
		n += m
		if err != nil {
			return n, err
		}
	}
	return n, nil
}

func expectAck(r *bufio.Reader) error {
	t, payload, err := protocol.ReadFrame(r)
	if err != nil {
		return fmt.Errorf("read hello ack: %w", err)
	}
	if t != protocol.MsgHelloAck {
		return fmt.Errorf("expected hello-ack, got frame type %d", t)
	}
	var ack protocol.HelloAck
	if err := jsonUnmarshal(payload, &ack); err != nil {
		return err
	}
	if !ack.Accepted {
		return fmt.Errorf("receiver rejected session: %s", ack.Message)
	}
	return nil
}

func expectDoneAck(r *bufio.Reader) (protocol.DoneAck, error) {
	var ack protocol.DoneAck
	t, payload, err := protocol.ReadFrame(r)
	if err != nil {
		return ack, fmt.Errorf("read done ack: %w", err)
	}
	if t != protocol.MsgDoneAck {
		return ack, fmt.Errorf("expected done-ack, got frame type %d", t)
	}
	return ack, jsonUnmarshal(payload, &ack)
}
