package appliance

import (
	"archive/tar"
	"io"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The file-cutover delivery serves the staged tree as a token-gated tar and a
// completion ping; the pasted command must pull the tar, extract to /, ping
// done, and reboot. Bad tokens are refused.
func TestFileDelivery(t *testing.T) {
	s := &Server{}
	s.cfg.PublicHost = "203.0.113.10"
	s.cfg.ConsolePort = 8080
	s.cfg.PublicKeyPin = "PINPIN"

	// Stage a small tree.
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "etc"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "etc", "app.conf"), []byte("k=v\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	tok, done := s.registerFileDelivery(7, root, time.Hour)

	// The paste command references both endpoints with the token, pins the key,
	// extracts to /, and reboots.
	cmd := s.fileCopyCmd(tok)
	for _, want := range []string{"/cutover/files.tar?token=" + tok, "/cutover/done?token=" + tok, "tar -xp", "-C /", "reboot", "sha256//PINPIN"} {
		if !strings.Contains(cmd, want) {
			t.Errorf("copy command missing %q; got %s", want, cmd)
		}
	}

	// Bad token -> 403 on both endpoints.
	rr := httptest.NewRecorder()
	s.handleCutoverTar(rr, httptest.NewRequest("GET", "/cutover/files.tar?token=NOPE", nil))
	if rr.Code != 403 {
		t.Fatalf("tar bad token: %d, want 403", rr.Code)
	}
	rr = httptest.NewRecorder()
	s.handleCutoverDone(rr, httptest.NewRequest("GET", "/cutover/done?token=NOPE", nil))
	if rr.Code != 403 {
		t.Fatalf("done bad token: %d, want 403", rr.Code)
	}

	// Good token -> a tar containing the staged file with its content.
	rr = httptest.NewRecorder()
	s.handleCutoverTar(rr, httptest.NewRequest("GET", "/cutover/files.tar?token="+tok, nil))
	if rr.Code != 200 {
		t.Fatalf("tar: %d, want 200", rr.Code)
	}
	names := map[string]string{}
	tr := tar.NewReader(rr.Body)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("tar read: %v", err)
		}
		b, _ := io.ReadAll(tr)
		names[h.Name] = string(b)
		if strings.HasPrefix(h.Name, "/") {
			t.Errorf("tar entry %q must be relative (no leading slash)", h.Name)
		}
	}
	if names["etc/app.conf"] != "k=v\n" {
		t.Errorf("tar missing the staged file content: %v", names)
	}

	// The done ping closes the wait channel.
	rr = httptest.NewRecorder()
	s.handleCutoverDone(rr, httptest.NewRequest("GET", "/cutover/done?token="+tok, nil))
	if rr.Code != 200 {
		t.Fatalf("done: %d, want 200", rr.Code)
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Error("done ping must close the wait channel")
	}
}
