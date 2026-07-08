package appliance

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/tiny125/vm-replication/internal/api"
	"github.com/tiny125/vm-replication/internal/store"
)

// automationServer builds a Server whose fileAutomation() is true: an in-memory
// store with a Linode token and a known appliance instance id. It makes NO
// network calls (destStatusFor/destReady only read fileDests).
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

// With Linode automation, a file migration must NOT be startable until an
// explicit destination instance has been created AND its receiver is ready.
// This is the fix for the "stuck at start" hang: Start is gated on destReady.
func TestFileDestinationGatesStart(t *testing.T) {
	s := automationServer(t)
	m := api.Migration{ID: 7, BootTarget: api.BootTargetFile, OSImage: "linode/ubuntu24.04", LinodeType: "g6-nanode-1"}

	// No destination created yet.
	if st, _, _, _, _ := s.destStatusFor(m); st != "none" {
		t.Errorf("initial dest state = %q, want none", st)
	}
	if s.destReady(m) {
		t.Error("destReady must be false before a destination is created")
	}

	// Launching (record present, no IP yet).
	s.fileDests.Store(m.ID, &fileDest{label: "game2-dest"})
	if st, _, _, _, _ := s.destStatusFor(m); st != "launching" {
		t.Errorf("dest state = %q, want launching", st)
	}
	if s.destReady(m) {
		t.Error("destReady must be false while launching")
	}

	// Installing (IP known, receiver not up).
	s.fileDests.Store(m.ID, &fileDest{instanceID: 100, ip: "172.237.70.65", label: "game2-dest", token: "tk"})
	if st, id, ip, _, cmd := s.destStatusFor(m); st != "installing" || id != 100 || ip != "172.237.70.65" || !strings.Contains(cmd, "/dest/install.sh?token=tk") {
		t.Errorf("installing: state=%q id=%d ip=%q cmd=%q", st, id, ip, cmd)
	}
	if s.destReady(m) {
		t.Error("destReady must be false while installing")
	}

	// Ready (receiver reachable).
	s.fileDests.Store(m.ID, &fileDest{instanceID: 100, ip: "172.237.70.65", ready: true, label: "game2-dest"})
	if st, _, _, _, _ := s.destStatusFor(m); st != "ready" {
		t.Errorf("dest state = %q, want ready", st)
	}
	if !s.destReady(m) {
		t.Error("destReady must be true once the receiver is reachable")
	}

	// Failed carries the error message.
	s.fileDests.Store(m.ID, &fileDest{label: "game2-dest", failed: true, errMsg: "boom"})
	if st, _, _, e, _ := s.destStatusFor(m); st != "failed" || e != "boom" {
		t.Errorf("failed: state=%q err=%q", st, e)
	}
}

// Without automation the file method stages on the appliance: no destination
// step, and Start is NOT gated on a destination (destReady is true).
func TestFileDestinationFallbackNoAutomation(t *testing.T) {
	s := &Server{} // no store/token → fileAutomation()==false
	m := api.Migration{ID: 1, BootTarget: api.BootTargetFile}
	if st, _, _, _, _ := s.destStatusFor(m); st != "fallback" {
		t.Errorf("no-automation dest state = %q, want fallback", st)
	}
	if !s.destReady(m) {
		t.Error("fallback must not gate Start (destReady true)")
	}
	// Block migrations have no destination step at all.
	if st, _, _, _, _ := s.destStatusFor(api.Migration{BootTarget: api.BootTargetVolume}); st != "" {
		t.Errorf("block dest state = %q, want empty", st)
	}
}

// createFileDestination refuses when it cannot proceed, so the endpoint returns
// a clean 409 instead of launching nothing.
func TestCreateFileDestinationGuards(t *testing.T) {
	s := &Server{} // no automation
	if err := s.createFileDestination(api.Migration{BootTarget: api.BootTargetVolume}, "x", "pw"); err == nil {
		t.Error("must refuse a non-file migration")
	}
	if err := s.createFileDestination(api.Migration{BootTarget: api.BootTargetFile}, "x", "pw"); err == nil {
		t.Error("must refuse when there is no Linode automation")
	}
	// With automation but a destination already up, a second create is refused.
	s2 := automationServer(t)
	m := api.Migration{ID: 5, BootTarget: api.BootTargetFile, OSImage: "linode/ubuntu24.04", LinodeType: "g6-nanode-1"}
	s2.fileDests.Store(m.ID, &fileDest{instanceID: 9, ready: true})
	if err := s2.createFileDestination(m, "x", "pw"); err == nil {
		t.Error("must refuse a second create when a destination already exists")
	}
}

// The manual receiver-install endpoint is token-gated and serves the install
// script, so the operator can bring the receiver up by hand in Lish.
func TestHandleDestInstall(t *testing.T) {
	s := &Server{}
	s.cfg.PublicHost = "203.0.113.10"
	s.cfg.ConsolePort = 8080
	tok := s.registerDestBootstrap(3, time.Hour)

	rr := httptest.NewRecorder()
	s.handleDestInstall(rr, httptest.NewRequest("GET", "/dest/install.sh?token=nope", nil))
	if rr.Code != 403 {
		t.Errorf("bad token: %d, want 403", rr.Code)
	}
	rr = httptest.NewRecorder()
	s.handleDestInstall(rr, httptest.NewRequest("GET", "/dest/install.sh?token="+tok, nil))
	if rr.Code != 200 || !strings.Contains(rr.Body.String(), "vmrepl-receiver.service") {
		t.Errorf("good token: %d, body lacks the receiver unit", rr.Code)
	}
}
