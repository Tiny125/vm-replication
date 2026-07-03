package appliance

import (
	"io"
	"net/http/httptest"
	"os"
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
		"dd of=/dev/sda",              // onto the rescue'd instance's local disk
		"status=progress",             // live progress in the Lish session
		"conv=fsync",
		"resize2fs",  // grow the (whole-disk ext) root to fill the local disk
		"poweroff",   // the "copy done" signal the appliance waits for
		"/dev/sda ]", // sanity check the target disk exists before writing
		"sha256//PINPINPIN",
	} {
		if !strings.Contains(script, want) {
			t.Errorf("copy script should contain %q; script:\n%s", want, script)
		}
	}
	// dd must be fed by the download — never run against an empty stdin.
	if !strings.Contains(script, "| dd of=/dev/sda") {
		t.Error("the image download must pipe straight into dd")
	}
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
	if rr.Code != 200 || !strings.Contains(rr.Body.String(), "dd of=/dev/sda") {
		t.Fatalf("script: status %d, body should be the copy script", rr.Code)
	}
	rr = httptest.NewRecorder()
	s.handleCutoverScript(rr, httptest.NewRequest("GET", "/cutover/copy.sh?token=WRONG", nil))
	if rr.Code != 403 {
		t.Fatalf("script bad token: status %d, want 403", rr.Code)
	}
}
