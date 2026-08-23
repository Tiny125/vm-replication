package appliance

import (
	"strings"
	"testing"

	"github.com/tiny125/vm-replication/internal/api"
)

// Local-disk boot puts ONLY the boot disk on the plan's local storage; the data
// disks become Block Storage volumes. Sizing the plan against the sum of every
// disk — which is what the code did while disk boot was single-disk only — now
// buys local storage nobody uses. On a 40 GB root + 400 GB data source that is
// the difference between a plan with a 40 GB disk and one with 440 GB.
func TestBootDiskBytesSizesOnTheBootDiskOnly(t *testing.T) {
	devices := []api.DeviceSpec{
		{Device: "/dev/sda", SizeBytes: 40 << 30},
		{Device: "/dev/sdb", SizeBytes: 400 << 30},
		{Device: "/dev/sdc", SizeBytes: 100 << 30},
	}
	if got, want := bootDiskBytes(devices), int64(40<<30); got != want {
		t.Errorf("bootDiskBytes = %d, want %d (only devices[0] lands on the local disk)", got, want)
	}
	// Single disk: identical to the old total, so nothing regresses.
	one := []api.DeviceSpec{{Device: "/dev/sda", SizeBytes: 20 << 30}}
	if got, want := bootDiskBytes(one), int64(20<<30); got != want {
		t.Errorf("single-disk bootDiskBytes = %d, want %d", got, want)
	}
	if bootDiskBytes(nil) != 0 {
		t.Error("bootDiskBytes(nil) must be 0, not a panic")
	}
}

// The final config profile mixes the instance's own local disk at sda with the
// cloned data volumes at sdb+. Verified against the live API on 2026-08-23:
// a config carrying {"sda":{"disk_id":D},"sdb":{"volume_id":V}} boots and the
// guest sees both devices. Referencing a volume in a config is ALSO what
// attaches it — attaching separately is refused while the instance is offline
// or in rescue mode.
func TestDiskBootConfigMixesLocalDiskAndVolumes(t *testing.T) {
	devs, err := diskBootDeviceMap(4242, []int64{7001, 7002})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	sda, ok := devs["sda"].(map[string]any)
	if !ok {
		t.Fatalf("sda missing or wrong shape: %#v", devs["sda"])
	}
	if sda["disk_id"] != int64(4242) {
		t.Errorf("sda must carry the local disk id, got %#v", sda)
	}
	if _, bad := sda["volume_id"]; bad {
		t.Error("sda must NOT carry a volume_id — the boot disk is local storage")
	}
	for slot, want := range map[string]int64{"sdb": 7001, "sdc": 7002} {
		got, ok := devs[slot].(map[string]any)
		if !ok {
			t.Fatalf("%s missing or wrong shape: %#v", slot, devs[slot])
		}
		if got["volume_id"] != want {
			t.Errorf("%s = %#v, want volume_id %d", slot, got, want)
		}
	}
	if len(devs) != 3 {
		t.Errorf("expected exactly 3 populated slots, got %d: %#v", len(devs), devs)
	}
}

// A bare disk-boot instance has sda taken by its local disk, leaving 7 slots.
// That is exactly the existing create-time cap of 8 disks per migration, so the
// two limits agree — but only if nothing silently drops disks past the end.
func TestDiskBootConfigRejectsMoreDisksThanSlots(t *testing.T) {
	if _, err := diskBootDeviceMap(1, []int64{1, 2, 3, 4, 5, 6, 7}); err != nil {
		t.Fatalf("7 data volumes must fit (sdb–sdh): %v", err)
	}
	_, err := diskBootDeviceMap(1, []int64{1, 2, 3, 4, 5, 6, 7, 8})
	if err == nil {
		t.Fatal("an 8th data volume has no slot and must be an error, not silently dropped")
	}
	if !strings.Contains(err.Error(), "sdh") {
		t.Errorf("the error should name the slot range so the operator can see the limit, got: %v", err)
	}
}

