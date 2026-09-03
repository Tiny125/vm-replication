package appliance

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/tiny125/vm-replication/internal/store"
)

// fakeTokenLinode is a tiny stand-in for the Linode API, enough to drive
// handleSetLinodeToken: GET /profile (token validation + account resolution)
// and GET /linode/instances/{id} (F-26's appliance-ownership check).
type fakeTokenLinode struct {
	srv *httptest.Server
	// profileStatus, if non-zero, makes GET /profile fail with this status
	// instead of returning a profile.
	profileStatus int
	// instanceStatus, if non-zero, makes GET /linode/instances/{id} fail with
	// this status instead of returning an instance.
	instanceStatus int
	// seenInstanceID records the id the handler looked up, so a test can
	// confirm it queried the right Linode.
	seenInstanceID string
}

func newFakeTokenLinode(t *testing.T) *fakeTokenLinode {
	t.Helper()
	f := &fakeTokenLinode{}
	mux := http.NewServeMux()
	mux.HandleFunc("/profile", func(w http.ResponseWriter, r *http.Request) {
		if f.profileStatus != 0 {
			w.WriteHeader(f.profileStatus)
			_, _ = w.Write([]byte(`{"errors":[{"reason":"nope"}]}`))
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"username": "flim_admin", "email": "flim@akamai.com"})
	})
	mux.HandleFunc("/linode/instances/", func(w http.ResponseWriter, r *http.Request) {
		f.seenInstanceID = strings.TrimPrefix(r.URL.Path, "/linode/instances/")
		if f.instanceStatus != 0 {
			w.WriteHeader(f.instanceStatus)
			_, _ = w.Write([]byte(`{"errors":[{"reason":"You do not have permission to access this Linode","field":"linode_id"}]}`))
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"id": 103154536, "label": "migration-instance", "status": "running", "region": "sg-sin-2"})
	})
	// Anything else (e.g. audit-bucket provisioning, best-effort) just 404s.
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

// tokenServer builds a Server wired to a fake Linode API and an in-memory
// store, with no token stored yet, so handleSetLinodeToken can be exercised
// without touching the network.
func tokenServer(t *testing.T, f *fakeTokenLinode, applianceLinodeID int64) *Server {
	t.Helper()
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	st.SetEncryptionKey(make([]byte, 32))
	s := &Server{st: st, ctx: context.Background()}
	s.cfg.ApplianceLinodeID = applianceLinodeID
	s.linodeBase = f.srv.URL
	return s
}

func postToken(t *testing.T, s *Server, token string) *httptest.ResponseRecorder {
	t.Helper()
	body := fmt.Sprintf(`{"token":%q}`, token)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/settings/linode-token", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	s.handleSetLinodeToken(rr, req)
	return rr
}

// F-26: a token whose account does NOT own the appliance Linode (GetInstance
// 403s — "You do not have permission to access this Linode") must be rejected
// with 400 and a message naming BOTH the resolved account and the appliance
// Linode id, not silently accepted only to fail later at migration-create time
// with a raw, misleading Linode 403.
func TestSetLinodeTokenRejectsWrongAccount(t *testing.T) {
	f := newFakeTokenLinode(t)
	f.instanceStatus = http.StatusForbidden
	s := tokenServer(t, f, 103154536)

	rr := postToken(t, s, "tok-wrong-account")
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body: %s", rr.Code, rr.Body.String())
	}
	body := rr.Body.String()
	for _, want := range []string{"flim_admin", "flim@akamai.com", "103154536"} {
		if !strings.Contains(body, want) {
			t.Errorf("rejection message should name %q (both sides of the mismatch), got: %s", want, body)
		}
	}
	if f.seenInstanceID != "103154536" {
		t.Errorf("handler looked up instance %q, want 103154536", f.seenInstanceID)
	}
	// The token must NOT be persisted — a rejected token should not become the
	// stored one.
	if tok, err := s.st.LinodeToken(context.Background()); err == nil && tok != "" {
		t.Errorf("rejected token must not be stored, got %q", tok)
	}
}

// A 404 on the appliance-Linode lookup (the id genuinely doesn't exist under
// this token's account, which reads the same as "not yours" from the token's
// point of view) must also be rejected the same way as a 403.
func TestSetLinodeTokenRejectsOnNotFound(t *testing.T) {
	f := newFakeTokenLinode(t)
	f.instanceStatus = http.StatusNotFound
	s := tokenServer(t, f, 103154536)

	rr := postToken(t, s, "tok-wrong-account")
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body: %s", rr.Code, rr.Body.String())
	}
}

// A transient/unrelated failure looking up the appliance Linode (5xx) must NOT
// reject the token — only a definite 403/404 means "not in this account". A
// flaky Linode API response must not block saving an otherwise-valid token.
func TestSetLinodeTokenAllowsOnTransientLookupError(t *testing.T) {
	f := newFakeTokenLinode(t)
	f.instanceStatus = http.StatusInternalServerError
	s := tokenServer(t, f, 103154536)

	rr := postToken(t, s, "tok-good")
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (transient errors must not reject the token); body: %s", rr.Code, rr.Body.String())
	}
	var resp map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp["ok"] != true {
		t.Errorf("ok = %v, want true", resp["ok"])
	}
	if resp["linode_account"] != "flim_admin <flim@akamai.com>" {
		t.Errorf("linode_account = %v, want the resolved account", resp["linode_account"])
	}
}

// When ApplianceLinodeID == 0 (non-automation mode: no appliance Linode is
// configured), the ownership check must be skipped entirely — there is nothing
// to check against, and GetInstance must not even be called.
func TestSetLinodeTokenSkipsCheckWithoutApplianceLinodeID(t *testing.T) {
	f := newFakeTokenLinode(t)
	f.instanceStatus = http.StatusForbidden // would reject if the check ran
	s := tokenServer(t, f, 0)

	rr := postToken(t, s, "tok-good")
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (ApplianceLinodeID==0 must skip the check); body: %s", rr.Code, rr.Body.String())
	}
	if f.seenInstanceID != "" {
		t.Errorf("GetInstance should not have been called when ApplianceLinodeID==0, but it was queried for %q", f.seenInstanceID)
	}
}

// The success response shape must be unchanged: exactly {"ok":true,
// "linode_account": "..."} — F-26 must not add/remove fields from the happy
// path.
func TestSetLinodeTokenSuccessShapeUnchanged(t *testing.T) {
	f := newFakeTokenLinode(t)
	s := tokenServer(t, f, 103154536)

	rr := postToken(t, s, "tok-good")
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", rr.Code, rr.Body.String())
	}
	var resp map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp) != 2 {
		t.Errorf("response has %d fields, want exactly 2 (ok, linode_account): %v", len(resp), resp)
	}
	if resp["ok"] != true || resp["linode_account"] != "flim_admin <flim@akamai.com>" {
		t.Errorf("unexpected response: %v", resp)
	}
	if got := strconv.FormatInt(s.cfg.ApplianceLinodeID, 10); got != "103154536" {
		t.Fatalf("sanity: ApplianceLinodeID = %s", got)
	}
}
