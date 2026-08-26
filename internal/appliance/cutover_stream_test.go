package appliance

import (
	"bytes"
	"fmt"
	"io"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// The disk-boot cutover streams the converted image from the appliance's own
// (fully hydrated) replication volume straight into the rescue'd destination
// over a token-gated one-time URL — replacing the old clone-volume + in-guest
// one-shot dance (which read a fresh clone slowly and depended on the migrated
// OS booting). These tests pin the token registry, the served copy script, and
// the HTTP handlers.

func TestCutoverStreamTokens(t *testing.T) {
	s := &Server{}

	tok := s.registerCutoverStream(7, "/dev/some-volume", 1<<30, time.Hour)
	if len(tok) < 32 {
		t.Fatalf("token %q too short to be unguessable", tok)
	}
	st, ok := s.lookupCutoverStream(tok)
	if !ok || st.path != "/dev/some-volume" || st.bytes != 1<<30 || st.migID != 7 {
		t.Fatalf("lookup = %+v, %v; want the registered stream", st, ok)
	}
	if _, ok := s.lookupCutoverStream("no-such-token"); ok {
		t.Error("unknown token must not resolve")
	}

	// The console command survives until the cutover ends.
	s.setCutoverCopyCmd(7, "curl … | sh")
	if got := s.cutoverCopyCmdFor(7); got != "curl … | sh" {
		t.Errorf("cutoverCopyCmdFor = %q", got)
	}
	if got := s.cutoverCopyCmdFor(8); got != "" {
		t.Errorf("other migration must have no command, got %q", got)
	}

	// Dropping the migration invalidates both the token and the command.
	s.dropCutoverStream(7)
	if _, ok := s.lookupCutoverStream(tok); ok {
		t.Error("token must be invalid after dropCutoverStream")
	}
	if got := s.cutoverCopyCmdFor(7); got != "" {
		t.Errorf("command must be gone after dropCutoverStream, got %q", got)
	}

	// Expired tokens must not resolve.
	tok = s.registerCutoverStream(9, "/dev/x", 1, -time.Second)
	if _, ok := s.lookupCutoverStream(tok); ok {
		t.Error("expired token must not resolve")
	}
}

func TestRescueCopyScriptAndCmd(t *testing.T) {
	s := &Server{}
	s.cfg.PublicHost = "203.0.113.10"
	s.cfg.ConsolePort = 8080
	s.cfg.PublicKeyPin = "PINPINPIN"

	cmd := s.cutoverCopyCmd("tok123")
	for _, want := range []string{"/cutover/copy.sh?token=tok123", "| sh", "sha256//PINPINPIN", "https://203.0.113.10:8080"} {
		if !strings.Contains(cmd, want) {
			t.Errorf("copy command %q should contain %q", cmd, want)
		}
	}

	script := s.rescueCopyScript("tok123", 5<<30)
	for _, want := range []string{
		"/cutover/image?token=tok123", // streams the image from the appliance
		"dd of=$TARGET",               // onto the disk select_target_disk chose
		"status=progress",             // live progress in the Lish session
		"conv=fsync",
		"resize2fs",                   // grow the (whole-disk ext) root to fill the local disk
		"poweroff",                    // the "copy done" signal the appliance waits for
		"select_target_disk",          // F-23: identify the disk by properties, not by name
		"TARGET=$(select_target_disk", // capture its choice before streaming
		"|| exit 1",                   // an ambiguous/no-match selection must abort, not guess
		"sha256//PINPINPIN",
	} {
		if !strings.Contains(script, want) {
			t.Errorf("copy script should contain %q; script:\n%s", want, script)
		}
	}
	// F-23: the fixed migration must never hardcode a device node — the whole
	// point is that Finnix does not reliably put the local disk at /dev/sda.
	if strings.Contains(script, "of=/dev/sda") {
		t.Error("copy script must not hardcode dd of=/dev/sda (F-23: Finnix does not reliably enumerate the local disk there)")
	}
	// dd must be fed by the download — never run against an empty stdin.
	if !strings.Contains(script, "| dd of=$TARGET") {
		t.Error("the image download must pipe straight into dd")
	}
}

// TestSelectTargetDisk exercises the select_target_disk shell function
// (embedded verbatim in the rescue copy script as selectTargetDiskFunc)
// standalone, against a fake `lsblk`, covering the F-23 selection rules:
// whole disk, writable, size within tolerance of the expected byte count —
// and that ambiguous or empty results fail loudly instead of guessing.
// The destination's local disk is created at the PLAN's size, while the image
// streamed onto it is the SOURCE disk's size. Those are deliberately different
// — a 24.5 GiB source image is written onto a 25 GiB nanode disk — so the disk
// is normally LARGER than the image and a near-equality match rejects it.
//
// Measured live: the selector found zero candidates and refused to proceed,
// listing "/dev/sda size=26826768384 ro=0" against an expected 26289897472 —
// a 512 MiB gap, far outside any sane tolerance. The correct rule is that the
// disk must be big enough to HOLD the image, not the same size as it.
func TestSelectTargetDiskAcceptsADiskLargerThanTheImage(t *testing.T) {
	// Exactly the live case that failed.
	const image = 26289897472 // 24.5 GiB source image
	out, stderr, err := runSelectTargetDisk(t,
		"loop0 511930368 1\nsda 26826768384 0\nsdb 0 1\nsdc 0 1\nsr0 660602880 0\nzram0 507510784 0\n",
		image)
	if err != nil {
		t.Fatalf("a 25 GiB writable disk must be accepted for a 24.5 GiB image; got error. stderr:\n%s", stderr)
	}
	if got := strings.TrimSpace(out); got != "/dev/sda" {
		t.Errorf("selected %q, want /dev/sda", got)
	}
}

// A disk SMALLER than the image can never hold it, so it must never be chosen.
func TestSelectTargetDiskRejectsADiskTooSmall(t *testing.T) {
	const image = 26289897472
	_, stderr, err := runSelectTargetDisk(t, "sda 10737418240 0\n", image)
	if err == nil {
		t.Error("a disk smaller than the image must be rejected, not written to")
	}
	if !strings.Contains(stderr, "26289897472") {
		t.Errorf("the failure must state the size it needed; got:\n%s", stderr)
	}
}

func TestSelectTargetDisk(t *testing.T) {
	const expect = 26843545600 // bytes; arbitrary "the image is this big"

	tests := []struct {
		name       string
		lsblk      string // fake `lsblk -bdno NAME,SIZE,RO` output
		expect     int64
		wantStdout string // exact stdout on success ("" when a failure is expected)
		wantErr    bool
		wantStderr []string // substrings that must appear in stderr on failure
	}{
		{
			name: "single writable whole disk of the right size wins",
			lsblk: "sda 0 1\n" + // unused rescue slot: zero-byte, read-only
				"sdb 0 1\n" + // another unused slot
				fmt.Sprintf("sdc %d 0\n", expect) + // the actual target
				"sr0 0 1\n" + // optical drive
				"zram0 4294967296 0", // zram: writable, but excluded by name
			expect:     expect,
			wantStdout: "/dev/sdc",
		},
		{
			// The normal shape: the plan's disk is bigger than the source image.
			name:       "a disk comfortably larger than the image matches",
			lsblk:      fmt.Sprintf("sdg %d 0", expect+536870912), // 512 MiB larger
			expect:     expect,
			wantStdout: "/dev/sdg",
		},
		{
			name:       "a disk too small to hold the image is rejected",
			lsblk:      fmt.Sprintf("sdg %d 0", expect-1), // one byte short
			expect:     expect,
			wantErr:    true,
			wantStderr: []string{"found 0", "/dev/sdg"},
		},
		{
			name: "read-only devices are excluded, so no match",
			lsblk: fmt.Sprintf("sda %d 1\n", expect) + // right size, but RO — the exact F-23 symptom
				fmt.Sprintf("sdg %d 1", expect),
			expect:     expect,
			wantErr:    true,
			wantStderr: []string{"found 0", "/dev/sda", "/dev/sdg", "ro=1"},
		},
		{
			name: "two writable disks of the right size is ambiguous",
			lsblk: fmt.Sprintf("sdb %d 0\n", expect) +
				fmt.Sprintf("sdc %d 0", expect),
			expect:     expect,
			wantErr:    true,
			wantStderr: []string{"found 2", "/dev/sdb", "/dev/sdc"},
		},
		{
			name:       "no disks at all",
			lsblk:      "",
			expect:     expect,
			wantErr:    true,
			wantStderr: []string{"found 0"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			stdout, stderr, err := runSelectTargetDisk(t, tc.lsblk, tc.expect)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("want a failure, got success with stdout %q", stdout)
				}
				for _, want := range tc.wantStderr {
					if !strings.Contains(stderr, want) {
						t.Errorf("stderr should contain %q; stderr:\n%s", want, stderr)
					}
				}
				if stdout != "" {
					t.Errorf("stdout on failure = %q, want empty (must never guess)", stdout)
				}
				return
			}
			if err != nil {
				t.Fatalf("select_target_disk failed: %v; stderr:\n%s", err, stderr)
			}
			if stdout != tc.wantStdout {
				t.Errorf("stdout = %q, want %q", stdout, tc.wantStdout)
			}
		})
	}
}

