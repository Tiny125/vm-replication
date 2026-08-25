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
func TestSelectTargetDisk(t *testing.T) {
	const expect = 26843545600 // bytes; arbitrary "the image is this big"
	const tol = 2097152        // 2 MiB, matching rescueCopyScript's tolerance

	tests := []struct {
		name       string
		lsblk      string // fake `lsblk -bdno NAME,SIZE,RO` output
		expect     int64
		tol        int64
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
			expect: expect, tol: tol,
			wantStdout: "/dev/sdc",
		},
		{
			name:   "within-tolerance rounding still matches",
			lsblk:  fmt.Sprintf("sdg %d 0", expect+1048576), // 1 MiB over, still <= 2 MiB tolerance
			expect: expect, tol: tol,
			wantStdout: "/dev/sdg",
		},
		{
			name:   "outside tolerance is not a match",
			lsblk:  fmt.Sprintf("sdg %d 0", expect+3145728), // 3 MiB over > 2 MiB tolerance
			expect: expect, tol: tol,
			wantErr:    true,
			wantStderr: []string{"found 0", "/dev/sdg"},
		},
		{
			name: "read-only devices are excluded, so no match",
			lsblk: fmt.Sprintf("sda %d 1\n", expect) + // right size, but RO — the exact F-23 symptom
				fmt.Sprintf("sdg %d 1", expect),
			expect: expect, tol: tol,
			wantErr:    true,
			wantStderr: []string{"found 0", "/dev/sda", "/dev/sdg", "ro=1"},
		},
		{
			name: "two writable disks of the right size is ambiguous",
			lsblk: fmt.Sprintf("sdb %d 0\n", expect) +
				fmt.Sprintf("sdc %d 0", expect),
			expect: expect, tol: tol,
			wantErr:    true,
			wantStderr: []string{"found 2", "/dev/sdb", "/dev/sdc"},
		},
		{
			name:   "no disks at all",
			lsblk:  "",
			expect: expect, tol: tol,
			wantErr:    true,
			wantStderr: []string{"found 0"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			stdout, stderr, err := runSelectTargetDisk(t, tc.lsblk, tc.expect, tc.tol)
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
func runSelectTargetDisk(t *testing.T, lsblkOut string, expect, tol int64) (stdout, stderr string, err error) {
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
	// arguments under test — this is exactly what rescueCopyScript's
	// "TARGET=$(select_target_disk %d %d)" does.
	driver := "#!/bin/sh\nset -e\n" + selectTargetDiskFunc +
		fmt.Sprintf("\nselect_target_disk %d %d\n", expect, tol)
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
