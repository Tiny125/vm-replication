package appliance

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/tiny125/vm-replication/internal/api"
)

// Cutover step 1 must freeze a CONSISTENT image: stop new replication passes
// and then WAIT for any pass already in flight to finish (a completed pass
// always ends at a single point in time). The old code only closed the
// listeners — an in-flight pass kept applying to the "frozen" volume and, when
// the operator powered the source off as instructed, was cut partway, leaving
// the image a mix of two points in time.
func TestDrainReceivers(t *testing.T) {
	mig := func(diskIDs ...int64) api.Migration {
		m := api.Migration{ID: 1}
		for _, id := range diskIDs {
			m.Disks = append(m.Disks, api.Disk{ID: id})
		}
		return m
	}

	// All in-flight work finishes promptly -> drained.
	s := &Server{receivers: map[int64]*receiverHandle{}}
	done1 := make(chan struct{})
	s.receivers[1] = &receiverHandle{cancel: func() {}, done: done1}
	go func() { time.Sleep(20 * time.Millisecond); close(done1) }()
	if !s.drainReceivers(mig(1), time.Second) {
		t.Error("expected drain to succeed when the session finishes within the grace")
	}
	if _, ok := s.receivers[1]; ok {
		t.Error("drained receiver must be removed from the map")
	}

	// A session that never ends -> drain gives up after the grace (and reports it).
	s = &Server{receivers: map[int64]*receiverHandle{}}
	s.receivers[2] = &receiverHandle{cancel: func() {}, done: make(chan struct{})}
	start := time.Now()
	if s.drainReceivers(mig(2), 50*time.Millisecond) {
		t.Error("expected drain to time out on a hung session")
	}
	if time.Since(start) > 2*time.Second {
		t.Error("drain must respect the grace bound, not hang")
	}

	// Cancel must be invoked (stops NEW sessions) even when nothing is in flight.
	cancelled := false
	s = &Server{receivers: map[int64]*receiverHandle{}}
	closed := make(chan struct{})
	close(closed)
	s.receivers[3] = &receiverHandle{cancel: func() { cancelled = true }, done: closed}
	if !s.drainReceivers(mig(3), time.Second) || !cancelled {
		t.Error("drain must cancel the receiver and succeed instantly when idle")
	}

	// Disks with no live receiver are fine (already stopped).
	s = &Server{receivers: map[int64]*receiverHandle{}}
	if !s.drainReceivers(mig(9), time.Second) {
		t.Error("drain with no receivers must succeed")
	}
}

// While a guided cutover's step 1 runs (drain + freeze), the console must tell
// the operator to KEEP THE SOURCE RUNNING until the freeze completes — the
// CutoverFreezing view flag drives that banner; it flips on when phase 1
// starts and off when the migration parks in awaiting_cutover (or fails).
func TestCutoverFreezingFlag(t *testing.T) {
	s := &Server{}
	if s.cutoverFreezingFor(1) {
		t.Error("freezing must default to false")
	}
	s.setCutoverFreezing(1, true)
	if !s.cutoverFreezingFor(1) {
		t.Error("freezing not reported after set")
	}
	if s.cutoverFreezingFor(2) {
		t.Error("other migrations must be unaffected")
	}
	s.setCutoverFreezing(1, false)
	if s.cutoverFreezingFor(1) {
		t.Error("freezing must clear")
	}
}

// isSourceDisconnect classifies the receiver error produced when the SOURCE
// vanishes mid-pass (powered off / rebooted): during cutover that is an
// expected consequence of "power off the source" and must be logged softly,
// not as a red replication failure.
func TestIsSourceDisconnect(t *testing.T) {
	for _, err := range []error{
		errors.New("stream closed before done"),
		errors.New("read frame: read tcp 1.2.3.4:5001->5.6.7.8:9: connection reset by peer"),
	} {
		if !isSourceDisconnect(err) {
			t.Errorf("%v should classify as a source disconnect", err)
		}
	}
	for _, err := range []error{
		errors.New("block at 12345 out of device bounds"),
		errors.New("read hello: tls: bad certificate"),
		context.Canceled,
		nil,
	} {
		if isSourceDisconnect(err) {
			t.Errorf("%v should NOT classify as a source disconnect", err)
		}
	}
}
