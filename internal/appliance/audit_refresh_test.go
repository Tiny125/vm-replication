package appliance

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/tiny125/vm-replication/internal/linode"
	"github.com/tiny125/vm-replication/internal/store"
)

// fakeLinode is a tiny stand-in for the Linode Object Storage API, enough to
// drive the audit-bucket provisioning / refresh / upload flows. Configure its
// behavior with the exported fields.
type fakeLinode struct {
	srv *httptest.Server
	// bucketExists controls whether the account holds our audit bucket.
	bucketExists bool
	// bucket is what ListBuckets returns for it (with the real cluster/hostname).
	bucket map[string]any
	// createConflict makes CreateBucket answer "already exists" (the reuse path).
	createConflict bool
	// putStatus is the HTTP status the presigned S3 PUT returns (0 => 200).
	putStatus int
	// seenPutSeg records the path segment the object-url call used (proves object
	// ops target the right cluster).
	seenPutSeg string
}

func newFakeLinode(t *testing.T) *fakeLinode {
	t.Helper()
	f := &fakeLinode{}
	mux := http.NewServeMux()
	// List buckets.
	mux.HandleFunc("/object-storage/buckets", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost { // CreateBucket
			if f.createConflict {
				w.WriteHeader(http.StatusBadRequest)
				_, _ = w.Write([]byte(`{"errors":[{"reason":"A bucket with this label already exists in your account"}]}`))
				return
			}
			writeJSON(w, http.StatusOK, f.bucket)
			return
		}
		var data []any
		if f.bucketExists {
			data = append(data, f.bucket)
		}
		writeJSON(w, http.StatusOK, map[string]any{"data": data})
	})
	// Presigned object-url: /object-storage/buckets/{cluster}/{label}/object-url
	mux.HandleFunc("/object-storage/buckets/", func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/object-url") {
			parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/object-storage/buckets/"), "/")
			if len(parts) > 0 {
				f.seenPutSeg = parts[0]
			}
			writeJSON(w, http.StatusOK, map[string]any{"url": f.srv.URL + "/s3put"})
			return
		}
		http.NotFound(w, r)
	})
	// The presigned S3 PUT target.
	mux.HandleFunc("/s3put", func(w http.ResponseWriter, r *http.Request) {
		if f.putStatus != 0 {
			w.WriteHeader(f.putStatus)
			_, _ = w.Write([]byte(`<Error><Code>NoSuchBucket</Code><Message>The specified bucket was not found.</Message></Error>`))
			return
		}
		w.WriteHeader(http.StatusOK)
	})
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

// auditServer builds a Server wired to a fake Linode API and an in-memory store
// with a saved token, so audit-bucket flows run without touching the network.
func auditServer(t *testing.T, f *fakeLinode) *Server {
	t.Helper()
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	st.SetEncryptionKey(make([]byte, 32))
	if err := st.SetLinodeToken(context.Background(), "tok"); err != nil {
		t.Fatalf("set token: %v", err)
	}
	s := &Server{st: st, ctx: context.Background()}
	s.cfg.ApplianceLinodeID = 100668062
	s.cfg.ObjRegion = "sg-sin-2" // avoid a GetInstance call for the region
	s.linodeBase = f.srv.URL
	return s
}

// sgBucket is the real-world SG bucket: region sg-sin-2 but cluster sg-sin-1.
func sgBucket() map[string]any {
	return map[string]any{
		"label":    "vmrep-audit-100668062",
		"region":   "sg-sin-2",
		"cluster":  "sg-sin-1",
		"hostname": "vmrep-audit-100668062.sg-sin-1.linodeobjects.com",
	}
}

// When the audit bucket already exists (created in a prior session), reusing it
// must store the REAL record — including its storage cluster — not a synthesized
// {label, region} with the cluster dropped. Otherwise object uploads target the
// region path (sg-sin-2) instead of the cluster (sg-sin-1) and 404, which is the
// bug that made the console falsely report the bucket "no longer exists".
func TestEnsureAuditBucketReusesRealClusterOnExisting(t *testing.T) {
	f := newFakeLinode(t)
	f.bucketExists = true
	f.bucket = sgBucket()
	f.createConflict = true // CreateBucket says "already exists" → reuse path
	s := auditServer(t, f)

	s.ensureAuditBucket(context.Background(), "tok")

	b, ok := s.auditBucket(context.Background())
	if !ok {
		t.Fatal("audit bucket should be ready after reusing an existing bucket")
	}
	if b.Cluster != "sg-sin-1" {
		t.Errorf("stored bucket cluster = %q, want sg-sin-1 (the real record, not a region-only synthesis)", b.Cluster)
	}
	// And an actual upload must address the cluster path, not the region.
	cl := s.newLinode("tok")
	if err := cl.PutObject(context.Background(), b, "main.log", "text/plain", []byte("hi")); err != nil {
		t.Fatalf("PutObject to the reused bucket failed: %v", err)
	}
	if f.seenPutSeg != "sg-sin-1" {
		t.Errorf("object-url used path segment %q, want the cluster sg-sin-1", f.seenPutSeg)
	}
}

