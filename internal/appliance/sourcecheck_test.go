package appliance

import (
	"bytes"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/tiny125/vm-replication/internal/api"
)

func boolp(b bool) *bool { return &b }

// A healthy Ubuntu source must pass all three methods, with the matching
// Linode image recommended for file transfer and "not applicable" wording for
// the block methods (they boot the migrated disk itself).
func TestAssessHealthyUbuntu(t *testing.T) {
	r := api.SourceCheckReport{
		Hostname: "web01", OSID: "ubuntu", OSVersion: "24.04", OSPretty: "Ubuntu 24.04 LTS",
		Arch: "x86_64", Kernel: "6.8.0", HasSystemd: true,
		RootFS: "ext4", RootDevice: "/dev/sda1",
		Disks:     []api.SourceCheckDisk{{Name: "sda", SizeBytes: 80 << 30}},
		UsedBytes: 4 << 30, DataPortOK: boolp(true),
	}
	a := assessSource(r)
	for _, m := range a.Methods {
		if m.Verdict != "ok" {
			t.Errorf("method %s verdict %q (reasons %v), want ok", m.Method, m.Verdict, m.Reasons)
		}
	}
	if img := methodByName(t, a, "file").RecommendedImage; img != "linode/ubuntu24.04" {
		t.Errorf("file recommended image %q, want linode/ubuntu24.04", img)
	}
	if methodByName(t, a, "volume").RecommendedImage != "" {
		t.Error("block methods must not recommend an image (destination boots the migrated disk)")
	}
}

// Non-x86_64 sources cannot migrate with any method (the agent and the Linode
// target are x86_64-only) — every method must be a hard fail.
func TestAssessARMFailsAllMethods(t *testing.T) {
	r := api.SourceCheckReport{OSID: "ubuntu", OSVersion: "22.04", Arch: "aarch64", HasSystemd: true, RootFS: "ext4"}
	for _, m := range assessSource(r).Methods {
		if m.Verdict != "fail" {
			t.Errorf("method %s on aarch64: verdict %q, want fail", m.Method, m.Verdict)
		}
	}
}

// A LUKS-encrypted root cannot be converted to boot on Linode: the block
// methods must fail, but file transfer (which copies the mounted files) is ok.
func TestAssessLUKSRoot(t *testing.T) {
	r := api.SourceCheckReport{
		OSID: "debian", OSVersion: "12", Arch: "x86_64", HasSystemd: true,
		RootFS: "ext4", RootOnLUKS: true, DataPortOK: boolp(true),
	}
	a := assessSource(r)
	if v := methodByName(t, a, "volume").Verdict; v != "fail" {
		t.Errorf("volume on LUKS root: %q, want fail", v)
	}
	if v := methodByName(t, a, "disk").Verdict; v != "fail" {
		t.Errorf("disk on LUKS root: %q, want fail", v)
	}
	if v := methodByName(t, a, "file").Verdict; v == "fail" {
		t.Errorf("file on LUKS root must not fail (it copies mounted files), got %q", v)
	}
}

// ZFS root: no conversion/fsck path — block methods fail; btrfs: warn (boots
// are unvalidated); XFS and LVM-backed ext4 are fully supported.
func TestAssessRootFilesystems(t *testing.T) {
	base := api.SourceCheckReport{OSID: "fedora", OSVersion: "40", Arch: "x86_64", HasSystemd: true, DataPortOK: boolp(true)}
	fs := func(fstype string, lvm bool) api.SourceAssessment {
		r := base
		r.RootFS = fstype
		r.RootOnLVM = lvm
		return assessSource(r)
	}
	if v := methodByName(t, fs("zfs", false), "volume").Verdict; v != "fail" {
		t.Errorf("volume on zfs root: %q, want fail", v)
	}
	if v := methodByName(t, fs("btrfs", false), "volume").Verdict; v != "warn" {
		t.Errorf("volume on btrfs root: %q, want warn", v)
	}
	if v := methodByName(t, fs("xfs", false), "volume").Verdict; v != "ok" {
		t.Errorf("volume on xfs root: %q, want ok", v)
	}
	if v := methodByName(t, fs("ext4", true), "volume").Verdict; v != "ok" {
		t.Errorf("volume on LVM ext4 root: %q, want ok", v)
	}
}

