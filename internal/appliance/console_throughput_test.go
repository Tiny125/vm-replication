package appliance

import (
	"strings"
	"testing"
)

// A real user, migrating AWS -> Linode, saw the throughput line render as
// "4.3143739454258046e-19 B/s". Two independent bugs combined to produce it:
//
//  1. fmtBytes was DEFINED TWICE in the same <script> block (console.go had
//     one definition near the top and a second, later one at the bottom).
//     JS function-declaration hoisting means the SECOND one wins everywhere,
//     and that second definition returned the raw Number (not a fixed
//     string) for the "B" unit, which JS renders in exponential notation for
//     a tiny sub-1 value.
//  2. replSpeed decayed a stale EMA (exponential moving average) forever once
//     bytes_on_wire froze at cutover — the "migrating" state takes the
//     fallback branch, computes a zero instantaneous rate every poll, and
//     halves the EMA each time without ever resetting it, so after ~88 polls
//     it reaches ~1e-19.
//
// These tests pin both fixes, plus a permanent guard against the general
// hoisting-collision pattern (this is the SECOND such bug in this console —
// a .info CSS class collision was the first).

// Guard against the whole class of "defined twice, hoisting picks the wrong
// one" bugs: fmtBytes must be defined exactly once in the console's script.
func TestFmtBytesDefinedExactlyOnce(t *testing.T) {
	n := strings.Count(consoleHTML, "function fmtBytes(")
	if n != 1 {
		t.Errorf("fmtBytes must be defined exactly once in the console script (JS hoisting makes the LAST definition win everywhere); found %d definitions", n)
	}
}

// The surviving fmtBytes must be hardened: coerce to Number, treat
// non-finite/<=0 as '0 B', and use toFixed(1) unconditionally so no branch
// can ever return a raw Number (which JS prints in exponential notation for
// tiny values, e.g. fmtBytes(4.3143739454258046e-19) -> "4.3143739454258046e-19
// B"). It must also keep IEC units (KiB/MiB/GiB) — every value fed to it is a
// binary byte count — not the decimal KB/MB/GB the second, wrong definition
// used (which is what the user actually saw: "GB").
func TestFmtBytesHardened(t *testing.T) {
	fn := extractJSFunc(t, "function fmtBytes(n){")
	if !strings.Contains(fn, "Number(n)") {
		t.Error("fmtBytes must coerce its argument with Number(n) so a non-numeric input can't slip through")
	}
	// The exact bug pattern: returning the raw number instead of a fixed
	// string for the smallest unit.
	if strings.Contains(fn, "i===0?n:") {
		t.Error("fmtBytes must not special-case the smallest unit to return a raw Number — that's what rendered 4.3143739454258046e-19 B")
	}
	for _, unit := range []string{"KiB", "MiB", "GiB"} {
		if !strings.Contains(fn, unit) {
			t.Errorf("fmtBytes must use IEC units (%q) — every byte count it receives is binary, not decimal", unit)
		}
	}
	for _, decimal := range []string{"'KB'", "'MB'", "'GB'"} {
		if strings.Contains(fn, decimal) {
			t.Errorf("fmtBytes must not use decimal unit %q — that's the wrong (duplicate) definition's units, and what the user actually saw", decimal)
		}
	}
}

// LIVE_REPL_STATES must be a single hoisted constant, not written out
// separately at each use site. It was triplicated (rpoText, pillFor, the
// migCard actions row) before this fix; replSpeed is a fourth site that
// needs the exact same list to fix the EMA-decay bug below, which would have
// made it a quadruplication if not hoisted.
func TestLiveReplicationStatesAreHoistedNotTriplicated(t *testing.T) {
	if !strings.Contains(consoleHTML, "LIVE_REPL_STATES") {
		t.Fatal("expected a shared LIVE_REPL_STATES constant")
	}
	n := strings.Count(consoleHTML, "['created','awaiting_agent','replicating','ready']")
	if n != 1 {
		t.Errorf("the live-replication state list literal must appear exactly once (its definition); found %d occurrences — every use site should reference the shared constant instead", n)
	}
	for _, fnHeader := range []string{
		"function rpoText(v,m){",
		"function pillFor(v,m){",
		"function migCard(v){",
		"function replSpeed(v,m){",
	} {
		fn := extractJSFunc(t, fnHeader)
		if !strings.Contains(fn, "LIVE_REPL_STATES") {
			t.Errorf("%s must reference the shared LIVE_REPL_STATES constant", fnHeader)
		}
	}
}

// replSpeed must not decay a stale EMA forever once a migration leaves live
// replication (e.g. "migrating", where bytes_on_wire is frozen because the
// receivers were drained at cutover and percent_done is -1, so every poll
// computed a zero instantaneous rate and halved the EMA without ever
// resetting — reaching ~1e-19 after ~88 polls, measured live). It must reset
// the sample and report "unknown" (-1) the moment the migration isn't in a
// live-replication state, and floor the EMA so even within a live state it
// can't decay to a meaningless near-zero value instead of just resetting.
func TestReplSpeedResetsOutsideLiveReplication(t *testing.T) {
	fn := extractJSFunc(t, "function replSpeed(v,m){")
	if !strings.Contains(fn, "LIVE_REPL_STATES") {
		t.Fatal("replSpeed must gate on LIVE_REPL_STATES")
	}
	if !strings.Contains(fn, "includes(m.state)") {
		t.Error("replSpeed must check the migration's live-replication membership")
	}
	if !strings.Contains(fn, "delete speedSamples[m.id]") {
		t.Error("replSpeed must clear the stale sample when the migration is not (or no longer) in a live-replication state, or a later resume would resample from stale bytes/time")
	}
	// A floor below which a decayed EMA reports unknown rather than a
	// vanishingly small number.
	if !strings.Contains(fn, "s.ema<") && !strings.Contains(fn, "s.ema >") {
		t.Error("replSpeed should floor the EMA (report -1 below some minimum) rather than let it decay toward zero forever")
	}
}

// The throughput RATE (bytes/sec) must only ever be shown while a live copy
// is actually being measured — never in awaiting_cutover, migrating,
// image_ready, launched or failed, where today the "speed" is either stale
// or (for image_ready/launched) a meaningless total-size/cutover-duration
// average. Once replSpeed (tested above) returns -1 outside
// LIVE_REPL_STATES, progressLine's existing "if(bps>=0)" guard naturally
// suppresses the rate in exactly those states — this test pins that the
// guard is still there so a future edit can't drop it.
func TestProgressLineOnlyShowsRateWhenPositive(t *testing.T) {
	fn := extractJSFunc(t, "function progressLine(v,m){")
	if !strings.Contains(fn, "if(bps>=0)") {
		t.Error("progressLine must only append the '.../s' rate when bps>=0 (replSpeed's -1 sentinel for 'not measurable')")
	}
}