// No data disks is the single-disk case, and must produce exactly what the
// method produced before this feature existed.
func TestDiskBootConfigSingleDiskUnchanged(t *testing.T) {
	devs, err := diskBootDeviceMap(99, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(devs) != 1 {
		t.Fatalf("single-disk config must have exactly one slot, got %#v", devs)
	}
	sda := devs["sda"].(map[string]any)
	if sda["disk_id"] != int64(99) {
		t.Errorf("sda = %#v, want disk_id 99", sda)
	}
}

// The appliance parses the convert script's marker to tell the operator which
// mounts will not come up. Getting this wrong means the warning never fires.
func TestParseFstabMarker(t *testing.T) {
	for _, tc := range []struct {
		name               string
		line               string
		verified, adjusted int
		mounts             []string
	}{
		{"nothing adjusted", "vmrepl-fstab: verified=3 adjusted=0 mounts=", 3, 0, nil},
		{"one adjusted", "vmrepl-fstab: verified=2 adjusted=1 mounts=/srv/appdata", 2, 1, []string{"/srv/appdata"}},
		{"several adjusted", "vmrepl-fstab: verified=0 adjusted=3 mounts=/srv/a,/var/lib/mysql,/backup", 0, 3, []string{"/srv/a", "/var/lib/mysql", "/backup"}},
		{"embedded in other output", "converting\nvmrepl-fstab: verified=1 adjusted=1 mounts=/data\ndone", 1, 1, []string{"/data"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := parseFstabMarker(tc.line)
			if !ok {
				t.Fatal("marker not found")
			}
			if got.Verified != tc.verified || got.Adjusted != tc.adjusted {
				t.Errorf("verified/adjusted = %d/%d, want %d/%d", got.Verified, got.Adjusted, tc.verified, tc.adjusted)
			}
			if strings.Join(got.Mounts, ",") != strings.Join(tc.mounts, ",") {
				t.Errorf("mounts = %v, want %v", got.Mounts, tc.mounts)
			}
		})
	}
	if _, ok := parseFstabMarker("no marker here"); ok {
		t.Error("absent marker must report not-found rather than a zero value")
	}
}

// The warning the operator sees before powering the source off. It has to name
// the mountpoints — "some mounts could not be verified" gives them nothing to
// act on, and this is the last moment backing out is free.
func TestFstabWarningNamesMountpoints(t *testing.T) {
	msg := fstabWarning(fstabMarker{Verified: 1, Adjusted: 2, Mounts: []string{"/srv/appdata", "/var/lib/mysql"}})
	for _, want := range []string{"/srv/appdata", "/var/lib/mysql", "will NOT be mounted"} {
		if !strings.Contains(msg, want) {
			t.Errorf("warning must contain %q, got: %s", want, msg)
		}
	}
	if fstabWarning(fstabMarker{Verified: 3}) != "" {
		t.Error("nothing adjusted must produce no warning at all — no noise on the happy path")
	}
}

// The console must size the plan for local-disk boot against the BOOT disk only.
// Sizing it against the sum silently pushes the operator onto a much larger (and
// much more expensive) plan to buy local storage that nothing will use — the data
// disks become Block Storage volumes.
func TestConsoleSizesDiskBootOnBootDiskOnly(t *testing.T) {
	js := extractJSFunc(t, "function sizeGBForPlan(")
	if !strings.Contains(js, "bootDiskGB()") {
		t.Errorf("sizeGBForPlan must use the boot disk for local-disk boot, not totalDiskGB(); got: %s", js)
	}
	boot := extractJSFunc(t, "function bootDiskGB(")
	if !strings.Contains(boot, ".d_size") {
		t.Errorf("bootDiskGB must read the first disk row's size field; got: %s", boot)
	}
}

// Local-disk boot with more than one disk now creates Block Storage volumes for
// the data disks, so the cutover dialog must let the operator name them. The old
// gate hid that field for every disk-boot migration.
func TestConsoleOffersVolumeNameForMultiDiskLocalBoot(t *testing.T) {
	js := extractJSFunc(t, "async function startMig(")
	if strings.Contains(js, "if(!disk)fields.push({id:'vol_name'") {
		t.Error("vol_name must no longer be hidden for ALL disk-boot migrations — multi-disk local boot creates data volumes that need naming")
	}
	if !strings.Contains(js, "vol_name") {
		t.Error("the cutover dialog must still offer a volume name")
	}
}

// The cost estimate has to account for the data volumes under local-disk boot,
// or a multi-disk migration quotes only the plan price and understates the bill.
func TestConsoleEstimatesDataVolumeCostForLocalBoot(t *testing.T) {
	js := extractJSFunc(t, "function updatePlanInfo(")
	if !strings.Contains(js, "dataDiskGB()") {
		t.Errorf("updatePlanInfo must price the data volumes for local-disk boot; got: %s", js)
	}
}

// The method help text must say what actually happens to extra disks, so the
// operator is not surprised by Block Storage charges they did not expect.
func TestConsoleDiskBootHelpMentionsDataVolumes(t *testing.T) {
	js := extractJSFunc(t, "function methodChanged(")
	if !strings.Contains(js, "boot") || !strings.Contains(js, "Block Storage") {
		t.Errorf("the local-disk help must explain that extra disks become Block Storage volumes; got: %s", js)
	}
}
