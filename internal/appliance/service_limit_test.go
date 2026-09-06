package appliance

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/tiny125/vm-replication/internal/api"
	"github.com/tiny125/vm-replication/internal/linode"
	"github.com/tiny125/vm-replication/internal/store"
)

// F-30: a two-disk cutover's final data-disk clone died live with the raw
// Linode error passed straight through:
//
//	cutover failed: clone data disk 1 into f14-twodisk-cutover-1:
//	linode POST /volumes/17756356/clone: 400 Bad Request:
//	{"errors": [{"reason": "You've reached a limit for the number of active
//	 services on your account. Please contact Support to request an increase
//	 and provide the total number of services you may need."}]}
//
// after the operator had already paid for the full replication, boot
// conversion, and destination creation — and the message never said what
// vm-replication needed or how many. These tests cover: rewriting that error
// with the arithmetic (needed vs. current), and the up-front informational
// event fired at the start of a cutover so the operator can act before the
// expensive work.

// fakeUsageLinode is a tiny stand-in for the Linode API exposing just enough
// (GET /linode/instances, GET /volumes) to drive linodeServiceUsage.
type fakeUsageLinode struct {
	srv       *httptest.Server
	instances int
	volumes   int
	// failInstances/failVolumes make the respective list call 500, to exercise
	// the "usage lookup itself failed" fallback path.
	failInstances, failVolumes bool
}

func newFakeUsageLinode(t *testing.T, instances, volumes int) *fakeUsageLinode {
	t.Helper()
	f := &fakeUsageLinode{instances: instances, volumes: volumes}
	mux := http.NewServeMux()
	mux.HandleFunc("/linode/instances", func(w http.ResponseWriter, r *http.Request) {
		if f.failInstances {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"errors":[{"reason":"nope"}]}`))
			return
		}
		data := make([]map[string]any, f.instances)
		for i := range data {
			data[i] = map[string]any{"id": i + 1, "label": "x", "status": "running"}
		}
		writeJSON(w, http.StatusOK, map[string]any{"data": data})
	})
	mux.HandleFunc("/volumes", func(w http.ResponseWriter, r *http.Request) {
		if f.failVolumes {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"errors":[{"reason":"nope"}]}`))
			return
		}
		data := make([]map[string]any, f.volumes)
		for i := range data {
			data[i] = map[string]any{"id": i + 1, "label": "x", "status": "active"}
		}
		writeJSON(w, http.StatusOK, map[string]any{"data": data})
	})
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeUsageLinode) client() *linode.Client {
	return linode.NewWithBase("tok", f.srv.URL)
}

// TestLinodeServiceUsageCounts confirms the two-call count is exactly the
// account's instance/volume totals.
func TestLinodeServiceUsageCounts(t *testing.T) {
	f := newFakeUsageLinode(t, 7, 15)
	got, err := linodeServiceUsage(context.Background(), f.client())
	if err != nil {
		t.Fatalf("linodeServiceUsage: %v", err)
	}
	if got.Instances != 7 || got.Volumes != 15 {
		t.Errorf("usage = %+v, want Instances=7 Volumes=15", got)
	}
}

// realServiceLimitErr is the exact error do() produces for the live F-30
// evidence (see findings.md F-30).
func realServiceLimitErr() error {
	return errors.New(`linode POST /volumes/17756356/clone: 400 Bad Request: {"errors": [{"reason": "You've reached a limit for the number of active services on your account. Please contact Support to request an increase and provide the total number of services you may need."}]}`)
}

// TestWrapServiceLimitNamesRequirementAndCurrent: the rewritten error must
// state both how many services this step needs and how many the account
// currently holds — the two numbers the raw Linode error never gave the
// operator.
func TestWrapServiceLimitNamesRequirementAndCurrent(t *testing.T) {
	f := newFakeUsageLinode(t, 18, 42)
	need := serviceNeed{Instances: 1, Volumes: 3}
	got := wrapServiceLimit(context.Background(), f.client(), realServiceLimitErr(), need)
	msg := got.Error()
	for _, want := range []string{"4", "1 instance", "3 volume", "60", "18 instance", "42 volume"} {
		if !strings.Contains(msg, want) {
			t.Errorf("rewritten error should mention %q, got: %s", want, msg)
		}
	}
}

