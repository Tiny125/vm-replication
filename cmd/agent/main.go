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
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/tiny125/vm-replication/internal/api"
	"github.com/tiny125/vm-replication/internal/blockdiff"
	"github.com/tiny125/vm-replication/internal/codec"
	"github.com/tiny125/vm-replication/internal/controlclient"
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

		// Control plane reporting (optional).
		control      = flag.String("control", os.Getenv("CONTROL_URL"), "control plane base URL (default $CONTROL_URL); empty disables reporting")
		controlToken = flag.String("control-token", os.Getenv("CONTROL_TOKEN"), "control plane bearer token (default $CONTROL_TOKEN)")
		controlJob   = flag.Int64("control-job", 0, "control plane numeric job id to report syncs under")
		sourceName   = flag.String("source-name", "", "inventory name for this source (default hostname)")

		// Source consistency for the read (see internal/snapshot).
		snapMode = flag.String("snapshot", "none", "consistency mode for the read: none|fsfreeze|lvm")
		preHook  = flag.String("pre-hook", "", "command to quiesce the app before snapshot (e.g. DB flush)")
		postHook = flag.String("post-hook", "", "command to resume the app after snapshot")
		lvSize   = flag.String("lvm-snapshot-size", "5G", "size of the LVM snapshot when --snapshot=lvm")

		// Change-block tracking backend (see internal/cbt).
		cbtMode       = flag.String("cbt", "hashdiff", "change tracking: hashdiff (rescan+hash) | dmera (device-mapper era)")
		dmeraName     = flag.String("dmera-name", "", "dm-era device-mapper name (when --cbt=dmera)")
		dmeraMeta     = flag.String("dmera-meta", "", "dm-era metadata device path (when --cbt=dmera)")
		dmeraEraFile  = flag.String("dmera-era-file", "", "file storing the last-synced era (default <dmera-name>.era)")
		dmeraEraSects = flag.Int("dmera-era-block-sectors", 8, "dm-era block size in 512-byte sectors")
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

	c := cfg{
		device:     *device,
		target:     *target,
		serverName: sni,
		manifest:   *manifest,
		jobID:      *jobID,
		blockSize:  *blockSize,
		full:       *full,
		compress:   *compress,
		tls:        transport.Files{CertFile: *certFile, KeyFile: *keyFile, CAFile: *caFile},
		snapMode:   *snapMode,
		preHook:    *preHook,
		postHook:   *postHook,
		lvSize:     *lvSize,
		cbtMode:    *cbtMode,
		dmera: dmeraCfg{
			name:        *dmeraName,
			meta:        *dmeraMeta,
			eraFile:     *dmeraEraFile,
			eraBlockSec: *dmeraEraSects,
		},
	}

	client := controlclient.New(*control, *controlToken)
	registerSource(client, *sourceName, *device)

	res, err := run(c)
	reportSync(client, *controlJob, res, err)
	if err != nil {
		log.Fatalf("agent: %v", err)
	}
}

type cfg struct {
	device, target, serverName, manifest, jobID string
	blockSize                                   int
	full, compress                              bool
	tls                                         transport.Files
	snapMode, preHook, postHook, lvSize         string
	cbtMode                                     string
	dmera                                       dmeraCfg
}

type dmeraCfg struct {
	name, meta, eraFile string
	eraBlockSec         int
}

// syncResult captures stats for control-plane reporting and logging.
type syncResult struct {
	mode                  api.SyncMode
	startedAt, finishedAt time.Time
	total, changed, bytes int64
	deviceSize            int64
}

