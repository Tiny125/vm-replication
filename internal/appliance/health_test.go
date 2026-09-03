package appliance

import (
	"context"
	"strings"
	"testing"

	"github.com/tiny125/vm-replication/internal/api"
	"github.com/tiny125/vm-replication/internal/store"
)

// automationServer builds a Server backed by an in-memory store with a Linode
// token and a known appliance instance id set, so Linode-automation-gated code
// paths (e.g. provisioning, plan sizing) can be exercised without real network
// calls.
func automationServer(t *testing.T) *Server {
	t.Helper()
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	st.SetEncryptionKey(make([]byte, 32)) // deterministic test key for at-rest token
	if err := st.SetLinodeToken(context.Background(), "tok-123"); err != nil {
		t.Fatalf("set token: %v", err)
	}
	s := &Server{st: st, ctx: context.Background()}
	s.cfg.ApplianceLinodeID = 42
	s.cfg.RPOTargetSec = 120
	s.cfg.PublicHost = "203.0.113.10"
	s.cfg.ConsolePort = 8080
	return s
}

func TestApplianceDiskFree(t *testing.T) {
	free, total, ok := applianceDiskFree(t.TempDir())
	if !ok {
		t.Skip("statfs unavailable on this platform")
	}
	if total <= 0 {
		t.Errorf("total = %d, want > 0", total)
	}
	if free < 0 || free > total {
		t.Errorf("free = %d, want 0 <= free <= total (%d)", free, total)
	}
	// A path that does not exist must report "unknown", never a zero that the
	// console would render as "0 bytes free".
	if _, _, ok := applianceDiskFree("/nonexistent/vmrepl-does-not-exist"); ok {
		t.Error("a missing path must report ok=false, not zero bytes")
	}
	if _, _, ok := applianceDiskFree(""); ok {
		t.Error("an empty path must report ok=false")
	}
}

func TestLowDiskThreshold(t *testing.T) {
	// Small filesystem: the absolute floor wins, because one delta pass needs
	// roughly the same room regardless of how big the boot disk is.
	if got := lowDiskThreshold(20 << 30); got != lowDiskFloor {
		t.Errorf("20 GiB disk: threshold = %d, want the %d floor", got, int64(lowDiskFloor))
	}
	// Large filesystem: 5% wins.
	if got := lowDiskThreshold(500 << 30); got != (500<<30)/20 {
		t.Errorf("500 GiB disk: threshold = %d, want 5%% (%d)", got, (500<<30)/20)
	}
}

func TestApplianceUndersized(t *testing.T) {
	for _, tc := range []struct {
		name  string
		vcpus int
		mem   int64
		want  bool
	}{
		{"the appliance I tested on: 1 vCPU / 2 GB", 1, 2 << 30, true},
		{"exactly the recommendation", 2, 4 << 30, false},
		{"comfortably above", 4, 8 << 30, false},
		{"enough RAM but one core", 1, 8 << 30, true},
		{"enough cores but little RAM", 4, 2 << 30, true},
		{"unknown RAM must not be treated as low", 2, 0, false},
		// F-27: the recommendation is a PLAN LABEL (4096 MB advertised), but
		// memBytes is what the GUEST sees via /proc/meminfo, which is always a
		// bit less than the advertised size because the kernel reserves some.
		// Measured live on g6-standard-2, the exact plan CONSOLE.md recommends:
		// the guest reports MemTotal = 4009868 kB, ~180 MiB short of 4 GiB.
		{"g6-standard-2 as its guest actually reports MemTotal", 2, 4009868 * 1024, false},
		// A real 2 GB plan's guest, by the same reserve-shrinkage rule of thumb
		// (~95% of the advertised 2013265920 B), must still read as undersized.
		{"a real 2 GB plan's guest MemTotal", 2, 1912602624, true},
		// The vCPU gate must fire regardless of how much RAM is present.
		{"1 vCPU is undersized even with plenty of RAM", 1, 8 << 30, true},
	} {
		if got := applianceUndersized(tc.vcpus, tc.mem); got != tc.want {
			t.Errorf("%s: undersized(%d, %d) = %v, want %v", tc.name, tc.vcpus, tc.mem, got, tc.want)
		}
	}
}

