package appliance

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/tiny125/vm-replication/internal/linode"
)

// Audit logging. When a Linode token is saved we provision an Object Storage
// bucket; thereafter we keep an audit trail in the DB and periodically upload it
// to the bucket: "main.log" (every console action + system log) and one file per
// migration ("migrations/<id>-<name>.log") capturing everything the appliance
// did for that migration. Logs are kept authoritatively in SQLite and the full
// file is re-uploaded on change (S3 has no append), via Linode-minted presigned
// URLs so we never sign requests ourselves.

const (
	keyAuditBucket = "audit_bucket"      // JSON of linode.Bucket
	keyAuditName   = "audit_bucket_name" // stable chosen label
	keyAuditReady  = "audit_ready"       // "1" when the bucket exists
	keyAuditErr    = "audit_error"       // last provisioning error (for the console)
)

type auditEntry struct {
	migID         int64
	level, source string
	msg           string
}

var migIDRe = regexp.MustCompile(`migration (\d+)`)

// recordAudit queues an audit entry (non-blocking; drops if the buffer is full
// so logging never stalls the caller). It must never call log.* (the global
// logger is teed back into here).
func (s *Server) recordAudit(migID int64, level, source, msg string) {
	if s.auditCh == nil || msg == "" {
		return
	}
	select {
	case s.auditCh <- auditEntry{migID, level, source, msg}:
	default:
	}
}

func (s *Server) auditDrain() {
	for {
		select {
		case <-s.ctx.Done():
			return
		case e := <-s.auditCh:
			_ = s.st.AddAudit(s.ctx, e.migID, e.level, e.source, e.msg)
		}
	}
}

// StartAudit tees the process logger into the audit trail so system messages and
// errors are captured (and tagged to a migration when the line mentions one).
// Called from main once, since it changes the global logger.
func (s *Server) StartAudit() {
	log.SetOutput(io.MultiWriter(os.Stderr, auditLogWriter{s}))
}

type auditLogWriter struct{ s *Server }

func (w auditLogWriter) Write(p []byte) (int, error) {
	line := strings.TrimRight(string(p), "\n")
	if line != "" {
		var migID int64
		if m := migIDRe.FindStringSubmatch(line); m != nil {
			fmt.Sscan(m[1], &migID)
		}
		w.s.recordAudit(migID, "info", "system", line)
	}
	return len(p), nil
}

// auditAction records a console action (queued for the "main" log).
func (s *Server) auditAction(level, msg string) { s.recordAudit(0, level, "console", msg) }

// ---- bucket provisioning ----

// ensureAuditBucket creates (best-effort) the Object Storage bucket for audit
// logs and records the outcome in settings for the console to show.
func (s *Server) ensureAuditBucket(ctx context.Context, token string) {
	cl := linode.New(token)
	// Region precedence: an explicit -obj-region override, else the appliance's
	// OWN region (so the bucket sits with the appliance — e.g. a Singapore
	// appliance gets a Singapore bucket), else the -region default as a last
	// resort. (Previously this used -region directly, which defaults to us-ord,
	// so buckets wrongly landed in the US regardless of where the appliance ran.)
	region := s.cfg.ObjRegion
	if region == "" && s.cfg.ApplianceLinodeID != 0 {
		if inst, err := cl.GetInstance(ctx, s.cfg.ApplianceLinodeID); err == nil && inst.Region != "" {
			region = inst.Region
		}
	}
	if region == "" {
		region = s.cfg.Region
	}
	if region == "" {
		s.setAuditErr("no region known for the appliance; cannot place the audit bucket")
		return
	}

	// Reuse our previously-chosen bucket (stable across restarts), but only when
	// it uses the current "<prefix>-NN" scheme. Older deployments stored a name
	// without the -NN suffix and are migrated to a fresh NN on the next provision.
	prefix := s.auditPrefix()
	if name, ok, _ := s.st.GetSetting(ctx, keyAuditName); ok && isAuditName(prefix, name) {
		b, err := cl.CreateBucket(ctx, name, region)
		if err != nil && !isAlreadyExists(err) {
			s.setAuditErr(err.Error())
			return
		}
		if err != nil { // already exists = the bucket we made before
			b = linode.Bucket{Label: name, Region: region}
		}
		s.saveAuditBucket(ctx, b)
		return
	}

	// First provision (or migrating an old name): keep the appliance id and pick
	// the lowest free "<prefix>-NN", checking existing buckets across the account
	// so deployments never collide. Retry on a creation race.
	taken := map[string]bool{}
	if bs, err := cl.ListBuckets(ctx); err == nil {
		for _, b := range bs {
			taken[strings.ToLower(b.Label)] = true
		}
	}
	for i := 1; i <= 99; i++ {
		cand := fmt.Sprintf("%s-%02d", prefix, i)
		if taken[cand] {
			continue
		}
		b, err := cl.CreateBucket(ctx, cand, region)
		if err != nil {
			if isAlreadyExists(err) { // lost a race with another appliance; try next
				taken[cand] = true
				continue
			}
			s.setAuditErr(err.Error())
			return
		}
		_ = s.st.SetSetting(ctx, keyAuditName, cand)
		s.saveAuditBucket(ctx, b)
		return
	}
	s.setAuditErr("no free audit bucket name available (" + prefix + "-01..99 are all taken)")
}