// The uploader's self-heal must not clear "ready" on a transient/misclassified
// 404: before declaring the bucket gone it must confirm — via ListBuckets — that
// the bucket truly no longer exists. A PUT that 404s while the bucket still
// exists is transient (or an endpoint hiccup), so ready state must survive.
func TestFlushAuditKeepsReadyWhenBucketStillExists(t *testing.T) {
	f := newFakeLinode(t)
	f.bucketExists = true // the bucket IS there…
	f.bucket = sgBucket()
	f.putStatus = http.StatusNotFound // …but the upload 404s (transient)
	s := auditServer(t, f)
	ctx := context.Background()

	// Mark ready with the real bucket, and queue one console audit line to upload.
	s.saveAuditBucket(ctx, mustBucket(t, f.bucket))
	if err := s.st.AddAudit(ctx, 0, "info", "console", "something happened"); err != nil {
		t.Fatalf("add audit: %v", err)
	}

	s.flushAudit(map[int64]int64{})

	ready, _, _ := s.st.GetSetting(ctx, keyAuditReady)
	if ready != "1" {
		t.Error("ready state must survive a 404 upload while the bucket still exists (transient), not be cleared")
	}
}

// But when the bucket is GENUINELY gone (deleted in Cloud Manager), the self-heal
// must still clear ready so the console reflects reality.
func TestFlushAuditClearsReadyWhenBucketReallyGone(t *testing.T) {
	f := newFakeLinode(t)
	f.bucketExists = false // ListBuckets confirms it is gone
	f.bucket = sgBucket()
	f.putStatus = http.StatusNotFound
	s := auditServer(t, f)
	ctx := context.Background()

	s.saveAuditBucket(ctx, mustBucket(t, f.bucket))
	if err := s.st.AddAudit(ctx, 0, "info", "console", "something happened"); err != nil {
		t.Fatalf("add audit: %v", err)
	}

	s.flushAudit(map[int64]int64{})

	ready, _, _ := s.st.GetSetting(ctx, keyAuditReady)
	if ready == "1" {
		t.Error("ready must be cleared once ListBuckets confirms the bucket is truly gone")
	}
}

// The refresh endpoint re-checks reality: if the bucket exists in the account it
// restores the console's ready state (healing a false "no longer exists"), and
// reports the bucket. This is what the card's Refresh button calls.
func TestHandleRefreshAuditBucketRestores(t *testing.T) {
	f := newFakeLinode(t)
	f.bucketExists = true
	f.bucket = sgBucket()
	s := auditServer(t, f)
	ctx := context.Background()
	// Simulate the stuck state: ready cleared with the false error.
	s.setAuditErr("the audit bucket no longer exists (deleted in Cloud Manager or a previous session) — use \"Re-create audit bucket\" to make a new one")

	rr := httptest.NewRecorder()
	s.handleRefreshAuditBucket(rr, httptest.NewRequest("POST", "/api/v1/settings/audit-bucket/refresh", nil).WithContext(ctx))
	if rr.Code != http.StatusOK {
		t.Fatalf("refresh: %d, body %s", rr.Code, rr.Body.String())
	}
	var resp map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp["audit_ready"] != true {
		t.Errorf("refresh should report audit_ready=true, got %v", resp["audit_ready"])
	}
	ready, _, _ := s.st.GetSetting(ctx, keyAuditReady)
	if ready != "1" {
		t.Error("refresh must restore ready state when the bucket exists")
	}
	if aerr, _, _ := s.st.GetSetting(ctx, keyAuditErr); aerr != "" {
		t.Errorf("refresh must clear the stale error, got %q", aerr)
	}
	b, _ := s.auditBucket(ctx)
	if b.Cluster != "sg-sin-1" {
		t.Errorf("refresh must store the real bucket (cluster sg-sin-1), got %q", b.Cluster)
	}
}

// If the bucket really is gone, refresh says so (ready stays false) with a
// truthful message — no false "still there".
func TestHandleRefreshAuditBucketReportsGone(t *testing.T) {
	f := newFakeLinode(t)
	f.bucketExists = false
	f.bucket = sgBucket()
	s := auditServer(t, f)
	ctx := context.Background()

	rr := httptest.NewRecorder()
	s.handleRefreshAuditBucket(rr, httptest.NewRequest("POST", "/api/v1/settings/audit-bucket/refresh", nil).WithContext(ctx))
	if rr.Code != http.StatusOK {
		t.Fatalf("refresh: %d, body %s", rr.Code, rr.Body.String())
	}
	var resp map[string]any
	_ = json.Unmarshal(rr.Body.Bytes(), &resp)
	if resp["audit_ready"] != false {
		t.Errorf("refresh should report audit_ready=false for a missing bucket, got %v", resp["audit_ready"])
	}
	ready, _, _ := s.st.GetSetting(ctx, keyAuditReady)
	if ready == "1" {
		t.Error("refresh must not mark ready when the bucket is absent")
	}
}

func mustBucket(t *testing.T, m map[string]any) (b linode.Bucket) {
	t.Helper()
	raw, _ := json.Marshal(m)
	if err := json.Unmarshal(raw, &b); err != nil {
		t.Fatalf("decode bucket: %v", err)
	}
	return b
}
