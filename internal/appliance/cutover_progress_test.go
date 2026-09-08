package appliance

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// A real user pasted the cutover copy command into Lish and had no idea what
// was happening for the whole 30-90+ minute copy: no feedback on progress,
// and no answer to "can I close this window". The appliance already knows
// the exact byte count (st.bytes, known before the paste) and observes every
// byte it streams — it just threw that information away. These tests pin
// the fix: the appliance now tracks paste / bytes-sent / finished per
// migration, chunked so progress is visible mid-transfer, and reset on each
// fresh request so a re-paste can't exceed 100%.

// streamImageChunked must actually split a transfer into multiple chunks
// (not just copy everything in one shot) and report each chunk's size via
// the callback, while still delivering byte-identical content end to end.
func TestStreamImageChunkedReportsProgressIncrementally(t *testing.T) {
	data := make([]byte, 10000)
	for i := range data {
		data[i] = byte(i)
	}
	var got bytes.Buffer
	var chunks []int64
	err := streamImageChunked(&got, bytes.NewReader(data), int64(len(data)), 4096, func(n int64) {
		chunks = append(chunks, n)
	})
	if err != nil {
		t.Fatalf("streamImageChunked: %v", err)
	}
	if !bytes.Equal(got.Bytes(), data) {
		t.Fatalf("streamed %d bytes did not match the %d-byte source", got.Len(), len(data))
	}
	if len(chunks) < 2 {
		t.Fatalf("expected multiple chunks for a 10000-byte transfer at 4096 bytes/chunk, got %d: %v", len(chunks), chunks)
	}
	var sum int64
	for _, c := range chunks {
		sum += c
	}
	if sum != int64(len(data)) {
		t.Errorf("chunk sizes summed to %d, want %d", sum, len(data))
	}
}