func run(c cfg) (syncResult, error) {
	res := syncResult{mode: api.SyncDelta, startedAt: time.Now()}

	// Establish a consistent read source (snapshot/freeze) if requested. The
	// returned readPath is what we actually replicate from; cleanup releases it.
	readPath, rawCleanup, err := prepareSource(c)
	if err != nil {
		return res, fmt.Errorf("prepare source consistency: %w", err)
	}
	// Run cleanup at most once, and also on SIGINT/SIGTERM so an interrupted run
	// never leaves the source filesystem frozen or an LVM snapshot dangling
	// (deferred cleanup alone would be skipped on a signal-driven exit).
	var cleanupOnce sync.Once
	cleanup := func() { cleanupOnce.Do(rawCleanup) }
	defer cleanup()
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	defer signal.Stop(sigCh)
	go func() {
		if _, ok := <-sigCh; ok {
			log.Printf("agent: signal received, releasing source snapshot/freeze")
			cleanup()
			os.Exit(1)
		}
	}()

	dev, err := blockdiff.OpenDeviceRead(readPath)
	if err != nil {
		return res, fmt.Errorf("open source: %w", err)
	}
	defer dev.Close()
	res.deviceSize = dev.Size

	// Load the previous checkpoint unless a full sync is forced.
	var prev *blockdiff.Manifest
	if !c.full {
		prev, err = blockdiff.LoadManifest(c.manifest)
		if err != nil {
			return res, fmt.Errorf("load manifest: %w", err)
		}
	}
	fullSync := !prev.Matches(dev.Size, c.blockSize)
	if fullSync && prev != nil {
		log.Printf("manifest geometry changed; promoting to full sync")
	}
	if fullSync {
		res.mode = api.SyncFull
	}

	next := blockdiff.NewManifest(dev.Size, c.blockSize)
	total := blockdiff.NumBlocks(dev.Size, c.blockSize)
	res.total = total

	// Pick the change-tracking backend and ask which blocks to consider.
	tracker, err := buildTracker(c)
	if err != nil {
		return res, fmt.Errorf("cbt: %w", err)
	}
	defer tracker.Close()
	candidates, considerAllCBT, err := tracker.Candidates(total)
	if err != nil {
		return res, fmt.Errorf("cbt candidates: %w", err)
	}
	considerAll := considerAllCBT || fullSync
	if !considerAll {
		// dm-era partial scan: blocks not reported dirty keep their prior hash.
		copy(next.Hashes, prev.Hashes)
		log.Printf("dm-era: %d candidate dirty blocks of %d", len(candidates), total)
	}

	// Connect to the receiver over mTLS.
	tlsCfg, err := transport.ClientConfig(c.tls, c.serverName)
	if err != nil {
		return res, err
	}
	conn, err := tls.Dial("tcp", c.target, tlsCfg)
	if err != nil {
		return res, fmt.Errorf("dial receiver: %w", err)
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
		return res, fmt.Errorf("send hello: %w", err)
	}
	if err := w.Flush(); err != nil {
		return res, err
	}
	if err := expectAck(r); err != nil {
		return res, err
	}

	start := time.Now()
	buf := make([]byte, c.blockSize)
	var changed, bytesWire int64

	// process reads, hashes, and (if changed vs the checkpoint) ships block i.
	process := func(i int64) error {
		blen := blockdiff.BlockLen(dev.Size, c.blockSize, i)
		off := i * int64(c.blockSize)
		if _, err := readBlockAt(dev, buf[:blen], off); err != nil {
			return fmt.Errorf("read block %d: %w", i, err)
		}
		sum := sha256.Sum256(buf[:blen])
		next.Hashes[i] = sum

		if !fullSync && prev.Hashes[i] == sum {
			return nil // unchanged since checkpoint (tracker may over-report)
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
		return nil
	}

	if considerAll {
		for i := int64(0); i < total; i++ {
			if err := process(i); err != nil {
				return res, err
			}
		}
	} else {
		for _, i := range candidates {
			if err := process(i); err != nil {
				return res, err
			}
		}
	}

	if err := protocol.WriteJSON(w, protocol.MsgDone, protocol.Done{
		TotalBlocks:   total,
		ChangedBlocks: changed,
		BytesOnWire:   bytesWire,
	}); err != nil {
		return res, fmt.Errorf("send done: %w", err)
	}
	if err := w.Flush(); err != nil {
		return res, err
	}

	ack, err := expectDoneAck(r)
	if err != nil {
		return res, err
	}
	if !ack.OK {
		return res, fmt.Errorf("receiver reported failure: %s", ack.Error)
	}

	// Only commit the new checkpoint once the receiver confirms the apply.
	if err := next.Save(c.manifest); err != nil {
		return res, fmt.Errorf("save manifest: %w", err)
	}
	// Advance the change-tracker's marker (no-op for hashdiff).
	if err := tracker.Checkpoint(); err != nil {
		log.Printf("warning: cbt checkpoint failed: %v", err)
	}

	res.changed, res.bytes = changed, bytesWire
	res.finishedAt = time.Now()
	dur := time.Since(start)
	log.Printf("%s sync complete: %d/%d blocks changed, %s on wire in %s (%.1f MiB/s)",
		res.mode, changed, total, humanBytes(bytesWire), dur.Round(time.Millisecond),
		float64(bytesWire)/(1024*1024)/dur.Seconds())
	return res, nil
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
