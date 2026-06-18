// Package store is the control plane's state layer, backed by SQLite via the
// pure-Go modernc.org/sqlite driver (no cgo, so controld still builds as a
// static binary). It holds the server inventory, replication jobs, and sync
// history, and computes the RPO/lag view used by the dashboard and metrics.
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/tiny125/vm-replication/internal/api"

	_ "modernc.org/sqlite"
)

// ErrNotFound is returned when a requested row does not exist.
var ErrNotFound = errors.New("store: not found")

// Store wraps a SQLite database.
type Store struct {
	db *sql.DB
}

// Open opens (creating if needed) the SQLite database at path and applies the
// schema. Use ":memory:" for tests.
func Open(path string) (*Store, error) {
	dsn := path
	if path != ":memory:" {
		// WAL + busy timeout make the single-writer control plane robust under
		// concurrent agent reports.
		dsn = fmt.Sprintf("file:%s?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)", path)
	}
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	// SQLite: serialize access on one connection to avoid "database is locked".
	// This is fine for a modest fleet; status/metrics use N+1 per-job queries, so
	// for large fleets move to set-based aggregate SQL and allow WAL read
	// concurrency before this single connection becomes a bottleneck.
	db.SetMaxOpenConns(1)
	s := &Store{db: db}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

// Close closes the underlying database.
func (s *Store) Close() error { return s.db.Close() }

func (s *Store) migrate() error {
	const schema = `
CREATE TABLE IF NOT EXISTS servers (
  id        INTEGER PRIMARY KEY AUTOINCREMENT,
  name      TEXT NOT NULL UNIQUE,
  role      TEXT NOT NULL,
  hostname  TEXT NOT NULL DEFAULT '',
  address   TEXT NOT NULL DEFAULT '',
  device    TEXT NOT NULL DEFAULT '',
  disk_size INTEGER NOT NULL DEFAULT 0,
  created_at INTEGER NOT NULL,
  last_seen  INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS jobs (
  id          INTEGER PRIMARY KEY AUTOINCREMENT,
  name        TEXT NOT NULL UNIQUE,
  state       TEXT NOT NULL,
  source_id   INTEGER REFERENCES servers(id),
  target_id   INTEGER REFERENCES servers(id),
  target_addr TEXT NOT NULL DEFAULT '',
  device      TEXT NOT NULL DEFAULT '',
  block_size  INTEGER NOT NULL DEFAULT 0,
  rpo_target  INTEGER NOT NULL DEFAULT 0,
  created_at  INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS syncs (
  id            INTEGER PRIMARY KEY AUTOINCREMENT,
  job_id        INTEGER NOT NULL REFERENCES jobs(id) ON DELETE CASCADE,
  mode          TEXT NOT NULL,
  started_at    INTEGER NOT NULL,
  finished_at   INTEGER NOT NULL,
  total_blocks  INTEGER NOT NULL,
  changed_blocks INTEGER NOT NULL,
  bytes_on_wire INTEGER NOT NULL,
  duration_ms   INTEGER NOT NULL,
  ok            INTEGER NOT NULL,
  error         TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS idx_syncs_job ON syncs(job_id, id DESC);

-- Appliance settings: admin password hash, encrypted Linode token, etc.
CREATE TABLE IF NOT EXISTS settings (
  key   TEXT PRIMARY KEY,
  value TEXT NOT NULL
);

-- Appliance migrations: the turnkey, console-driven unit of work.
CREATE TABLE IF NOT EXISTS migrations (
  id              INTEGER PRIMARY KEY AUTOINCREMENT,
  name            TEXT NOT NULL UNIQUE,
  state           TEXT NOT NULL,
  source_hostname TEXT NOT NULL DEFAULT '',
  source_ip       TEXT NOT NULL DEFAULT '',
  source_device   TEXT NOT NULL DEFAULT '',
  source_disk_size INTEGER NOT NULL DEFAULT 0,
  enroll_token    TEXT NOT NULL,
  receiver_port   INTEGER NOT NULL DEFAULT 0,
  volume_id       INTEGER NOT NULL DEFAULT 0,
  volume_device   TEXT NOT NULL DEFAULT '',
  image_id        TEXT NOT NULL DEFAULT '',
  launched_id     INTEGER NOT NULL DEFAULT 0,
  agent_last_seen INTEGER NOT NULL DEFAULT 0,
  full_sync_done  INTEGER NOT NULL DEFAULT 0,
  total_blocks    INTEGER NOT NULL DEFAULT 0,
  changed_blocks  INTEGER NOT NULL DEFAULT 0,
  bytes_on_wire   INTEGER NOT NULL DEFAULT 0,
  last_sync_at    INTEGER NOT NULL DEFAULT 0,
  last_error      TEXT NOT NULL DEFAULT '',
  assessed_at     INTEGER NOT NULL DEFAULT 0,
  migrate_started INTEGER NOT NULL DEFAULT 0,
  migrate_finished INTEGER NOT NULL DEFAULT 0,
  boot_target     TEXT NOT NULL DEFAULT 'volume',
  plan_class      TEXT NOT NULL DEFAULT '',
  linode_type     TEXT NOT NULL DEFAULT '',
  created_at      INTEGER NOT NULL
);

-- One row per source disk in a migration (boot disk has idx 0).
CREATE TABLE IF NOT EXISTS migration_disks (
  id              INTEGER PRIMARY KEY AUTOINCREMENT,
  migration_id    INTEGER NOT NULL REFERENCES migrations(id) ON DELETE CASCADE,
  idx             INTEGER NOT NULL,
  source_device   TEXT NOT NULL,
  size_bytes      INTEGER NOT NULL DEFAULT 0,
  receiver_port   INTEGER NOT NULL DEFAULT 0,
  volume_id       INTEGER NOT NULL DEFAULT 0,
  volume_device   TEXT NOT NULL DEFAULT '',
  artifact_id     TEXT NOT NULL DEFAULT '',
  full_sync_done  INTEGER NOT NULL DEFAULT 0,
  total_blocks    INTEGER NOT NULL DEFAULT 0,
  changed_blocks  INTEGER NOT NULL DEFAULT 0,
  bytes_on_wire   INTEGER NOT NULL DEFAULT 0,
  last_sync_at    INTEGER NOT NULL DEFAULT 0,
  agent_last_seen INTEGER NOT NULL DEFAULT 0,
  last_error      TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS idx_disks_migration ON migration_disks(migration_id, idx);

-- Per-migration activity log shown in the console.
CREATE TABLE IF NOT EXISTS migration_events (
  id           INTEGER PRIMARY KEY AUTOINCREMENT,
  migration_id INTEGER NOT NULL REFERENCES migrations(id) ON DELETE CASCADE,
  at           INTEGER NOT NULL,
  level        TEXT NOT NULL DEFAULT 'info',
  message      TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_events_migration ON migration_events(migration_id, id DESC);

-- Audit trail uploaded to Linode Object Storage. migration_id 0 is the global
-- console log ("main"); >0 is a per-migration server log.
CREATE TABLE IF NOT EXISTS audit_log (
  id           INTEGER PRIMARY KEY AUTOINCREMENT,
  migration_id INTEGER NOT NULL DEFAULT 0,
  at           INTEGER NOT NULL,
  level        TEXT NOT NULL DEFAULT 'info',
  source       TEXT NOT NULL DEFAULT '',
  message      TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_audit_migration ON audit_log(migration_id, id);
`
	if _, err := s.db.Exec(schema); err != nil {
		return err
	}
	// Upgrade pre-existing databases: ADD COLUMN fails with "duplicate column"
	// when already applied, which is safe to ignore (SQLite has no IF NOT EXISTS
	// for columns).
	for _, stmt := range []string{
		`ALTER TABLE migrations ADD COLUMN source_ip TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE migrations ADD COLUMN assessed_at INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE migrations ADD COLUMN migrate_started INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE migrations ADD COLUMN migrate_finished INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE migrations ADD COLUMN boot_target TEXT NOT NULL DEFAULT 'volume'`,
		`ALTER TABLE migrations ADD COLUMN plan_class TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE migrations ADD COLUMN linode_type TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE migration_disks ADD COLUMN last_error TEXT NOT NULL DEFAULT ''`,
	} {
		_, _ = s.db.Exec(stmt)
	}
	return nil
}

func unix(t time.Time) int64 { return t.UTC().Unix() }
func fromUnix(n int64) time.Time {
	if n == 0 {
		return time.Time{}
	}
	return time.Unix(n, 0).UTC()
}

// ---- servers ----

// UpsertServer registers a server by name or updates the existing one. It
// always refreshes last_seen, doubling as a heartbeat.
func (s *Store) UpsertServer(ctx context.Context, r api.RegisterServerRequest) (api.Server, error) {
	now := time.Now()
	_, err := s.db.ExecContext(ctx, `
INSERT INTO servers (name, role, hostname, address, device, disk_size, created_at, last_seen)
VALUES (?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(name) DO UPDATE SET
  role=excluded.role, hostname=excluded.hostname, address=excluded.address,
  device=excluded.device, disk_size=excluded.disk_size, last_seen=excluded.last_seen`,
		r.Name, string(r.Role), r.Hostname, r.Address, r.Device, r.DiskSize, unix(now), unix(now))
	if err != nil {
		return api.Server{}, err
	}
	return s.ServerByName(ctx, r.Name)
}

func scanServer(row interface{ Scan(...any) error }) (api.Server, error) {
	var sv api.Server
	var role string
	var created, seen int64
	if err := row.Scan(&sv.ID, &sv.Name, &role, &sv.Hostname, &sv.Address, &sv.Device, &sv.DiskSize, &created, &seen); err != nil {
		return api.Server{}, err
	}
	sv.Role = api.Role(role)
	sv.CreatedAt, sv.LastSeen = fromUnix(created), fromUnix(seen)
	return sv, nil
}

const serverCols = `id, name, role, hostname, address, device, disk_size, created_at, last_seen`

// ServerByName looks up a server by its unique name.
func (s *Store) ServerByName(ctx context.Context, name string) (api.Server, error) {
	row := s.db.QueryRowContext(ctx, `SELECT `+serverCols+` FROM servers WHERE name=?`, name)
	sv, err := scanServer(row)
	if errors.Is(err, sql.ErrNoRows) {
		return api.Server{}, ErrNotFound
	}
	return sv, err
}

// Server looks up a server by id.
func (s *Store) Server(ctx context.Context, id int64) (api.Server, error) {
	row := s.db.QueryRowContext(ctx, `SELECT `+serverCols+` FROM servers WHERE id=?`, id)
	sv, err := scanServer(row)
	if errors.Is(err, sql.ErrNoRows) {
		return api.Server{}, ErrNotFound
	}
	return sv, err
}

// ListServers returns all servers ordered by name.
func (s *Store) ListServers(ctx context.Context) ([]api.Server, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+serverCols+` FROM servers ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []api.Server
	for rows.Next() {
		sv, err := scanServer(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, sv)
	}
	return out, rows.Err()
}

// ---- jobs ----

const jobCols = `id, name, state, source_id, target_id, target_addr, device, block_size, rpo_target, created_at`

func scanJob(row interface{ Scan(...any) error }) (api.Job, error) {
	var j api.Job
	var state string
	var srcID, tgtID sql.NullInt64
	var created int64
	if err := row.Scan(&j.ID, &j.Name, &state, &srcID, &tgtID, &j.TargetAddr, &j.Device, &j.BlockSize, &j.RPOTargetSec, &created); err != nil {
		return api.Job{}, err
	}
	j.State = api.JobState(state)
	j.SourceServerID, j.TargetServerID = srcID.Int64, tgtID.Int64
	j.CreatedAt = fromUnix(created)
	return j, nil
}

// CreateJob inserts a new replication job.
func (s *Store) CreateJob(ctx context.Context, r api.CreateJobRequest) (api.Job, error) {
	now := time.Now()
	res, err := s.db.ExecContext(ctx, `
INSERT INTO jobs (name, state, source_id, target_id, target_addr, device, block_size, rpo_target, created_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		r.Name, string(api.JobActive), nullID(r.SourceServerID), nullID(r.TargetServerID),
		r.TargetAddr, r.Device, r.BlockSize, r.RPOTargetSec, unix(now))
	if err != nil {
		return api.Job{}, err
	}
	id, _ := res.LastInsertId()
	return s.Job(ctx, id)
}

func nullID(id int64) any {
	if id == 0 {
		return nil
	}
	return id
}

// Job looks up a job by id.
func (s *Store) Job(ctx context.Context, id int64) (api.Job, error) {
	row := s.db.QueryRowContext(ctx, `SELECT `+jobCols+` FROM jobs WHERE id=?`, id)
	j, err := scanJob(row)
	if errors.Is(err, sql.ErrNoRows) {
		return api.Job{}, ErrNotFound
	}
	return j, err
}

// ListJobs returns all jobs ordered by id.
func (s *Store) ListJobs(ctx context.Context) ([]api.Job, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+jobCols+` FROM jobs ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []api.Job
	for rows.Next() {
		j, err := scanJob(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, j)
	}
	return out, rows.Err()
}

// SetJobState updates a job's lifecycle state.
func (s *Store) SetJobState(ctx context.Context, id int64, state api.JobState) error {
	res, err := s.db.ExecContext(ctx, `UPDATE jobs SET state=? WHERE id=?`, string(state), id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// ---- syncs ----

const syncCols = `id, job_id, mode, started_at, finished_at, total_blocks, changed_blocks, bytes_on_wire, duration_ms, ok, error`

func scanSync(row interface{ Scan(...any) error }) (api.Sync, error) {
	var sy api.Sync
	var mode string
	var started, finished int64
	var ok int
	if err := row.Scan(&sy.ID, &sy.JobID, &mode, &started, &finished, &sy.TotalBlocks, &sy.ChangedBlocks, &sy.BytesOnWire, &sy.DurationMS, &ok, &sy.Error); err != nil {
		return api.Sync{}, err
	}
	sy.Mode = api.SyncMode(mode)
	sy.StartedAt, sy.FinishedAt = fromUnix(started), fromUnix(finished)
	sy.OK = ok != 0
	return sy, nil
}

// AddSync records a completed replication pass for a job.
func (s *Store) AddSync(ctx context.Context, jobID int64, r api.ReportSyncRequest) (api.Sync, error) {
	if _, err := s.Job(ctx, jobID); err != nil {
		return api.Sync{}, err
	}
	ok := 0
	if r.OK {
		ok = 1
	}
	res, err := s.db.ExecContext(ctx, `
INSERT INTO syncs (job_id, mode, started_at, finished_at, total_blocks, changed_blocks, bytes_on_wire, duration_ms, ok, error)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		jobID, string(r.Mode), unix(r.StartedAt), unix(r.FinishedAt), r.TotalBlocks, r.ChangedBlocks,
		r.BytesOnWire, r.DurationMS, ok, r.Error)
	if err != nil {
		return api.Sync{}, err
	}
	id, _ := res.LastInsertId()
	row := s.db.QueryRowContext(ctx, `SELECT `+syncCols+` FROM syncs WHERE id=?`, id)
	return scanSync(row)
}

