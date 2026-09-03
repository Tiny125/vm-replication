package appliance

import (
	"context"
	"fmt"
	"os"
	"runtime"
	"strconv"
	"strings"
)

// Appliance self-health, deliberately narrow.
//
// The console already reports per-MIGRATION telemetry well (state, progress, ETA,
// throughput, RPO, validations, activity log). What it never showed is anything
// about the machine doing the work. This adds the smallest set that has an action
// attached, and deliberately omits the rest:
//
//   - No /metrics endpoint. Nothing scrapes it: the appliance is a single-tenant
//     box that lives for the days a migration takes. It would also need its own
//     auth gate, since it would carry migration names, source hostnames and
//     destination IPs. Fleet monitoring is what controld already exists for.
//   - No CPU or memory gauges. Sustained 100% CPU during SHA-256 verification is
//     the tool working correctly, not a fault, and there is no action an operator
//     can take mid-migration. Under-provisioning is a SIZING question, answered
//     once by applianceUndersized below.
//   - No replication-volume capacity meter. The replication volume is attached as
//     a RAW, UNMOUNTED block device and written at fixed offsets; it has no
//     filesystem, so it has no free space to report. Undersizing it is caught at
//     the first agent handshake, before any data lands. A meter here would read a
//     meaningless number forever.
//   - Not a validation check. Those gate cutover, and blocking cutover on low disk
//     would be backwards: cutting over and closing the migration is how the space
//     gets freed.

// recommendedVCPUs / recommendedMemBytes mirror the sizing guidance in
// CONSOLE.md ("Prerequisites — sizing the replication server"): 2 vCPU / 4 GB for
// 1-3 concurrent disks. Below that, transfers are slower because each in-flight
// copy does SHA-256 verification at line rate.
const (
	recommendedVCPUs    = 2
	recommendedMemBytes = 4 << 30
)

// minMemBytes is the actual comparison floor used by applianceUndersized. It is
// deliberately LESS than recommendedMemBytes: recommendedMemBytes is the plan's
// ADVERTISED size (a label like "4 GB"), but the value we compare it against
// (hostSpecs' memBytes, read from /proc/meminfo MemTotal) is what the GUEST
// kernel sees — which is always somewhat less than advertised because the
// hypervisor/kernel reserves a slice of it. Comparing a guest measurement
// directly against a plan label means the recommended plan can never clear the
// bar. Measured live on g6-standard-2 (the exact plan CONSOLE.md recommends):
// advertised 4096 MB = 4294967296 B, but guest MemTotal = 4009868 kB =
// 4106104832 B — a ~180 MiB shortfall against the raw label. 90% of the
// advertised size comfortably absorbs that reservation while still catching
// genuinely undersized plans (e.g. a 2 GB plan's guest lands well under it).
const minMemBytes = recommendedMemBytes * 9 / 10

// lowDiskFloor is the absolute headroom the appliance wants on its data
// directory. Replication STAGES each delta pass to a file there before applying
// it atomically (internal/receiver/stage.go), so a pass needs room for its own
// changed bytes, per disk, concurrently. An absolute floor beats a percentage
// because what consumes the space — one delta pass — has a size roughly
// independent of how big the boot disk is.
//
// Tune after observing a busy multi-disk source; this is reasoned, not measured.
const lowDiskFloor = 2 << 30 // 2 GiB

// keyLowDisk latches the low-disk warning so a restart does not re-announce it.
const keyLowDisk = "appliance_low_disk"

// lowDiskThreshold is the greater of the absolute floor and 5% of the filesystem.
func lowDiskThreshold(total int64) int64 {
	if pct := total / 20; pct > lowDiskFloor {
		return pct
	}
	return lowDiskFloor
}

// hostSpecs reports this machine's CPU count and total RAM. Read once at startup:
// neither changes without a reboot, and both are only used for sizing advice.
func hostSpecs() (vcpus int, memBytes int64) {
	vcpus = runtime.NumCPU()
	b, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return vcpus, 0
	}
	for _, line := range strings.Split(string(b), "\n") {
		if !strings.HasPrefix(line, "MemTotal:") {
			continue
		}
		f := strings.Fields(line)
		if len(f) >= 2 {
			if kb, err := strconv.ParseInt(f[1], 10, 64); err == nil {
				return vcpus, kb * 1024
			}
		}
		break
	}
	return vcpus, 0
}

// applianceUndersized reports whether this appliance is below the documented
// recommendation. It is advice, not an error: a smaller appliance works, it is
// just slower.
func applianceUndersized(vcpus int, memBytes int64) bool {
	return vcpus < recommendedVCPUs || (memBytes > 0 && memBytes < minMemBytes)
}

// checkDiskHeadroom warns ONCE when the data directory crosses the low-disk
// threshold, and re-arms only after it recovers well clear of it, so a migration
// hovering at the line does not flap. Emitted to each active migration's activity
// log, because that is where an operator is already looking.
func (s *Server) checkDiskHeadroom(ctx context.Context) {
	free, total, ok := applianceDiskFree(s.cfg.DataDir)
	if !ok || total <= 0 {
		return
	}
	warned, _, _ := s.st.GetSetting(ctx, keyLowDisk)
	low := free < lowDiskThreshold(total)

	// Hysteresis: only clear the latch once free space is comfortably back.
	if warned == "1" {
		if free > lowDiskThreshold(total)*5/4 {
			_ = s.st.SetSetting(ctx, keyLowDisk, "")
		}
		return
	}
	if !low {
		return
	}
	_ = s.st.SetSetting(ctx, keyLowDisk, "1")
	msg := fmt.Sprintf(
		"the appliance is low on disk: %s free of %s in %s. Replication stages each delta pass here before applying it, so a pass can fail with \"no space left on device\". Free space on the appliance, or cut over and close finished migrations.",
		humanBytes(free), humanBytes(total), s.cfg.DataDir)
	migs, err := s.st.ListMigrations(ctx)
	if err != nil {
		return
	}
	for _, m := range migs {
		if isMigrationActive(m.State) {
			_ = s.st.AddEvent(ctx, m.ID, "warn", msg)
		}
	}
}