// SELinux enforcing: the file method does not preserve contexts/xattrs, so it
// must WARN (with the block methods unaffected).
func TestAssessSELinuxEnforcing(t *testing.T) {
	r := api.SourceCheckReport{
		OSID: "almalinux", OSVersion: "9", Arch: "x86_64", HasSystemd: true,
		RootFS: "xfs", SELinux: "enforcing", DataPortOK: boolp(true),
	}
	a := assessSource(r)
	if v := methodByName(t, a, "file").Verdict; v != "warn" {
		t.Errorf("file with SELinux enforcing: %q, want warn", v)
	}
	if v := methodByName(t, a, "volume").Verdict; v != "ok" {
		t.Errorf("volume with SELinux enforcing: %q, want ok", v)
	}
}

// A blocked data port must warn on every method — replication cannot start
// until TCP 5000–5100 is reachable.
func TestAssessBlockedDataPort(t *testing.T) {
	r := api.SourceCheckReport{
		OSID: "ubuntu", OSVersion: "24.04", Arch: "x86_64", HasSystemd: true,
		RootFS: "ext4", DataPortOK: boolp(false), DataPortTried: 5100,
	}
	for _, m := range assessSource(r).Methods {
		if m.Verdict == "ok" {
			t.Errorf("method %s with blocked data port must not be ok", m.Method)
		}
	}
}

// Disks over the 10 TiB Block Storage limit: volume boot fails, disk boot
// warns (needs a plan with that much local disk), file transfer unaffected.
func TestAssessHugeDisk(t *testing.T) {
	r := api.SourceCheckReport{
		OSID: "ubuntu", OSVersion: "22.04", Arch: "x86_64", HasSystemd: true, RootFS: "ext4",
		Disks: []api.SourceCheckDisk{{Name: "sda", SizeBytes: 11 << 40}}, DataPortOK: boolp(true),
	}
	a := assessSource(r)
	if v := methodByName(t, a, "volume").Verdict; v != "fail" {
		t.Errorf("volume with 11TiB disk: %q, want fail", v)
	}
	if v := methodByName(t, a, "disk").Verdict; v != "warn" {
		t.Errorf("disk with 11TiB disk: %q, want warn", v)
	}
	if v := methodByName(t, a, "file").Verdict; v != "ok" {
		t.Errorf("file with 11TiB disk: %q, want ok", v)
	}
}

// Image recommendations map the source distro/version to the closest Linode
// image, with a sensible fallback when unknown.
func TestRecommendedImages(t *testing.T) {
	for _, tc := range []struct{ id, ver, want string }{
		{"ubuntu", "24.04", "linode/ubuntu24.04"},
		{"ubuntu", "22.04", "linode/ubuntu22.04"},
		{"debian", "12", "linode/debian12"},
		{"almalinux", "9", "linode/almalinux9"},
		{"rocky", "8", "linode/rocky8"},
		{"fedora", "40", "linode/fedora40"},
		{"centos", "7", "linode/centos7"},
		{"rhel", "9", "linode/almalinux9"},      // closest free rebuild
		{"sles", "15.5", "linode/opensuse15.6"}, // SLES → openSUSE Leap (shared codebase)
		{"amzn", "2023", "linode/almalinux9"},   // Amazon Linux is RHEL-family
		{"weirdos", "1", ""},                    // unknown → pick manually
	} {
		if got := recommendedImage(tc.id, tc.ver); got != tc.want {
			t.Errorf("recommendedImage(%s,%s) = %q, want %q", tc.id, tc.ver, got, tc.want)
		}
	}
}

