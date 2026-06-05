package controlplane

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/tiny125/vm-replication/internal/api"
	"github.com/tiny125/vm-replication/internal/store"
)

func newTestServer(t *testing.T) (*httptest.Server, string) {
	t.Helper()
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	const token = "secret-token"
	srv := httptest.NewServer(New(st, token).Handler())
	t.Cleanup(srv.Close)
	return srv, token
}

func do(t *testing.T, method, url, token string, body any) (*http.Response, []byte) {
	t.Helper()
	var r io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		r = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, url, r)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	data, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	return resp, data
}

func TestAuthRequired(t *testing.T) {
	srv, _ := newTestServer(t)
	resp, _ := do(t, http.MethodGet, srv.URL+"/api/v1/status", "wrong", nil)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", resp.StatusCode)
	}
}

func TestEndToEndFlow(t *testing.T) {
	srv, token := newTestServer(t)

	// Register a source server.
	resp, _ := do(t, http.MethodPost, srv.URL+"/api/v1/servers", token,
		api.RegisterServerRequest{Name: "web01", Role: api.RoleSource, Device: "/dev/sda"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("register: status %d", resp.StatusCode)
	}

	// Create a job.
	resp, data := do(t, http.MethodPost, srv.URL+"/api/v1/jobs", token,
		api.CreateJobRequest{Name: "mig-web01", TargetAddr: "1.2.3.4:4444", RPOTargetSec: 60})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create job: status %d (%s)", resp.StatusCode, data)
	}
	var job api.Job
	if err := json.Unmarshal(data, &job); err != nil {
		t.Fatalf("decode job: %v", err)
	}

	// Report a successful sync.
	now := time.Now()
	resp, data = do(t, http.MethodPost, srv.URL+"/api/v1/jobs/"+itoa(job.ID)+"/syncs", token,
		api.ReportSyncRequest{Mode: api.SyncFull, StartedAt: now.Add(-5 * time.Second), FinishedAt: now,
			TotalBlocks: 10, ChangedBlocks: 10, BytesOnWire: 4096, DurationMS: 5000, OK: true})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("report sync: status %d (%s)", resp.StatusCode, data)
	}

	// Status should reflect the job and not be breached.
	resp, data = do(t, http.MethodGet, srv.URL+"/api/v1/status", token, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status: %d", resp.StatusCode)
	}
	var statuses []api.JobStatus
	if err := json.Unmarshal(data, &statuses); err != nil {
		t.Fatalf("decode status: %v", err)
	}
	if len(statuses) != 1 || statuses[0].Job.Name != "mig-web01" {
		t.Fatalf("unexpected status payload: %s", data)
	}
	if statuses[0].LastSync == nil || statuses[0].TotalSyncs != 1 {
		t.Errorf("status missing sync info: %+v", statuses[0])
	}

	// Metrics endpoint should expose the job's RPO gauge.
	resp, data = do(t, http.MethodGet, srv.URL+"/metrics", token, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("metrics: %d", resp.StatusCode)
	}
	if !strings.Contains(string(data), `vm_repl_rpo_seconds{job="mig-web01"`) {
		t.Errorf("metrics missing rpo gauge:\n%s", data)
	}
}

func TestRejectInvalidStateAndMode(t *testing.T) {
	srv, token := newTestServer(t)

	resp, data := do(t, http.MethodPost, srv.URL+"/api/v1/jobs", token, api.CreateJobRequest{Name: "j1"})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create job: %d (%s)", resp.StatusCode, data)
	}
	var job api.Job
	_ = json.Unmarshal(data, &job)
	id := itoa(job.ID)

	// A bogus state (XSS/metrics-injection vector) must be rejected.
	resp, _ = do(t, http.MethodPost, srv.URL+"/api/v1/jobs/"+id+"/state", token,
		map[string]string{"state": "<img src=x onerror=alert(1)>"})
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("invalid state: status %d, want 400", resp.StatusCode)
	}

	// A bogus sync mode must be rejected.
	resp, _ = do(t, http.MethodPost, srv.URL+"/api/v1/jobs/"+id+"/syncs", token,
		map[string]any{"mode": "evil", "ok": true})
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("invalid mode: status %d, want 400", resp.StatusCode)
	}

	// A valid state still works.
	resp, _ = do(t, http.MethodPost, srv.URL+"/api/v1/jobs/"+id+"/state", token,
		api.SetStateRequest{State: api.JobCutover})
	if resp.StatusCode != http.StatusOK {
		t.Errorf("valid state: status %d, want 200", resp.StatusCode)
	}
}

func itoa(n int64) string {
	return strings.TrimSpace(string(jsonNumber(n)))
}
func jsonNumber(n int64) []byte {
	b, _ := json.Marshal(n)
	return b
}
