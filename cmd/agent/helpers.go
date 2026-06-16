package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/tiny125/vm-replication/internal/api"
	"github.com/tiny125/vm-replication/internal/cbt"
	"github.com/tiny125/vm-replication/internal/controlclient"
	"github.com/tiny125/vm-replication/internal/snapshot"
)

// registerSource best-effort registers this host in the control plane inventory.
// Failures are logged but never block replication.
func registerSource(client *controlclient.Client, name, device string) {
	if !client.Enabled() {
		return
	}
	if name == "" {
		name, _ = os.Hostname()
	}
	host, _ := os.Hostname()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := client.RegisterServer(ctx, api.RegisterServerRequest{
		Name:     name,
		Role:     api.RoleSource,
		Hostname: host,
		Device:   device,
	}); err != nil {
		log.Printf("control plane: register source failed: %v", err)
	}
}

// reportSync best-effort reports a completed (or failed) pass to the control
// plane. Failures are logged but never change the agent's exit status.
func reportSync(client *controlclient.Client, jobID int64, res syncResult, runErr error) {
	if !client.Enabled() || jobID == 0 {
		return
	}
	finished := res.finishedAt
	if finished.IsZero() {
		finished = time.Now()
	}
	req := api.ReportSyncRequest{
		Mode:          res.mode,
		StartedAt:     res.startedAt,
		FinishedAt:    finished,
		TotalBlocks:   res.total,
		ChangedBlocks: res.changed,
		BytesOnWire:   res.bytes,
		DurationMS:    finished.Sub(res.startedAt).Milliseconds(),
		OK:            runErr == nil,
	}
	if runErr != nil {
		req.Error = runErr.Error()
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := client.ReportSync(ctx, jobID, req); err != nil {
		log.Printf("control plane: report sync failed: %v", err)
	}
}

// buildTracker constructs the change-block-tracking backend for this run.
func buildTracker(c cfg) (cbt.Tracker, error) {
	switch cbt.Strategy(c.cbtMode) {
	case "", cbt.StrategyHashDiff:
		return cbt.HashDiff{}, nil
	case cbt.StrategyDMEra:
		return cbt.NewDMEra(cbt.DMEraConfig{
			DMName:          c.dmera.name,
			MetaDev:         c.dmera.meta,
			CheckpointFile:  c.dmera.eraFile,
			BlockSize:       c.blockSize,
			EraBlockSectors: c.dmera.eraBlockSec,
		})
	default:
		return nil, fmt.Errorf("unknown cbt strategy %q (want hashdiff|dmera)", c.cbtMode)
	}
}

// prepareSource establishes a read source in the given consistency mode and
// returns the path to read from plus a cleanup function that must always be
// called. For "none" it is a pass-through to the device itself.
func prepareSource(c cfg, mode snapshot.Mode) (string, func(), error) {
	opts := snapshot.Options{
		Mode:        mode,
		Device:      c.device,
		PreHook:     c.preHook,
		PostHook:    c.postHook,
		LVMSnapSize: c.lvSize,
	}
	return snapshot.Prepare(opts)
}

// chooseMode resolves the snapshot mode for a pass. Steady-state passes use the
// operator's -snapshot flag (default none = live, no downtime). When the
// receiver asks for a crash-consistent resync at cutover, we honor an explicit
// -snapshot choice if one was pinned, otherwise auto-detect the least-disruptive
// point-in-time strategy: an LVM snapshot when possible, else ModeNone (read
// live). We never auto-select fsfreeze — holding a freeze across a whole-device
// read would block all writes and wedge the source.
func chooseMode(c cfg, consistent bool) snapshot.Mode {
	pinned := snapshot.Mode(c.snapMode)
	if !consistent {
		if pinned == "" {
			return snapshot.ModeNone
		}
		return pinned
	}
	if pinned != "" && pinned != snapshot.ModeNone {
		return pinned
	}
	return snapshot.DetectMode(c.device)
}

// defaultManifestPath derives a checkpoint filename from the device path,
// e.g. /dev/sda -> sda.cbt, ./disk.img -> disk.img.cbt.
func defaultManifestPath(device string) string {
	base := filepath.Base(device)
	return base + ".cbt"
}

// hostOf strips the port from a host:port address.
func hostOf(addr string) string {
	if i := strings.LastIndex(addr, ":"); i >= 0 {
		return addr[:i]
	}
	return addr
}

func jsonUnmarshal(b []byte, v any) error {
	if err := json.Unmarshal(b, v); err != nil {
		return fmt.Errorf("decode message: %w", err)
	}
	return nil
}

func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for x := n / unit; x >= unit; x /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}