// auditPrefix is the bucket-name prefix for this appliance: it keeps the
// appliance's Linode id (so buckets are tied to the instance) and a -NN suffix
// is added for uniqueness. Falls back to "vmrep-audit" with no id off-Linode.
func (s *Server) auditPrefix() string {
	if s.cfg.ApplianceLinodeID != 0 {
		return fmt.Sprintf("vmrep-audit-%d", s.cfg.ApplianceLinodeID)
	}
	return "vmrep-audit"
}

// isAuditName reports whether name is "<prefix>-NN" (1–3 digits).
func isAuditName(prefix, name string) bool {
	if !strings.HasPrefix(name, prefix+"-") {
		return false
	}
	suf := name[len(prefix)+1:]
	if len(suf) < 1 || len(suf) > 3 {
		return false
	}
	for _, c := range suf {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

func isAlreadyExists(err error) bool {
	return err != nil && strings.Contains(strings.ToLower(err.Error()), "exist")
}

func (s *Server) setAuditErr(msg string) {
	_ = s.st.SetSetting(s.ctx, keyAuditReady, "")
	_ = s.st.SetSetting(s.ctx, keyAuditErr, msg)
}

func (s *Server) saveAuditBucket(ctx context.Context, b linode.Bucket) {
	bj, _ := json.Marshal(b)
	_ = s.st.SetSetting(ctx, keyAuditBucket, string(bj))
	_ = s.st.SetSetting(ctx, keyAuditReady, "1")
	_ = s.st.SetSetting(ctx, keyAuditErr, "")
	s.auditAction("info", "audit log bucket ready: "+b.Label)
}

// auditBucket loads the provisioned bucket, ok=false if not ready.
func (s *Server) auditBucket(ctx context.Context) (linode.Bucket, bool) {
	if v, ok, _ := s.st.GetSetting(ctx, keyAuditReady); !ok || v != "1" {
		return linode.Bucket{}, false
	}
	raw, ok, _ := s.st.GetSetting(ctx, keyAuditBucket)
	if !ok {
		return linode.Bucket{}, false
	}
	var b linode.Bucket
	if json.Unmarshal([]byte(raw), &b) != nil || b.Label == "" {
		return linode.Bucket{}, false
	}
	return b, true
}

// ---- uploader ----

func (s *Server) auditUploader() {
	t := time.NewTicker(20 * time.Second)
	defer t.Stop()
	seen := map[int64]int64{} // stream -> last uploaded watermark
	for {
		select {
		case <-s.ctx.Done():
			s.flushAudit(seen)
			return
		case <-t.C:
			s.flushAudit(seen)
		}
	}
}

func (s *Server) flushAudit(seen map[int64]int64) {
	ctx := s.ctx
	b, ok := s.auditBucket(ctx)
	if !ok {
		return
	}
	cl, ok := s.linodeClient(ctx)
	if !ok {
		return
	}
	// Streams: 0 (console main) + every migration.
	ids := []int64{0}
	if migs, err := s.st.ListMigrations(ctx); err == nil {
		for _, m := range migs {
			ids = append(ids, m.ID)
		}
	}
	for _, id := range ids {
		wm, err := s.st.AuditWatermark(ctx, id)
		if err != nil || wm == seen[id] {
			continue
		}
		name, content := s.renderAudit(ctx, id)
		if content == "" {
			seen[id] = wm
			continue
		}
		if err := cl.PutObject(ctx, b, name, "text/plain; charset=utf-8", []byte(content)); err != nil {
			continue // leave watermark unchanged so we retry next tick
		}
		seen[id] = wm
	}
}

// renderAudit builds the object name and full text for a stream.
func (s *Server) renderAudit(ctx context.Context, migID int64) (name, content string) {
	type line struct {
		at            time.Time
		level, source string
		msg           string
	}
	var lines []line

	if migID > 0 {
		if evs, err := s.st.Events(ctx, migID, 100000); err == nil {
			for _, e := range evs { // Events is newest-first
				lines = append(lines, line{e.At, e.Level, "event", e.Message})
			}
		}
	}
	if rows, err := s.st.AuditRows(ctx, migID); err == nil {
		for _, r := range rows {
			lines = append(lines, line{r.At, r.Level, r.Source, r.Message})
		}
	}
	sort.SliceStable(lines, func(i, j int) bool { return lines[i].at.Before(lines[j].at) })

	var sb strings.Builder
	if migID == 0 {
		sb.WriteString("vm-replication — console & system audit log\n")
		sb.WriteString("Every console action and system message on this appliance.\n")
	} else {
		m, _ := s.st.Migration(ctx, migID)
		fmt.Fprintf(&sb, "vm-replication — migration #%d (%s) server log\n", migID, m.Name)
		sb.WriteString("Everything the appliance did for this migration.\n")
		name = fmt.Sprintf("migrations/%d-%s.log", migID, sanitizeName(m.Name))
	}
	if migID == 0 {
		name = "main.log"
	}
	fmt.Fprintf(&sb, "Generated %s\n\n", time.Now().UTC().Format(time.RFC3339))
	for _, l := range lines {
		src := l.source
		if src == "" {
			src = "info"
		}
		fmt.Fprintf(&sb, "%s [%s] (%s) %s\n", l.at.UTC().Format(time.RFC3339), l.level, src, l.msg)
	}
	return name, sb.String()
}

func sanitizeName(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteRune('-')
		}
	}
	out := b.String()
	if out == "" {
		out = "migration"
	}
	return out
}