// The warning must fire once and then stay quiet. A low-disk condition is global
// but is reported on every active migration, so an un-latched warning would spam
// the log of every migration on every 5-second poll.
func TestDiskHeadroomWarnsOnceAndRearms(t *testing.T) {
	s := automationServer(t)
	ctx := context.Background()
	m, _, err := s.st.CreateMigration(ctx, api.CreateMigrationRequest{
		Name: "app", SourceHostname: "app", BootTarget: api.BootTargetVolume,
		LinodeType: "g6-standard-1",
		Devices:    []api.DeviceSpec{{Device: "/dev/sda", SizeBytes: 1 << 30}},
	})
	if err != nil {
		t.Fatalf("create migration: %v", err)
	}

	count := func() int {
		evs, err := s.st.Events(ctx, m.ID, 0)
		if err != nil {
			t.Fatalf("events: %v", err)
		}
		n := 0
		for _, e := range evs {
			if strings.Contains(e.Message, "low on disk") {
				n++
			}
		}
		return n
	}

	// The latch is what we are testing, so drive it directly rather than trying
	// to fill a real filesystem.
	_ = s.st.SetSetting(ctx, keyLowDisk, "1")
	for i := 0; i < 5; i++ {
		s.checkDiskHeadroom(ctx)
	}
	if n := count(); n != 0 {
		t.Errorf("an already-latched warning must not repeat, got %d events", n)
	}

	// With plenty of space the latch clears and nothing is announced.
	_ = s.st.SetSetting(ctx, keyLowDisk, "")
	s.checkDiskHeadroom(ctx)
	if n := count(); n != 0 {
		t.Errorf("a healthy appliance must announce nothing, got %d events", n)
	}
}

// Guard the boundary I most want to hold: appliance health must never become a
// cutover gate. A red disk check would strand an operator whose appliance is
// full, because cutting over is how the space gets freed.
func TestApplianceHealthIsNotACutoverGate(t *testing.T) {
	s := automationServer(t)
	ctx := context.Background()
	m, _, err := s.st.CreateMigration(ctx, api.CreateMigrationRequest{
		Name: "app", SourceHostname: "app", BootTarget: api.BootTargetVolume,
		LinodeType: "g6-standard-1",
		Devices:    []api.DeviceSpec{{Device: "/dev/sda", SizeBytes: 1 << 30}},
	})
	if err != nil {
		t.Fatalf("create migration: %v", err)
	}
	got, err := s.st.Migration(ctx, m.ID)
	if err != nil {
		t.Fatalf("migration: %v", err)
	}
	for _, v := range s.validations(got, 0) {
		low := strings.ToLower(v.Name + " " + v.Detail)
		for _, banned := range []string{"disk space", "low on disk", "appliance disk", "headroom"} {
			if strings.Contains(low, banned) {
				t.Errorf("validation %q mentions appliance health (%q) — these gate cutover, and blocking cutover on low disk is backwards", v.Name, v.Detail)
			}
		}
	}
}

// Appliance health is a GLOBAL condition. Rendering it on per-migration cards
// would paint every card at once for one low-disk state — the same pathology as
// the duplicated cutover warnings fixed earlier. It belongs in the settings area
// and nowhere else.
func TestApplianceHealthStaysOutOfMigrationCards(t *testing.T) {
	i := strings.Index(consoleHTML, "function migCard")
	if i < 0 {
		t.Skip("migCard not found; console structure changed")
	}
	rest := consoleHTML[i:]
	if end := strings.Index(rest, "\nfunction "); end > 0 {
		rest = rest[:end]
	}
	for _, banned := range []string{"appliance_disk", "appliance_vcpus", "appliance_undersized", "This appliance"} {
		if strings.Contains(rest, banned) {
			t.Errorf("migCard references %q — appliance health is global and must not render per migration", banned)
		}
	}
}

// The settings area should render it, though.
func TestApplianceHealthRendersInSettings(t *testing.T) {
	for _, want := range []string{"This appliance", "appliance_disk_known", "appliance_undersized", "2 vCPU / 4 GB"} {
		if !strings.Contains(consoleHTML, want) {
			t.Errorf("settings should surface %q", want)
		}
	}
}
