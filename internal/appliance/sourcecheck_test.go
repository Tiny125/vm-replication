package appliance

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/tiny125/vm-replication/internal/api"
)

func boolp(b bool) *bool { return &b }

// A healthy Ubuntu source must pass both remaining methods (volume and disk
// boot).
func TestAssessHealthyUbuntu(t *testing.T) {
	r := api.SourceCheckReport{
		Hostname: "web01", OSID: "ubuntu", OSVersion: "24.04", OSPretty: "Ubuntu 24.04 LTS",
		Arch: "x86_64", Kernel: "6.8.0", HasSystemd: true,
		RootFS: "ext4", RootDevice: "/dev/sda1",
		Disks:     []api.SourceCheckDisk{{Name: "sda", SizeBytes: 80 << 30}},
		UsedBytes: 4 << 30, DataPortOK: boolp(true),
	}
	a := assessSource(r)
	if len(a.Methods) != 2 {
		t.Fatalf("assessment has %d methods, want 2 (volume, disk) now that file transfer is removed", len(a.Methods))
	}
	for _, m := range a.Methods {
		if m.Verdict != "ok" {
			t.Errorf("method %s verdict %q (reasons %v), want ok", m.Method, m.Verdict, m.Reasons)
		}
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

// A LUKS-encrypted root cannot be converted to boot on Linode: both remaining
// (block) methods must fail.
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
// warns (needs a plan with that much local disk).
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
}

// Azure's ephemeral resource disk (temporary storage mounted at /mnt) must not
// be block-migrated: the block methods warn about it, and an oversized
// EPHEMERAL disk must NOT trip the 10 TiB volume-size failure — it isn't part
// of the migration.
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
	if st.Status != "done" || st.Assessment == nil || len(st.Assessment.Methods) != 2 {
		t.Fatalf("post-report status %+v", st)
	}
}

