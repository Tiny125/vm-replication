package store

import (
	"context"
	"database/sql"
	"time"
)

// AuditRow is one entry in the audit trail.
type AuditRow struct {
	At      time.Time
	Level   string
	Source  string
	Message string
}

// AddAudit appends an audit entry. migrationID 0 is the global console log.
func (s *Store) AddAudit(ctx context.Context, migrationID int64, level, source, message string) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO audit_log (migration_id, at, level, source, message) VALUES (?, ?, ?, ?, ?)`,
		migrationID, unix(time.Now()), level, source, message)
	return err
}

// AuditRows returns a stream's audit entries in chronological order.
func (s *Store) AuditRows(ctx context.Context, migrationID int64) ([]AuditRow, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT at, level, source, message FROM audit_log WHERE migration_id=? ORDER BY id`, migrationID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AuditRow
	for rows.Next() {
		var r AuditRow
		var at int64
		if err := rows.Scan(&at, &r.Level, &r.Source, &r.Message); err != nil {
			return nil, err
		}
		r.At = fromUnix(at)
		out = append(out, r)
	}
	return out, rows.Err()
}

// AuditWatermark returns a value that changes whenever a stream gains a new
// event or audit entry, so the uploader can detect what to re-upload without a
// separate dirty flag. migrationID 0 (the console log) has no events.
func (s *Store) AuditWatermark(ctx context.Context, migrationID int64) (int64, error) {
	var ev, au sql.NullInt64
	err := s.db.QueryRowContext(ctx,
		`SELECT (SELECT MAX(id) FROM migration_events WHERE migration_id=?),
		        (SELECT MAX(id) FROM audit_log WHERE migration_id=?)`,
		migrationID, migrationID).Scan(&ev, &au)
	if err != nil {
		return 0, err
	}
	return ev.Int64*1_000_000_000 + au.Int64, nil
}