// Azure's ephemeral resource disk (temporary storage mounted at /mnt) must not
// be block-migrated: the block methods warn about it (file transfer never
// copies /mnt), and an oversized EPHEMERAL disk must NOT trip the 10 TiB
// volume-size failure — it isn't part of the migration.
func TestAssessAzureEphemeralDisk(t *testing.T) {
	r := api.SourceCheckReport{
		OSID: "ubuntu", OSVersion: "20.04", Arch: "x86_64", HasSystemd: true,
		RootFS: "ext4", Virt: "microsoft",
		Disks: []api.SourceCheckDisk{
			{Name: "sda", SizeBytes: 30 << 30},
			{Name: "sdb", SizeBytes: 11 << 40, Ephemeral: true}, // huge resource disk
		},
		DataPortOK: boolp(true),
	}
	a := assessSource(r)
	for _, m := range []string{"volume", "disk"} {
		got := methodByName(t, a, m)
		if got.Verdict != "warn" {
			t.Errorf("%s with ephemeral disk: verdict %q, want warn", m, got.Verdict)
		}
		found := false
		for _, reason := range got.Reasons {
			if strings.Contains(reason, "resource disk") || strings.Contains(reason, "ephemeral") {
				found = true
			}
			if strings.Contains(reason, "10 TiB") {
				t.Errorf("%s: ephemeral disk must be excluded from the volume size limit (got %q)", m, reason)
			}
		}
		if !found {
			t.Errorf("%s: expected an ephemeral-disk caution, got %v", m, got.Reasons)
		}
	}
	if v := methodByName(t, a, "file").Verdict; v != "ok" {
		t.Errorf("file with ephemeral disk: %q, want ok (file never copies /mnt)", v)
	}
}

// Approximate image recommendations must carry an honest note: Amazon Linux
// has no Linode image (AlmaLinux is RHEL-family but NOT drop-in); RHEL maps to
// its binary-compatible rebuild; SLES maps to openSUSE. Exact matches carry no
// note.
func TestRecommendedImageNotes(t *testing.T) {
	for _, tc := range []struct {
		id      string
		wantSub string // substring of the note; "" = no note
	}{
		{"amzn", "not a drop-in"},
		{"rhel", "binary-compatible"},
		{"sles", "openSUSE"},
		{"ubuntu", ""},
		{"debian", ""},
	} {
		got := recommendedImageNote(tc.id)
		if tc.wantSub == "" && got != "" {
			t.Errorf("recommendedImageNote(%s) = %q, want empty", tc.id, got)
		}
		if tc.wantSub != "" && !strings.Contains(got, tc.wantSub) {
			t.Errorf("recommendedImageNote(%s) = %q, want substring %q", tc.id, got, tc.wantSub)
		}
	}
	// The note reaches the file method's assessment.
	a := assessSource(api.SourceCheckReport{OSID: "amzn", OSVersion: "2", Arch: "x86_64", HasSystemd: true, RootFS: "xfs", DataPortOK: boolp(true)})
	if n := methodByName(t, a, "file").RecommendedImageNote; !strings.Contains(n, "not a drop-in") {
		t.Errorf("file assessment note for amzn = %q", n)
	}
}

// No systemd → the agent's timer can't be installed automatically: warn on all
// methods (manual scheduling is possible but not turnkey).
func TestAssessNoSystemd(t *testing.T) {
	r := api.SourceCheckReport{OSID: "alpine", OSVersion: "3.20", Arch: "x86_64", RootFS: "ext4", DataPortOK: boolp(true)}
	for _, m := range assessSource(r).Methods {
		if m.Verdict == "ok" {
			t.Errorf("method %s without systemd must carry at least a warning", m.Method)
		}
	}
}