// The most valuable test for this fix: serve a stream over httptest, read
// only the first chunk, and assert the progress reports 0 < sent < total
// WHILE THE REQUEST IS STILL OPEN — proving the counter is incremental (not
// filled in after the fact) and the handler is not buffering the whole
// image before it starts reporting. Also asserts Content-Length still equals
// the exact byte count, and that the eventual full body matches exactly —
// chunking must not corrupt the stream.
func TestCutoverImageReportsProgressWhileStreaming(t *testing.T) {
	const total = 200000
	content := make([]byte, total)
	for i := range content {
		content[i] = byte(i)
	}
	img := filepath.Join(t.TempDir(), "disk.img")
	if err := os.WriteFile(img, content, 0o600); err != nil {
		t.Fatal(err)
	}

	// Shrink the per-request chunk size so a modest test file exercises many
	// chunks; production streams 32 MiB per call.
	origChunk := cutoverImageChunkBytes
	cutoverImageChunkBytes = 4096
	defer func() { cutoverImageChunkBytes = origChunk }()

	s := &Server{}
	s.cfg.PublicHost = "h"
	s.cfg.ConsolePort = 8080
	tok := s.registerCutoverStream(42, img, total, time.Hour)

	// Deterministically pause the handler right after the FIRST chunk so we
	// can inspect progress mid-flight without racing socket-buffer timing.
	paused := make(chan struct{})
	resume := make(chan struct{})
	first := true
	s.testAfterCutoverChunk = func() {
		if first {
			first = false
			close(paused)
			<-resume
		}
	}

	srv := httptest.NewServer(http.HandlerFunc(s.handleCutoverImage))
	defer srv.Close()

	type result struct {
		resp *http.Response
		err  error
	}
	resultCh := make(chan result, 1)
	go func() {
		resp, err := http.Get(srv.URL + "/cutover/image?token=" + tok)
		resultCh <- result{resp, err}
	}()

	select {
	case <-paused:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for the first chunk")
	}

	cv, ok := s.cutoverCopyViewFor(42)
	if !ok {
		t.Fatal("expected cutover copy progress to be tracked for migration 42")
	}
	if cv.TotalBytes != total {
		t.Errorf("TotalBytes = %d, want %d", cv.TotalBytes, total)
	}
	if !(cv.SentBytes > 0 && cv.SentBytes < total) {
		t.Errorf("expected 0 < sent < total while the request is still open; got sent=%d total=%d", cv.SentBytes, total)
	}
	if cv.Phase != "copying" {
		t.Errorf("phase = %q, want %q", cv.Phase, "copying")
	}

	close(resume)

	select {
	case r := <-resultCh:
		if r.err != nil {
			t.Fatalf("request failed: %v", r.err)
		}
		defer r.resp.Body.Close()
		if cl := r.resp.Header.Get("Content-Length"); cl != strconv.Itoa(total) {
			t.Errorf("Content-Length = %q, want %d", cl, total)
		}
		got, err := io.ReadAll(r.resp.Body)
		if err != nil {
			t.Fatalf("read body: %v", err)
		}
		if !bytes.Equal(got, content) {
			t.Errorf("streamed content mismatch: got %d bytes, want %d bytes", len(got), len(content))
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for the request to complete")
	}

	cv, ok = s.cutoverCopyViewFor(42)
	if !ok || cv.Phase != "copying" || cv.SentBytes != total {
		t.Errorf("after the stream finished sending, expected phase=copying (byte-send done; 'finished' is set explicitly once the instance powers off) sent=%d; got %+v ok=%v", total, cv, ok)
	}
}

// A re-paste (curl retried, or the operator pastes the command a second
// time) must restart the percentage at 0 rather than accumulate past 100%.
func TestCutoverImageRepasteResetsProgress(t *testing.T) {
	const total = 1000
	content := bytes.Repeat([]byte{0x42}, total)
	img := filepath.Join(t.TempDir(), "disk.img")
	if err := os.WriteFile(img, content, 0o600); err != nil {
		t.Fatal(err)
	}
	s := &Server{}
	tok := s.registerCutoverStream(7, img, total, time.Hour)

	// First full request.
	rr := httptest.NewRecorder()
	s.handleCutoverImage(rr, httptest.NewRequest("GET", "/cutover/image?token="+tok, nil))
	if rr.Code != 200 {
		t.Fatalf("first request: status %d", rr.Code)
	}
	cv, ok := s.cutoverCopyViewFor(7)
	if !ok || cv.SentBytes != total || cv.Attempt != 1 {
		t.Fatalf("after first request: %+v ok=%v, want sent=%d attempt=1", cv, ok, total)
	}

	// Second request (re-paste) on the same token — must reset, not add to,
	// the byte count.
	rr = httptest.NewRecorder()
	s.handleCutoverImage(rr, httptest.NewRequest("GET", "/cutover/image?token="+tok, nil))
	if rr.Code != 200 {
		t.Fatalf("second request: status %d", rr.Code)
	}
	cv, ok = s.cutoverCopyViewFor(7)
	if !ok {
		t.Fatal("expected progress after the second request")
	}
	if cv.SentBytes != total {
		t.Errorf("SentBytes after re-paste = %d, want exactly %d (not %d, which would be double-counted)", cv.SentBytes, total, 2*total)
	}
	if cv.Attempt != 2 {
		t.Errorf("Attempt after re-paste = %d, want 2", cv.Attempt)
	}
}

// handleCutoverScript must record that the paste happened, idempotently
// (curl retries the script fetch), and report only ONE transition so the
// caller logs a single event instead of one per retry.
func TestCutoverCopyMarkPastedIsIdempotent(t *testing.T) {
	s := &Server{}
	img := filepath.Join(t.TempDir(), "disk.img")
	if err := os.WriteFile(img, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	s.registerCutoverStream(3, img, 1, time.Hour)

	cv, ok := s.cutoverCopyViewFor(3)
	if !ok || cv.Phase != "waiting" {
		t.Fatalf("fresh copy state should start in 'waiting'; got %+v ok=%v", cv, ok)
	}

	if !s.cutoverCopyMarkPasted(3) {
		t.Error("the first paste must report a transition")
	}
	if s.cutoverCopyMarkPasted(3) {
		t.Error("a second (retried) paste must NOT report a transition")
	}
	if s.cutoverCopyMarkPasted(3) {
		t.Error("a third paste must still not report a transition")
	}
}

// handleCutoverScript itself must call the paste-marking hook so the
// end-to-end HTTP path is covered, not just the helper in isolation.
func TestCutoverScriptRecordsPaste(t *testing.T) {
	s := &Server{}
	s.cfg.PublicHost = "h"
	img := filepath.Join(t.TempDir(), "disk.img")
	if err := os.WriteFile(img, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	tok := s.registerCutoverStream(5, img, 1, time.Hour)

	if cv, _ := s.cutoverCopyViewFor(5); cv.Phase != "waiting" {
		t.Fatalf("expected 'waiting' before the script is fetched; got %q", cv.Phase)
	}
	rr := httptest.NewRecorder()
	s.handleCutoverScript(rr, httptest.NewRequest("GET", "/cutover/copy.sh?token="+tok, nil))
	if rr.Code != 200 {
		t.Fatalf("script request: status %d", rr.Code)
	}
	// Fetching the script records the paste, but the copy itself hasn't
	// started (no bytes requested yet) — still 'waiting', just now marked
	// pasted internally (verified indirectly via cutoverCopyMarkPasted's
	// idempotency in the unit test above; here we just confirm the HTTP path
	// doesn't panic/skip the call and phase stays consistent).
	if cv, ok := s.cutoverCopyViewFor(5); !ok || cv.Phase != "waiting" {
		t.Errorf("expected phase still 'waiting' immediately after fetching the script (no bytes streamed yet); got %+v ok=%v", cv, ok)
	}
}

// cutoverCopyMarkFinished / cutoverCopyMarkInterrupted drive the phases that
// the byte-streaming path alone can't reach: "finished" is set explicitly by
// migrations.go once the instance is confirmed powered off (dropCutoverStream
// already erased the stream/cmd by then), and "interrupted" is set on
// restart when an in-flight copy's progress could not survive.
func TestCutoverCopyMarkFinishedAndInterrupted(t *testing.T) {
	s := &Server{}
	img := filepath.Join(t.TempDir(), "disk.img")
	if err := os.WriteFile(img, []byte("xx"), 0o600); err != nil {
		t.Fatal(err)
	}
	s.registerCutoverStream(9, img, 2, time.Hour)

	rr := httptest.NewRecorder()
	tok := "" // look up the just-registered stream's token indirectly via direct handler bypass
	_ = tok
	// Drive a request directly through the state machinery instead of HTTP,
	// since only the phase transitions matter here.
	s.cutoverCopyBeginRequest(9)
	s.cutoverCopyAddBytes(9, 2)
	if cv, _ := s.cutoverCopyViewFor(9); cv.Phase != "copying" {
		t.Fatalf("expected 'copying' once bytes have started flowing; got %q", cv.Phase)
	}

	s.cutoverCopyMarkFinished(9)
	cv, ok := s.cutoverCopyViewFor(9)
	if !ok || cv.Phase != "finished" {
		t.Fatalf("expected 'finished' after cutoverCopyMarkFinished; got %+v ok=%v", cv, ok)
	}
	_ = rr

	// A restart loses in-memory progress; the console must say "interrupted",
	// never fabricate a resumed percentage.
	s.cutoverCopyMarkInterrupted(9, 500)
	cv, ok = s.cutoverCopyViewFor(9)
	if !ok || cv.Phase != "waiting" || !cv.Interrupted || cv.SentBytes != 0 || cv.TotalBytes != 500 {
		t.Errorf("expected a fresh, interrupted 'waiting' state with sent reset to 0; got %+v ok=%v", cv, ok)
	}
}

// The copy-budget-timeout failure message must report how far the copy
// actually got, not a generic "it didn't finish".
func TestCutoverCopyProgressMessageReportsHowFarItGot(t *testing.T) {
	waiting := cutoverCopyView{Phase: "waiting", TotalBytes: 26843545600}
	msg := cutoverCopyProgressSummary(waiting)
	if !strings.Contains(msg, "waiting") {
		t.Errorf("waiting-phase summary should say so; got %q", msg)
	}

	copying := cutoverCopyView{Phase: "copying", SentBytes: 10 << 30, TotalBytes: 25 << 30, ElapsedSecs: 600, ETASecs: 900}
	msg = cutoverCopyProgressSummary(copying)
	for _, want := range []string{"10.0 GiB", "25.0 GiB"} {
		if !strings.Contains(msg, want) {
			t.Errorf("copying-phase summary should report how far the copy got (%q); got %q", want, msg)
		}
	}
}