// TestWrapServiceLimitPassesThroughUnrelatedErrors: an error that is not a
// service-limit hit must come back byte-for-byte unchanged — this rewrite
// must never mask or alter an unrelated failure (e.g. a genuinely bad label,
// or a transient 5xx).
func TestWrapServiceLimitPassesThroughUnrelatedErrors(t *testing.T) {
	f := newFakeUsageLinode(t, 1, 1)
	orig := errors.New(`linode POST /volumes: 400 Bad Request: {"errors": [{"reason": "Label must be 1-32 characters"}]}`)
	got := wrapServiceLimit(context.Background(), f.client(), orig, serviceNeed{Instances: 1, Volumes: 1})
	if got != orig {
		t.Errorf("unrelated error must pass through unchanged, got: %v", got)
	}
}

// TestWrapServiceLimitSurvivesUsageLookupFailure: if counting current usage
// itself fails, the rewrite must still say what was NEEDED (that much is
// known for certain) rather than silently falling back to the raw, opaque
// Linode error.
func TestWrapServiceLimitSurvivesUsageLookupFailure(t *testing.T) {
	f := newFakeUsageLinode(t, 1, 1)
	f.failInstances = true
	got := wrapServiceLimit(context.Background(), f.client(), realServiceLimitErr(), serviceNeed{Instances: 1, Volumes: 2})
	msg := got.Error()
	if !strings.Contains(msg, "3") || !strings.Contains(msg, "1 instance") || !strings.Contains(msg, "2 volume") {
		t.Errorf("should still state the requirement even when usage lookup fails, got: %s", msg)
	}
}

// TestAnnounceCutoverServiceNeedEmitsEvent: at the start of a cutover, an
// informational event must state how many additional services the run will
// need and how many the account currently has — BEFORE any expensive work
// happens, so the operator can act on it instead of discovering the limit
// only after the full replication + conversion + destination creation.
func TestAnnounceCutoverServiceNeedEmitsEvent(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	m, _, err := st.CreateMigration(ctx, api.CreateMigrationRequest{
		Name: "f30", SourceHostname: "f30", BootTarget: api.BootTargetDisk,
		LinodeType: "g6-nanode-1",
		Devices: []api.DeviceSpec{
			{Device: "/dev/sda", SizeBytes: 25 << 30},
			{Device: "/dev/sdb", SizeBytes: 25 << 30},
		},
	})
	if err != nil {
		t.Fatalf("create migration: %v", err)
	}

	f := newFakeUsageLinode(t, 18, 42)
	s := &Server{st: st, ctx: ctx}
	s.announceCutoverServiceNeed(ctx, m.ID, f.client(), serviceNeed{Instances: 1, Volumes: 1})

	evs, err := st.Events(ctx, m.ID, 0)
	if err != nil {
		t.Fatalf("events: %v", err)
	}
	var found bool
	for _, e := range evs {
		low := strings.ToLower(e.Message)
		if strings.Contains(low, "cutover") && strings.Contains(e.Message, "2") /* total need */ &&
			strings.Contains(e.Message, "60") /* total current */ {
			found = true
		}
	}
	if !found {
		t.Errorf("expected an up-front event naming the service requirement and current usage, got: %+v", evs)
	}
}

// TestAnnounceCutoverServiceNeedSkipsSilentlyOnLookupFailure: this is a
// best-effort diagnostic, not a gate — if the usage lookup fails, the cutover
// must not be blocked and no misleading (zero-filled) event should appear.
func TestAnnounceCutoverServiceNeedSkipsSilentlyOnLookupFailure(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	m, _, err := st.CreateMigration(ctx, api.CreateMigrationRequest{
		Name: "f30-fail", SourceHostname: "f30-fail", BootTarget: api.BootTargetDisk,
		LinodeType: "g6-nanode-1",
		Devices:    []api.DeviceSpec{{Device: "/dev/sda", SizeBytes: 25 << 30}},
	})
	if err != nil {
		t.Fatalf("create migration: %v", err)
	}
	f := newFakeUsageLinode(t, 1, 1)
	f.failInstances = true
	s := &Server{st: st, ctx: ctx}
	s.announceCutoverServiceNeed(ctx, m.ID, f.client(), serviceNeed{Instances: 1, Volumes: 0})

	evs, err := st.Events(ctx, m.ID, 0)
	if err != nil {
		t.Fatalf("events: %v", err)
	}
	if len(evs) != 0 {
		t.Errorf("a failed usage lookup must not emit any event, got: %+v", evs)
	}
}