// The full HTTP round trip: mint a token (auth'd console call), serve the
// script (token-gated), accept the report, and expose the assessment to the
// console poll endpoint.
func TestSourceCheckRoundTrip(t *testing.T) {
	s := &Server{}
	s.cfg.PublicHost = "203.0.113.10"
	s.cfg.ConsolePort = 8080

	// Mint.
	rr := httptest.NewRecorder()
	s.handleSourceCheckCreate(rr, httptest.NewRequest("POST", "/api/v1/sourcecheck", nil))
	if rr.Code != 200 {
		t.Fatalf("create: %d", rr.Code)
	}
	var created struct {
		Token string `json:"token"`
		Cmd   string `json:"cmd"`
	}
	_ = json.Unmarshal(rr.Body.Bytes(), &created)
	if created.Token == "" || !strings.Contains(created.Cmd, created.Token) {
		t.Fatalf("create returned token=%q cmd=%q", created.Token, created.Cmd)
	}

	// Script served for a good token, refused otherwise.
	rr = httptest.NewRecorder()
	s.handleSourceCheckScript(rr, httptest.NewRequest("GET", "/check/source.sh?token="+created.Token, nil))
	if rr.Code != 200 || !strings.Contains(rr.Body.String(), "os-release") {
		t.Fatalf("script: %d", rr.Code)
	}
	rr = httptest.NewRecorder()
	s.handleSourceCheckScript(rr, httptest.NewRequest("GET", "/check/source.sh?token=bad", nil))
	if rr.Code != 403 {
		t.Fatalf("script with bad token: %d, want 403", rr.Code)
	}

	// Poll before the report: pending.
	rr = httptest.NewRecorder()
	s.handleSourceCheckStatus(rr, httptest.NewRequest("GET", "/api/v1/sourcecheck/"+created.Token, nil))
	var st api.SourceCheckStatus
	_ = json.Unmarshal(rr.Body.Bytes(), &st)
	if st.Status != "pending" {
		t.Fatalf("pre-report status %q, want pending", st.Status)
	}

	// Report.
	rep := api.SourceCheckReport{OSID: "ubuntu", OSVersion: "24.04", Arch: "x86_64", HasSystemd: true, RootFS: "ext4"}
	body, _ := json.Marshal(rep)
	rr = httptest.NewRecorder()
	s.handleSourceCheckReport(rr, httptest.NewRequest("POST", "/check/report?token="+created.Token, bytes.NewReader(body)))
	if rr.Code != 200 {
		t.Fatalf("report: %d", rr.Code)
	}

	// Poll after: done, with the assessment attached.
	rr = httptest.NewRecorder()
	s.handleSourceCheckStatus(rr, httptest.NewRequest("GET", "/api/v1/sourcecheck/"+created.Token, nil))
	_ = json.Unmarshal(rr.Body.Bytes(), &st)
	if st.Status != "done" || st.Assessment == nil || len(st.Assessment.Methods) != 3 {
		t.Fatalf("post-report status %+v", st)
	}
}

// The check script must be self-sufficient: it prints the FULL assessment in
// the source server's own terminal (facts, per-method verdicts, recommended
// image) BEFORE attempting to deliver the report — so the operator still gets
// the result when the network to the migration instance is not accessible —
// and on delivery failure it prints a prominent note saying exactly that.
func TestSourceCheckScriptPrintsLocalResult(t *testing.T) {
	for _, want := range []string{
		"SOURCE CHECK RESULT",              // the local result banner
		"recommended destination OS image", // image recommendation printed locally
		"NOT SUPPORTED",                    // local verdict labels exist
		"VERDICT:",                         // bottom-line verdict
		"could NOT be delivered",           // the offline note…
		"is not accessible",                // …says the network to the instance is blocked
		"printed above is complete",        // …and that the local result still stands
	} {
		if !strings.Contains(sourceCheckScript, want) {
			t.Errorf("check script missing local-result piece %q", want)
		}
	}
	// The local verdicts must be computed BEFORE the report POST, so they print
	// even when the console is unreachable.
	res := strings.Index(sourceCheckScript, "SOURCE CHECK RESULT")
	post := strings.Index(sourceCheckScript, "/check/report?token=")
	if res < 0 || post < 0 || res > post {
		t.Error("the local result must print before the report is delivered")
	}
}

// The console must carry the Source check tab, its view, and the check flow.
func TestConsoleSourceCheckTab(t *testing.T) {
	for _, want := range []string{
		`id="tabSrc"`, `id="view-src"`, "Source check",
		"runSourceCheck", "/api/v1/sourcecheck",
	} {
		if !strings.Contains(consoleHTML, want) {
			t.Errorf("console missing source-check piece %q", want)
		}
	}
}

func methodByName(t *testing.T, a api.SourceAssessment, name string) api.MethodAssessment {
	t.Helper()
	for _, m := range a.Methods {
		if m.Method == name {
			return m
		}
	}
	t.Fatalf("assessment has no method %q", name)
	return api.MethodAssessment{}
}