// The check script must be self-sufficient: it prints the FULL assessment in
// the source server's own terminal (facts, per-method verdicts) BEFORE
// attempting to deliver the report — so the operator still gets the result
// when the network to the migration instance is not accessible — and on
// delivery failure it prints a prominent note saying exactly that.
func TestSourceCheckScriptPrintsLocalResult(t *testing.T) {
	for _, want := range []string{
		"SOURCE CHECK RESULT",       // the local result banner
		"NOT SUPPORTED",             // local verdict labels exist
		"VERDICT:",                  // bottom-line verdict
		"could NOT be delivered",    // the offline note…
		"is not accessible",         // …says the network to the instance is blocked
		"printed above is complete", // …and that the local result still stands
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

// The source check prints its result to the terminal AND posts it back so the
// console's Source check tab can show it. That delivery used to interpolate the
// whole pre-quoted pin flag into a shell variable:
//
//	PIN="-k --pinnedpubkey 'sha256//AAAA…' "
//	CURL="curl -fsSL"
//	[ -n "$PIN" ] && CURL="$CURL $PIN"
//
// Shell quote removal does not apply to text arriving through a variable, so
// curl received --pinnedpubkey with LITERAL single quotes around the value and
// rejected every pin: "SSL: public key does not match pinned public key". The
// delivery always failed, the console waited forever, and the script blamed the
// operator's network — telling them to open firewall ports that were never shut.
//
// The enrollment script has always done this correctly: pass the BARE pin and
// build the flag literally in the script. This asserts the source check matches.
func TestSourceCheckScriptBuildsPinFlagFromBareValue(t *testing.T) {
	// The delivery used to interpolate the whole PRE-QUOTED pin flag into a shell
	// variable and then word-split it:
	//
	//	PIN="-k --pinnedpubkey 'sha256//AAAA…' "
	//	CURL="curl -fsSL"
	//	[ -n "$PIN" ] && CURL="$CURL $PIN"
	//
	// Shell quote removal does not apply to text arriving through a variable, so
	// curl received --pinnedpubkey with LITERAL single quotes around the value and
	// rejected every pin ("SSL: public key does not match pinned public key").
	// Delivery therefore ALWAYS failed against the self-signed console cert: the
	// Source check tab waited forever, and the script blamed the operator's
	// network, telling them to open firewall ports that were never shut.
	//
	// The enrollment script has always done this correctly — pass the BARE pin and
	// build the flag literally. This pins the source check to the same shape.
	if !strings.Contains(sourceCheckScript, `CURL="$CURL -k --pinnedpubkey sha256//$PIN"`) {
		t.Error("the delivery must build the pin flag literally from the bare pin value")
	}
	if strings.Contains(sourceCheckScript, `CURL="$CURL $PIN"`) {
		t.Error("interpolating a pre-quoted pin flag puts literal quotes in curl's argv; curl then rejects every pin")
	}
	// -k must survive: the console certificate is self-signed by design.
	if !strings.Contains(sourceCheckScript, "-k --pinnedpubkey") {
		t.Error("the delivery needs -k: the console's certificate is self-signed")
	}
}

// F-31: convertibleRootFS only judges the ROOT FILESYSTEM, so an AWS-style
// partitioned source got a clean bill of health by construction — EFIBoot was
// already collected and never used. assessSource must warn clearly for the
// cases that land on linode/latest-64bit (Linode's own kernel instead of the
// source's) after migration: a partitioned disk with no BIOS/MBR bootloader,
// UEFI-only sources with no BIOS/MBR fallback, and non-GRUB bootloaders.
func TestAssessPartitionedNoMBRBootloaderWarns(t *testing.T) {
	r := api.SourceCheckReport{
		OSID: "ubuntu", OSVersion: "24.04", Arch: "x86_64", HasSystemd: true,
		RootFS: "ext4", DataPortOK: boolp(true),
		PartTable: "gpt", MBRBootloader: false, BootloaderKind: "grub",
	}
	a := assessSource(r)
	for _, name := range []string{"volume", "disk"} {
		m := methodByName(t, a, name)
		if m.Verdict == "ok" {
			t.Errorf("method %s: partitioned + no MBR bootloader must not be ok, got verdict %q", name, m.Verdict)
		}
		found := false
		for _, reason := range m.Reasons {
			if strings.Contains(reason, "F-31") {
				found = true
			}
		}
		if !found {
			t.Errorf("method %s: expected an F-31 reason among %v", name, m.Reasons)
		}
	}
}

// The MBR-bootloader-present case is the F-31 FIX (linode/direct-disk) — a
// partitioned disk with a BIOS bootloader must NOT be warned about on this
// basis alone.
func TestAssessPartitionedWithMBRBootloaderOK(t *testing.T) {
	r := api.SourceCheckReport{
		OSID: "ubuntu", OSVersion: "24.04", Arch: "x86_64", HasSystemd: true,
		RootFS: "ext4", DataPortOK: boolp(true),
		Disks:     []api.SourceCheckDisk{{Name: "sda", SizeBytes: 80 << 30}},
		PartTable: "gpt", MBRBootloader: true, BootloaderKind: "grub",
	}
	a := assessSource(r)
	for _, name := range []string{"volume", "disk"} {
		if v := methodByName(t, a, name).Verdict; v != "ok" {
			t.Errorf("method %s: partitioned WITH an MBR bootloader must be ok, got %q (reasons: %v)", name, v, methodByName(t, a, name).Reasons)
		}
	}
}

// A partitionless source (PartTable unset/"none") must never trigger the
// partitioned-no-MBR-bootloader warning, regardless of MBRBootloader.
func TestAssessPartitionlessNeverWarnsOnMBRBootloader(t *testing.T) {
	for _, pt := range []string{"", "none"} {
		r := api.SourceCheckReport{
			OSID: "ubuntu", OSVersion: "24.04", Arch: "x86_64", HasSystemd: true,
			RootFS: "ext4", DataPortOK: boolp(true),
			PartTable: pt, MBRBootloader: false,
		}
		a := assessSource(r)
		for _, name := range []string{"volume", "disk"} {
			m := methodByName(t, a, name)
			for _, reason := range m.Reasons {
				if strings.Contains(reason, "F-31") {
					t.Errorf("part_table=%q method %s: unexpected F-31 warning for a partitionless source: %v", pt, name, m.Reasons)
				}
			}
		}
	}
}

// EFI-only boot with no BIOS/MBR fallback must warn even when PartTable
// itself is not reported (a source-check script that could read
// /sys/firmware/efi but not run sgdisk, say).
func TestAssessEFIOnlyNoMBRFallbackWarns(t *testing.T) {
	r := api.SourceCheckReport{
		OSID: "ubuntu", OSVersion: "24.04", Arch: "x86_64", HasSystemd: true,
		RootFS: "ext4", DataPortOK: boolp(true),
		EFIBoot: true, MBRBootloader: false,
	}
	a := assessSource(r)
	m := methodByName(t, a, "disk")
	if m.Verdict == "fail" {
		t.Errorf("EFI-only with no MBR fallback should warn, not fail: %v", m.Reasons)
	}
	if m.Verdict != "warn" {
		t.Errorf("EFI-only with no MBR fallback must warn, got %q", m.Verdict)
	}
	found := false
	for _, reason := range m.Reasons {
		if strings.Contains(strings.ToUpper(reason), "UEFI") {
			found = true
		}
	}
	if !found {
		t.Errorf("expected a UEFI-specific reason among %v", m.Reasons)
	}
}

// A non-GRUB bootloader must warn: the conversion only regenerates GRUB
// configs, so a syslinux/lilo/other source is unverified.
func TestAssessNonGRUBBootloaderWarns(t *testing.T) {
	r := api.SourceCheckReport{
		OSID: "debian", OSVersion: "12", Arch: "x86_64", HasSystemd: true,
		RootFS: "ext4", DataPortOK: boolp(true),
		PartTable: "dos", MBRBootloader: true, BootloaderKind: "syslinux",
	}
	a := assessSource(r)
	m := methodByName(t, a, "disk")
	if m.Verdict != "warn" {
		t.Errorf("non-GRUB bootloader must warn, got %q (reasons %v)", m.Verdict, m.Reasons)
	}
	found := false
	for _, reason := range m.Reasons {
		if strings.Contains(strings.ToLower(reason), "non-grub") || strings.Contains(reason, "syslinux") {
			found = true
		}
	}
	if !found {
		t.Errorf("expected a non-GRUB-bootloader reason among %v", m.Reasons)
	}
}

// A healthy BIOS/MBR Ubuntu source must stay fully "ok" -- the new fields'
// zero values (as in TestAssessHealthyUbuntu, which sets none of them) must
// never introduce a false-positive warning.
func TestAssessHealthyUbuntuStaysOKWithNewFieldsUnset(t *testing.T) {
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
			t.Errorf("method %s verdict %q (reasons %v), want ok (new F-31 fields left at zero value must not warn)", m.Method, m.Verdict, m.Reasons)
		}
	}
}

// The rendered source-check script must be syntactically valid bash. Guards
// against exactly the class of mistake that is easy to make when editing a
// large embedded shell script inside a Go raw string (e.g. a stray backtick
// in a comment terminating the Go string early, or a shell syntax slip in
// the F-31 boot-fact gathering added to it).
func TestSourceCheckScriptIsValidBash(t *testing.T) {
	rendered := fmt.Sprintf(sourceCheckScript, "https://example.invalid:8443", "tok123", "203.0.113.5", 5100, "")
	f, err := os.CreateTemp(t.TempDir(), "sourcecheck-*.sh")
	if err != nil {
		t.Fatalf("create temp file: %v", err)
	}
	if _, err := f.WriteString(rendered); err != nil {
		t.Fatalf("write script: %v", err)
	}
	f.Close()
	cmd := exec.Command("bash", "-n", f.Name())
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("rendered source-check script has a bash syntax error: %v\n%s", err, out)
	}
}