// runSelectTargetDisk runs the real select_target_disk shell function (lifted
// verbatim from selectTargetDiskFunc, the exact text embedded in the rescue
// copy script) under /bin/sh, backed by a fake `lsblk` that prints lsblkOut
// regardless of arguments. It returns stdout, stderr, and the run error (nil
// on exit 0).
func runSelectTargetDisk(t *testing.T, lsblkOut string, expect int64) (stdout, stderr string, err error) {
	t.Helper()
	dir := t.TempDir()

	// Fake lsblk: select_target_disk always invokes it as
	// "lsblk -bdno NAME,SIZE,RO" and reads its stdout; the fake ignores the
	// arguments and just prints the canned listing for this case.
	fakeLsblk := "#!/bin/sh\ncat <<'LSBLK_EOF'\n" + lsblkOut + "\nLSBLK_EOF\n"
	if werr := os.WriteFile(filepath.Join(dir, "lsblk"), []byte(fakeLsblk), 0o755); werr != nil {
		t.Fatalf("write fake lsblk: %v", werr)
	}

	// Driver script: the function under test plus one call to it with the
	// argument under test — this is exactly what rescueCopyScript's
	// "TARGET=$(select_target_disk %d)" does.
	driver := "#!/bin/sh\nset -e\n" + selectTargetDiskFunc +
		fmt.Sprintf("\nselect_target_disk %d\n", expect)
	driverPath := filepath.Join(dir, "select.sh")
	if werr := os.WriteFile(driverPath, []byte(driver), 0o755); werr != nil {
		t.Fatalf("write driver script: %v", werr)
	}

	cmd := exec.Command("sh", driverPath)
	cmd.Env = append(os.Environ(), "PATH="+dir+":"+os.Getenv("PATH"))
	var outBuf, errBuf bytes.Buffer
	cmd.Stdout = &outBuf
	cmd.Stderr = &errBuf
	err = cmd.Run()
	return strings.TrimRight(outBuf.String(), "\n"), errBuf.String(), err
}

