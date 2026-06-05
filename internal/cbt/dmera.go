package cbt

import (
	"encoding/xml"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
)

// DMEraConfig configures the dm-era backend.
//
// Set up the era device once with scripts/dm-era-setup.sh, which creates a
// dm-era target named DMName backed by metadata device MetaDev over your data
// device. Point the agent's --device at /dev/mapper/<DMName>.
type DMEraConfig struct {
	DMName          string // device-mapper name of the era target (e.g. "vmrepl-data")
	MetaDev         string // era metadata device (e.g. /dev/vg/era_meta)
	CheckpointFile  string // file storing the last-synced era number
	BlockSize       int    // the agent's block size (bytes)
	EraBlockSectors int    // dm-era block size in 512-byte sectors (from setup)
}

// DMEra is a Tracker backed by a device-mapper era target. It reports the
// blocks written since the last checkpoint era.
//
// NOTE: requires root, the device-mapper era target, and the thin-provisioning
// tools (era_invalidate from the `thin-provisioning-tools` package). It operates
// on real devices and cannot run in a sandbox; it is exercised on a real host.
type DMEra struct {
	cfg DMEraConfig
}

// NewDMEra validates configuration and returns a dm-era tracker.
func NewDMEra(cfg DMEraConfig) (*DMEra, error) {
	if cfg.DMName == "" || cfg.MetaDev == "" {
		return nil, fmt.Errorf("cbt/dmera: DMName and MetaDev are required")
	}
	if cfg.BlockSize <= 0 {
		return nil, fmt.Errorf("cbt/dmera: BlockSize must be > 0")
	}
	if cfg.EraBlockSectors <= 0 {
		cfg.EraBlockSectors = 8 // 4 KiB default era block
	}
	if cfg.CheckpointFile == "" {
		cfg.CheckpointFile = cfg.DMName + ".era"
	}
	for _, tool := range []string{"dmsetup", "era_invalidate"} {
		if _, err := exec.LookPath(tool); err != nil {
			return nil, fmt.Errorf("cbt/dmera: required tool %q not found: %w", tool, err)
		}
	}
	return &DMEra{cfg: cfg}, nil
}

func dmRun(name string, args ...string) (string, error) {
	out, err := exec.Command(name, args...).CombinedOutput()
	if err != nil {
		return string(out), fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return string(out), nil
}

// lastEra reads the checkpoint era (0 if none → caller should full-sync).
func (d *DMEra) lastEra() int64 {
	b, err := os.ReadFile(d.cfg.CheckpointFile)
	if err != nil {
		return 0
	}
	n, _ := strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64)
	return n
}

// currentEra parses the current era from `dmsetup status <name>`.
// era status: <start> <len> era <meta_block_size> <used>/<total> <current_era> ...
func (d *DMEra) currentEra() (int64, error) {
	out, err := dmRun("dmsetup", "status", d.cfg.DMName)
	if err != nil {
		return 0, err
	}
	fields := strings.Fields(out)
	if len(fields) < 6 || fields[2] != "era" {
		return 0, fmt.Errorf("cbt/dmera: unexpected status for %s: %q", d.cfg.DMName, strings.TrimSpace(out))
	}
	return strconv.ParseInt(fields[5], 10, 64)
}

// eraBlocks is the XML schema emitted by era_invalidate.
type eraBlocks struct {
	XMLName xml.Name   `xml:"blocks"`
	Ranges  []eraRange `xml:"range"`
	Blocks  []eraBlock `xml:"block"`
}
type eraRange struct {
	Begin int64 `xml:"begin,attr"`
	End   int64 `xml:"end,attr"` // exclusive
}
type eraBlock struct {
	Block int64 `xml:"block,attr"`
}

// Candidates returns the agent block indices overlapping any era block written
// since the checkpoint era. If there is no checkpoint, it requests a full sync.
func (d *DMEra) Candidates(totalBlocks int64) ([]int64, bool, error) {
	since := d.lastEra()
	if since <= 0 {
		return nil, true, nil // no baseline era → full sync
	}

	// Take a metadata snapshot so era_invalidate can read a consistent view of a
	// live device, and always drop it afterward.
	if _, err := dmRun("dmsetup", "message", d.cfg.DMName, "0", "take_metadata_snap"); err != nil {
		return nil, false, fmt.Errorf("take_metadata_snap: %w", err)
	}
	defer dmRun("dmsetup", "message", d.cfg.DMName, "0", "drop_metadata_snap")

	out, err := dmRun("era_invalidate", "--metadata-snapshot", "--written-since", strconv.FormatInt(since, 10), d.cfg.MetaDev)
	if err != nil {
		return nil, false, fmt.Errorf("era_invalidate: %w", err)
	}

	var parsed eraBlocks
	if err := xml.Unmarshal([]byte(out), &parsed); err != nil {
		return nil, false, fmt.Errorf("parse era_invalidate output: %w", err)
	}

	// Map era blocks → agent block indices via byte offsets.
	eraBytes := int64(d.cfg.EraBlockSectors) * 512
	bs := int64(d.cfg.BlockSize)
	dirty := make(map[int64]struct{})
	mark := func(eraBegin, eraEndExcl int64) {
		startByte := eraBegin * eraBytes
		endByte := eraEndExcl * eraBytes // exclusive
		first := startByte / bs
		last := (endByte - 1) / bs
		for i := first; i <= last && i < totalBlocks; i++ {
			if i >= 0 {
				dirty[i] = struct{}{}
			}
		}
	}
	for _, r := range parsed.Ranges {
		mark(r.Begin, r.End)
	}
	for _, b := range parsed.Blocks {
		mark(b.Block, b.Block+1)
	}

	indices := make([]int64, 0, len(dirty))
	for i := range dirty {
		indices = append(indices, i)
	}
	sortInt64(indices)
	return indices, false, nil
}

// Checkpoint records the current era so the next run reports deltas since now.
//
// Correctness depends on era_invalidate --written-since N being INCLUSIVE of
// era N. We store the era observed before advancing (currentEra) and query
// --written-since <that era> next run, so any blocks written during this sync
// (still in the current open era) are re-reported and re-sent — idempotent and
// safe. If a given era_invalidate build is exclusive, those in-flight blocks
// could be missed; verify inclusivity on the target distro before relying on
// dm-era for production (the default hashdiff backend has no such caveat).
func (d *DMEra) Checkpoint() error {
	era, err := d.currentEra()
	if err != nil {
		return err
	}
	// Advance the era so the next snapshot's "written-since" excludes the era we
	// just captured.
	if _, err := dmRun("dmsetup", "message", d.cfg.DMName, "0", "checkpoint"); err != nil {
		return fmt.Errorf("dm-era checkpoint: %w", err)
	}
	tmp := d.cfg.CheckpointFile + ".tmp"
	if err := os.WriteFile(tmp, []byte(strconv.FormatInt(era, 10)), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, d.cfg.CheckpointFile)
}

// Close is a no-op; the era device persists across runs by design.
func (d *DMEra) Close() error { return nil }

func sortInt64(a []int64) {
	// insertion sort is fine: dirty sets are small relative to the disk.
	for i := 1; i < len(a); i++ {
		for j := i; j > 0 && a[j-1] > a[j]; j-- {
			a[j-1], a[j] = a[j], a[j-1]
		}
	}
}
