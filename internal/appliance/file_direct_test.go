package appliance

import (
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The destination bootstrap must: mint a token, build a cloud-init script that
// downloads the receiver + certs and runs the receiver writing to / on the
// fixed port, and serve those downloads only for a valid token.
func TestDestBootstrap(t *testing.T) {
	s := &Server{}
	s.cfg.PublicHost = "203.0.113.10"
	s.cfg.ConsolePort = 8080
	s.cfg.PublicKeyPin = "PINPIN"

	tok := s.registerDestBootstrap(9, time.Hour)
	if _, ok := s.lookupDestBootstrap(tok); !ok {
		t.Fatal("freshly minted token should resolve")
	}
	if _, ok := s.lookupDestBootstrap("nope"); ok {
		t.Error("unknown token must not resolve")
	}

	ci := s.destCloudInit(tok)
	for _, want := range []string{
		"/dest/receiver?token=" + tok,
		"/dest/cert?token=" + tok,
		"-mode file", "-device /", ":5999",
		"vmrepl-receiver.service", "sha256//PINPIN",
	} {
		if !strings.Contains(ci, want) {
			t.Errorf("cloud-init missing %q", want)
		}
	}

	// Endpoints: bad token -> 403.
	rr := httptest.NewRecorder()
	s.handleDestReceiver(rr, httptest.NewRequest("GET", "/dest/receiver?token=nope", nil))
	if rr.Code != 403 {
		t.Errorf("receiver bad token: %d, want 403", rr.Code)
	}
	rr = httptest.NewRecorder()
	s.handleDestCert(rr, httptest.NewRequest("GET", "/dest/cert?token=nope&name=ca.crt", nil))
	if rr.Code != 403 {
		t.Errorf("cert bad token: %d, want 403", rr.Code)
	}

	// Good token: cert served from the configured file.
	dir := t.TempDir()
	ca := filepath.Join(dir, "ca.crt")
	if err := os.WriteFile(ca, []byte("CADATA"), 0o600); err != nil {
		t.Fatal(err)
	}
	s.cfg.TLS.CAFile = ca
	rr = httptest.NewRecorder()
	s.handleDestCert(rr, httptest.NewRequest("GET", "/dest/cert?token="+tok+"&name=ca.crt", nil))
	if rr.Code != 200 || rr.Body.String() != "CADATA" {
		t.Errorf("cert good token: %d %q", rr.Code, rr.Body.String())
	}
	// Unknown cert name rejected.
	rr = httptest.NewRecorder()
	s.handleDestCert(rr, httptest.NewRequest("GET", "/dest/cert?token="+tok+"&name=secret", nil))
	if rr.Code != 400 {
		t.Errorf("unknown cert name: %d, want 400", rr.Code)
	}
}

// isPrivateIP must pick out public IPv4s (the destination's reachable address).
func TestIsPrivateIP(t *testing.T) {
	for _, priv := range []string{"10.0.0.1", "192.168.1.5", "172.16.0.1", "127.0.0.1", "169.254.1.1", "bogus"} {
		if !isPrivateIP(priv) {
			t.Errorf("%s should be treated as non-public", priv)
		}
	}
	for _, pub := range []string{"172.237.70.65", "203.0.113.10", "8.8.8.8"} {
		if isPrivateIP(pub) {
			t.Errorf("%s should be treated as public", pub)
		}
	}
}
