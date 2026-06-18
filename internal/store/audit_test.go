package store

import (
	"context"
	"testing"
)

func TestAuditLog(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	wm0, err := st.AuditWatermark(ctx, 0)
	if err != nil {
		t.Fatalf("AuditWatermark: %v", err)
	}
	if wm0 != 0 {
		t.Fatalf("empty watermark = %d, want 0", wm0)
	}

	if err := st.AddAudit(ctx, 0, "info", "console", "admin logged in"); err != nil {
		t.Fatalf("AddAudit: %v", err)
	}
	if err := st.AddAudit(ctx, 0, "warn", "console", "POST /x -> 400"); err != nil {
		t.Fatalf("AddAudit: %v", err)
	}
	// A different stream must not bleed into stream 0.
	_ = st.AddAudit(ctx, 7, "info", "system", "migration 7: started")

	rows, err := st.AuditRows(ctx, 0)
	if err != nil {
		t.Fatalf("AuditRows: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("stream 0 rows = %d, want 2", len(rows))
	}
	if rows[0].Message != "admin logged in" || rows[1].Source != "console" {
		t.Fatalf("unexpected rows: %+v", rows)
	}

	wm0b, _ := st.AuditWatermark(ctx, 0)
	if wm0b == wm0 {
		t.Fatal("watermark did not advance after adding entries")
	}
	if wm7, _ := st.AuditWatermark(ctx, 7); wm7 == 0 {
		t.Fatal("stream 7 watermark should be non-zero")
	}
}