// ListSyncs returns the most recent syncs for a job (newest first).
func (s *Store) ListSyncs(ctx context.Context, jobID int64, limit int) ([]api.Sync, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := s.db.QueryContext(ctx, `SELECT `+syncCols+` FROM syncs WHERE job_id=? ORDER BY id DESC LIMIT ?`, jobID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []api.Sync
	for rows.Next() {
		sy, err := scanSync(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, sy)
	}
	return out, rows.Err()
}

func (s *Store) lastSync(ctx context.Context, jobID int64, onlyOK bool) (*api.Sync, error) {
	q := `SELECT ` + syncCols + ` FROM syncs WHERE job_id=?`
	if onlyOK {
		q += ` AND ok=1`
	}
	q += ` ORDER BY id DESC LIMIT 1`
	row := s.db.QueryRowContext(ctx, q, jobID)
	sy, err := scanSync(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &sy, nil
}

// JobStatus computes the health/RPO view for a single job.
func (s *Store) JobStatus(ctx context.Context, j api.Job, now time.Time) (api.JobStatus, error) {
	st := api.JobStatus{Job: j}

	last, err := s.lastSync(ctx, j.ID, false)
	if err != nil {
		return st, err
	}
	st.LastSync = last

	okSync, err := s.lastSync(ctx, j.ID, true)
	if err != nil {
		return st, err
	}
	st.LastOKSync = okSync

	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM syncs WHERE job_id=?`, j.ID).Scan(&st.TotalSyncs); err != nil {
		return st, err
	}

	if okSync != nil {
		st.RPOSeconds = now.Sub(okSync.FinishedAt).Seconds()
		if st.RPOSeconds < 0 {
			st.RPOSeconds = 0
		}
		st.RPOBreached = j.RPOTargetSec > 0 && st.RPOSeconds > float64(j.RPOTargetSec)
	} else {
		// No successful sync yet: treat as breached if a target is set.
		st.RPOBreached = j.RPOTargetSec > 0
	}

	if j.SourceServerID != 0 {
		if sv, err := s.Server(ctx, j.SourceServerID); err == nil {
			st.Source = &sv
		}
	}
	if j.TargetServerID != 0 {
		if sv, err := s.Server(ctx, j.TargetServerID); err == nil {
			st.Target = &sv
		}
	}
	return st, nil
}

// AllJobStatuses returns the health view for every job.
func (s *Store) AllJobStatuses(ctx context.Context) ([]api.JobStatus, error) {
	jobs, err := s.ListJobs(ctx)
	if err != nil {
		return nil, err
	}
	now := time.Now()
	out := make([]api.JobStatus, 0, len(jobs))
	for _, j := range jobs {
		st, err := s.JobStatus(ctx, j, now)
		if err != nil {
			return nil, err
		}
		out = append(out, st)
	}
	return out, nil
}
