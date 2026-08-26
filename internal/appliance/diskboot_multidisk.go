package appliance

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/tiny125/vm-replication/internal/api"
)

// diskBootSlots are the Linode config-profile device slots. Local-disk boot
// takes sda for the instance's own disk, leaving sdb–sdh (7) for cloned data
// volumes — which is exactly the create-time cap of 8 disks per migration.
var diskBootSlots = []string{"sda", "sdb", "sdc", "sdd", "sde", "sdf", "sdg", "sdh"}

// bootDiskBytes is how much local storage a local-disk-boot destination needs:
// the FIRST disk only. The rest of the source's disks become Block Storage
// volumes attached to the same instance, so they must not inflate the plan.
//
// Sizing against the sum (which is what this method did while it was
// single-disk only) buys local storage nothing uses: a 40 GB root + 400 GB data
// source would demand a plan with a 440 GB disk instead of a 40 GB one.
func bootDiskBytes(devices []api.DeviceSpec) int64 {
	if len(devices) == 0 {
		return 0
	}
	return devices[0].SizeBytes
}

// diskBootDeviceMap builds the final config profile's device map: the
// instance's own local disk at sda, and each cloned data volume at sdb onward.
//
// Mixing a disk_id slot and volume_id slots in one config is supported and was
// verified against the live API (2026-08-23): the guest booted and saw both
// devices. It is also how the data volumes get ATTACHED — naming a volume in a
// config profile attaches it, whereas a separate attach call is refused while
// the instance is offline or in rescue mode ("Couldn't choose a configuration
// profile to add this volume to").
func diskBootDeviceMap(bootDiskID int64, dataVolumeIDs []int64) (map[string]any, error) {
	if len(dataVolumeIDs) > len(diskBootSlots)-1 {
		return nil, fmt.Errorf("%d data disks need %d device slots, but the boot disk takes sda and only sdb–sdh remain (7 data disks maximum)",
			len(dataVolumeIDs), len(dataVolumeIDs)+1)
	}
	devices := map[string]any{
		"sda": map[string]any{"disk_id": bootDiskID},
	}
	for i, vid := range dataVolumeIDs {
		devices[diskBootSlots[i+1]] = map[string]any{"volume_id": vid}
	}
	return devices, nil
}

// fstabMarker is what the convert script reports about the migrated machine's
// data mounts. Mounts lists ONLY the adjusted ones — the mountpoints that will
// not come up on the destination.
type fstabMarker struct {
	Verified int
	Adjusted int
	Mounts   []string
}

// parseFstabMarker pulls the convert script's summary line out of its output:
//
//	vmrepl-fstab: verified=2 adjusted=1 mounts=/srv/appdata
func parseFstabMarker(out string) (fstabMarker, bool) {
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "vmrepl-fstab:") {
			continue
		}
		var m fstabMarker
		for _, f := range strings.Fields(strings.TrimPrefix(line, "vmrepl-fstab:")) {
			k, v, ok := strings.Cut(f, "=")
			if !ok {
				continue
			}
			switch k {
			case "verified":
				m.Verified, _ = strconv.Atoi(v)
			case "adjusted":
				m.Adjusted, _ = strconv.Atoi(v)
			case "mounts":
				for _, mp := range strings.Split(v, ",") {
					if mp = strings.TrimSpace(mp); mp != "" {
						m.Mounts = append(m.Mounts, mp)
					}
				}
			}
		}
		return m, true
	}
	return fstabMarker{}, false
}

// fstabWarning is the message the operator sees while the source is still
// running and backing out is still free. It names the mountpoints: "some mounts
// could not be verified" gives them nothing to act on.
//
// Returns "" when there is nothing to warn about, so the happy path stays quiet.
func fstabWarning(m fstabMarker) string {
	if m.Adjusted == 0 || len(m.Mounts) == 0 {
		return ""
	}
	noun := "entry"
	if m.Adjusted != 1 {
		noun = "entries"
	}
	return fmt.Sprintf(
		"%d fstab %s could not be matched to any migrated disk: %s. They have been marked nofail so the migrated machine still boots, but those paths will NOT be mounted — check the disk list before you power the source off.",
		m.Adjusted, noun, strings.Join(m.Mounts, ", "))
}

// --- F-25: don't race the volume attach when booting -----------------------
//
// Naming a volume in a config profile is what ATTACHES it (see
// diskBootDeviceMap), and that attach is asynchronous. Issuing Boot as the very
// next call after CreateConfig races it: measured on 2026-08-26, Linode logged
// linode_boot "failed" ONE SECOND after linode_config_create, the appliance
// then waited out its 10-minute status poll, and the cutover failed. Booting
// the identical config by hand immediately afterwards worked first time, so
// nothing was wrong but the timing.
//
// This costs more than a normal transient failure: it lands AFTER the operator
// has pasted the rescue copy command and waited out the stream, and retrying
// the cutover deletes the instance and its local disk — throwing the whole copy
// away. So we both wait for the attach (the cause) and retry the boot (the
// safety net).
const (
	// bootAttachRetries bounds re-issuing an immediate boot failure. More than
	// one because a single attempt is exactly the bug; small because a config
	// that is genuinely wrong should fail promptly rather than grind.
	bootAttachRetries = 4
	// bootAttachDelay separates the attempts. Seconds, not minutes: the attach
	// completes quickly, and the 10-minute status wait afterwards is already
	// spent doing nothing.
	bootAttachDelay = 5 * time.Second
	// volumeAttachWait bounds how long we wait for the config to finish
	// attaching every data volume before the first boot attempt.
	volumeAttachWait = 2 * time.Minute
)

// errVolumeLookup marks a volume whose state could not be read.
var errVolumeLookup = errors.New("volume lookup failed")

// volumesAttachedTo reports whether every volume is attached to instanceID.
// linodeIDOf returns the instance a volume currently reports itself attached
// to. A lookup error counts as NOT attached — treating it as ready would put us
// straight back into the race this exists to prevent.
func volumesAttachedTo(volumeIDs []int64, instanceID int64, linodeIDOf func(int64) (int64, error)) bool {
	for _, id := range volumeIDs {
		lid, err := linodeIDOf(id)
		if err != nil || lid != instanceID {
			return false
		}
	}
	return true
}
