package appliance

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/tiny125/vm-replication/internal/api"
	"github.com/tiny125/vm-replication/internal/store"
)

// fakeLinodePlans is a minimal stand-in for the Linode API, just enough to
// satisfy the disk-boot create path: GET /profile (token validity) and
// GET /linode/types (plan sizing).
func fakeLinodePlans(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/profile", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"username": "tester", "email": "t@example.com"})
	})
	mux.HandleFunc("/linode/types", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"data": []map[string]any{
			{"id": "g6-standard-2", "label": "Linode 4GB", "class": "standard", "disk": 81920, "memory": 4096, "vcpus": 2,
				"price": map[string]float64{"hourly": 0.036, "monthly": 24}},
		}})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// bootTargetTestServer builds a Server backed by an in-memory store and a
// fake Linode API (token + plan catalog), with no TLS material configured —
// ensureReceivers therefore fails fast (no cert files) and does not attempt
// to bind a port, so handleCreateMigration can be exercised end-to-end
// without any real network or listening socket.
func bootTargetTestServer(t *testing.T) *Server {
	t.Helper()
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	st.SetEncryptionKey(make([]byte, 32))
	if err := st.SetLinodeToken(context.Background(), "tok-123"); err != nil {
		t.Fatalf("set token: %v", err)
	}
	fake := fakeLinodePlans(t)
	s := &Server{st: st, ctx: context.Background(), linodeBase: fake.URL}
	s.cfg.RPOTargetSec = 120
	s.cfg.PublicHost = "203.0.113.10"
	s.cfg.ConsolePort = 8080
	s.cfg.BaseReceiverPort = 5000
	return s
}

func createMigration(t *testing.T, s *Server, req api.CreateMigrationRequest) *httptest.ResponseRecorder {
	t.Helper()
	body, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}
	rr := httptest.NewRecorder()
	s.handleCreateMigration(rr, httptest.NewRequest(http.MethodPost, "/api/v1/migrations", bytes.NewReader(body)))
	return rr
}

// An empty boot_target must default to disk boot (the Linode's own local
// disk) — the file-transfer method, which used to be the implicit default,
// has been removed, and disk boot replaces it as the new default.
func TestCreateMigrationEmptyBootTargetDefaultsToDisk(t *testing.T) {
	s := bootTargetTestServer(t)
	rr := createMigration(t, s, api.CreateMigrationRequest{
		Name: "web01", SourceHostname: "web01.prod", SourceIP: "203.0.113.20",
		Devices: []api.DeviceSpec{{Device: "/dev/sda", SizeBytes: 20 << 30}},
		// BootTarget deliberately left empty.
	})
	if rr.Code != http.StatusCreated {
		t.Fatalf("create with empty boot_target: status %d, body %s", rr.Code, rr.Body.String())
	}
	var view api.MigrationView
	if err := json.Unmarshal(rr.Body.Bytes(), &view); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if view.Migration.BootTarget != api.BootTargetDisk {
		t.Errorf("BootTarget = %q, want %q (the default)", view.Migration.BootTarget, api.BootTargetDisk)
	}
}

// Creating a migration with the removed file-transfer method must be
// rejected with a clear, actionable 400 — not a generic "must be 'volume' or
// 'disk'" that leaves the operator wondering why a value that used to work no
// longer does.
func TestCreateMigrationRejectsRemovedFileMethod(t *testing.T) {
	s := bootTargetTestServer(t)
	rr := createMigration(t, s, api.CreateMigrationRequest{
		Name: "web01", SourceHostname: "web01.prod", SourceIP: "203.0.113.20",
		Devices:    []api.DeviceSpec{{Device: "/", SizeBytes: 4 << 30}},
		BootTarget: "file",
	})
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("create with boot_target=file: status %d, want 400. body: %s", rr.Code, rr.Body.String())
	}
	var errResp struct {
		Error string `json:"error"`
	}
	_ = json.Unmarshal(rr.Body.Bytes(), &errResp)
	for _, want := range []string{"file-transfer", "removed", "disk", "volume"} {
		if !strings.Contains(errResp.Error, want) {
			t.Errorf("error message %q should mention %q so the operator knows what to do instead", errResp.Error, want)
		}
	}
}

// Product decision: the console no longer OFFERS volume boot (Linode's
// Backups/Images/cross-DC tooling doesn't cover Block Storage volumes), but
// the code path is deliberately kept so it can be re-enabled later by editing
// code alone. The HTTP API must therefore still ACCEPT boot_target:"volume"
// and create the migration normally — it must not 400.
func TestCreateMigrationAcceptsVolumeBootTarget(t *testing.T) {
	s := bootTargetTestServer(t)
	rr := createMigration(t, s, api.CreateMigrationRequest{
		Name: "web02", SourceHostname: "web02.prod", SourceIP: "203.0.113.21",
		Devices:    []api.DeviceSpec{{Device: "/dev/sda", SizeBytes: 20 << 30}},
		BootTarget: api.BootTargetVolume,
	})
	if rr.Code != http.StatusCreated {
		t.Fatalf("create with boot_target=volume: status %d, want %d — the volume-boot code path must remain fully functional even though the console no longer offers it. body: %s",
			rr.Code, http.StatusCreated, rr.Body.String())
	}
	var view api.MigrationView
	if err := json.Unmarshal(rr.Body.Bytes(), &view); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if view.Migration.BootTarget != api.BootTargetVolume {
		t.Errorf("BootTarget = %q, want %q", view.Migration.BootTarget, api.BootTargetVolume)
	}
}

// An unknown boot_target (neither a current nor a recognizably-removed
// method) still gets the generic, accurate error.
func TestCreateMigrationRejectsUnknownBootTarget(t *testing.T) {
	s := bootTargetTestServer(t)
	rr := createMigration(t, s, api.CreateMigrationRequest{
		Name: "web01", SourceHostname: "web01.prod", SourceIP: "203.0.113.20",
		Devices:    []api.DeviceSpec{{Device: "/dev/sda", SizeBytes: 20 << 30}},
		BootTarget: "bogus",
	})
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("create with bogus boot_target: status %d, want 400", rr.Code)
	}
	var errResp struct {
		Error string `json:"error"`
	}
	_ = json.Unmarshal(rr.Body.Bytes(), &errResp)
	if !strings.Contains(errResp.Error, "'volume' or 'disk'") {
		t.Errorf("error message %q should name the two valid methods", errResp.Error)
	}
}

// A pre-existing migration row that still carries a removed boot_target
// (e.g. 'file', from before the method was dropped) must not crash the
// view/validations computation the console polls every few seconds — it
// should degrade gracefully rather than panicking the appliance.
func TestViewAndValidationsToleratesLegacyBootTarget(t *testing.T) {
	s := bootTargetTestServer(t)
	m := api.Migration{
		ID: 1, State: api.MigReplicating, BootTarget: "file",
		Disks: []api.Disk{{ID: 1, Index: 0, SourceDevice: "/", SizeBytes: 4 << 30}},
	}
	// Must not panic.
	v := s.view(context.Background(), m, "tok")
	if v.Migration.BootTarget != "file" {
		t.Errorf("view must preserve the raw (legacy) boot_target, got %q", v.Migration.BootTarget)
	}
	checks := s.validations(m, 0)
	if len(checks) == 0 {
		t.Error("validations must still return a checklist for a legacy-method migration")
	}
}