func TestCutoverHandlers(t *testing.T) {
	s := &Server{}
	s.cfg.PublicHost = "h"
	s.cfg.ConsolePort = 8080

	// Back the "device" with a regular file (the file-fallback path does the same).
	img := filepath.Join(t.TempDir(), "disk.img")
	content := []byte("BOOTSECTOR-then-filesystem-bytes")
	if err := os.WriteFile(img, content, 0o600); err != nil {
		t.Fatal(err)
	}
	tok := s.registerCutoverStream(1, img, int64(len(content)), time.Hour)

	// Image: bad token → 403, nothing streamed.
	rr := httptest.NewRecorder()
	s.handleCutoverImage(rr, httptest.NewRequest("GET", "/cutover/image?token=WRONG", nil))
	if rr.Code != 403 {
		t.Fatalf("bad token: status %d, want 403", rr.Code)
	}
	// Image: good token → exact bytes with Content-Length.
	rr = httptest.NewRecorder()
	s.handleCutoverImage(rr, httptest.NewRequest("GET", "/cutover/image?token="+tok, nil))
	if rr.Code != 200 {
		t.Fatalf("good token: status %d, want 200", rr.Code)
	}
	if got, _ := io.ReadAll(rr.Body); string(got) != string(content) {
		t.Errorf("streamed %d bytes, want the exact image content", len(got))
	}
	if cl := rr.Header().Get("Content-Length"); cl != strconv.Itoa(len(content)) {
		t.Errorf("Content-Length = %q, want %d", cl, len(content))
	}

	// Script: good token → the copy script; bad token → 403.
	rr = httptest.NewRecorder()
	s.handleCutoverScript(rr, httptest.NewRequest("GET", "/cutover/copy.sh?token="+tok, nil))
	if rr.Code != 200 || !strings.Contains(rr.Body.String(), "dd of=$TARGET") {
		t.Fatalf("script: status %d, body should be the copy script", rr.Code)
	}
	rr = httptest.NewRecorder()
	s.handleCutoverScript(rr, httptest.NewRequest("GET", "/cutover/copy.sh?token=WRONG", nil))
	if rr.Code != 403 {
		t.Fatalf("script bad token: status %d, want 403", rr.Code)
	}
}

// F-22, appliance side. machine-convert.sh MOUNTS the replication volume and
// rewrites it heavily — chroot, GRUB, fstab, stripping the source's agent —
// and then this handler streams that same device to the destination with a
// plain buffered read.
//
// Reads of a block device come from the kernel's page cache for that device,
// which can hold pages predating the convert's writes. Streaming those would
// ship a boot image missing the very changes that make it bootable, and the
// copy would report success. The agent-side read path had exactly this defect
// (F-22: a file arrived with its final chunk stale, and the stale hash was
// then recorded so no later pass ever corrected it).
//
// So the handler must invalidate the device's cached pages before streaming.
func TestCutoverImageStreamInvalidatesPageCacheFirst(t *testing.T) {
	src, err := os.ReadFile("cutover_stream.go")
	if err != nil {
		t.Fatal(err)
	}
	s := string(src)
	i := strings.Index(s, "func (s *Server) handleCutoverImage")
	if i < 0 {
		t.Fatal("handleCutoverImage not found")
	}
	body := s[i:]
	if end := strings.Index(body, "\nfunc "); end > 0 {
		body = body[:end]
	}
	if !strings.Contains(body, "InvalidatePageCache") {
		t.Error("handleCutoverImage streams the replication volume with a buffered read but never drops the device's cached pages — a stale page here ships a boot image missing the convert's changes")
	}
	// It must not be fatal: a device that refuses the ioctl (or a plain file
	// image in the fallback path) must still stream rather than fail cutover.
	if strings.Contains(body, "InvalidatePageCache") &&
		strings.Contains(body, "writeErr(w, http.StatusInternalServerError, \"cannot invalidate") {
		t.Error("a failed cache invalidation must be logged and streamed anyway, not turned into a cutover failure")
	}
}
