package store

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/tiny125/vm-replication/internal/api"
)

// ---- settings (key/value) ----

// GetSetting returns a setting value, or "" with ok=false if absent.
func (s *Store) GetSetting(ctx context.Context, key string) (string, bool, error) {
	var v string
	err := s.db.QueryRowContext(ctx, `SELECT value FROM settings WHERE key=?`, key).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return v, true, nil
}

// SetSetting upserts a setting.
func (s *Store) SetSetting(ctx context.Context, key, value string) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO settings(key,value) VALUES(?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value`,
		key, value)
	return err
}

// ---- admin password ----

const (
	keyAdminSalt  = "admin_pw_salt"
	keyAdminHash  = "admin_pw_hash"
	keyLinodeTok  = "linode_token_enc"
	keyLinodeAcct = "linode_account" // human label of the account the token belongs to
)

// hashPassword returns hex(sha256(salt || password)) iterated to slow brute
// force. The admin password is a high-entropy generated secret, so this is
// sufficient without an external KDF dependency.
func hashPassword(salt, password string) string {
	h := []byte(salt + password)
	for i := 0; i < 100_000; i++ {
		sum := sha256.Sum256(h)
		h = sum[:]
	}
	return hex.EncodeToString(h)
}

// AdminPasswordSet reports whether an admin password has been configured.
func (s *Store) AdminPasswordSet(ctx context.Context) (bool, error) {
	_, ok, err := s.GetSetting(ctx, keyAdminHash)
	return ok, err
}

// SetAdminPassword stores a (salted, hashed) admin password.
func (s *Store) SetAdminPassword(ctx context.Context, password string) error {
	saltBytes := make([]byte, 16)
	if _, err := rand.Read(saltBytes); err != nil {
		return err
	}
	salt := hex.EncodeToString(saltBytes)
	if err := s.SetSetting(ctx, keyAdminSalt, salt); err != nil {
		return err
	}
	return s.SetSetting(ctx, keyAdminHash, hashPassword(salt, password))
}

// VerifyAdminPassword reports whether password matches the stored hash.
func (s *Store) VerifyAdminPassword(ctx context.Context, password string) (bool, error) {
	salt, ok, err := s.GetSetting(ctx, keyAdminSalt)
	if err != nil || !ok {
		return false, err
	}
	want, ok, err := s.GetSetting(ctx, keyAdminHash)
	if err != nil || !ok {
		return false, err
	}
	got := hashPassword(salt, password)
	return subtle.ConstantTimeCompare([]byte(got), []byte(want)) == 1, nil
}

// ---- Linode token (encrypted at rest) ----

// encKey is the AES key used to encrypt the Linode token. It is set by the
// daemon from a key file kept outside the database (mode 0600).
var encKey []byte

// SetEncryptionKey installs the at-rest encryption key (32 bytes for AES-256).
func (s *Store) SetEncryptionKey(key []byte) { encKey = key }

func aesGCM() (cipher.AEAD, error) {
	if len(encKey) != 32 {
		return nil, fmt.Errorf("store: encryption key not set (need 32 bytes)")
	}
	block, err := aes.NewCipher(encKey)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

// SetLinodeToken encrypts and stores the Linode API token.
func (s *Store) SetLinodeToken(ctx context.Context, token string) error {
	gcm, err := aesGCM()
	if err != nil {
		return err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return err
	}
	ct := gcm.Seal(nonce, nonce, []byte(token), nil)
	return s.SetSetting(ctx, keyLinodeTok, hex.EncodeToString(ct))
}

// LinodeToken decrypts and returns the stored Linode API token.
func (s *Store) LinodeToken(ctx context.Context) (string, error) {
	enc, ok, err := s.GetSetting(ctx, keyLinodeTok)
	if err != nil {
		return "", err
	}
	if !ok {
		return "", ErrNotFound
	}
	gcm, err := aesGCM()
	if err != nil {
		return "", err
	}
	raw, err := hex.DecodeString(enc)
	if err != nil {
		return "", err
	}
	ns := gcm.NonceSize()
	if len(raw) < ns {
		return "", fmt.Errorf("store: corrupt linode token ciphertext")
	}
	pt, err := gcm.Open(nil, raw[:ns], raw[ns:], nil)
	if err != nil {
		return "", fmt.Errorf("store: decrypt linode token: %w", err)
	}
	return string(pt), nil
}

// LinodeTokenSet reports whether a Linode token has been stored.
func (s *Store) LinodeTokenSet(ctx context.Context) (bool, error) {
	_, ok, err := s.GetSetting(ctx, keyLinodeTok)
	return ok, err
}

// SetLinodeAccount records a human label of the account the token belongs to
// (e.g. "alice <alice@example.com>"), shown in the console.
func (s *Store) SetLinodeAccount(ctx context.Context, account string) error {
	return s.SetSetting(ctx, keyLinodeAcct, account)
}

// LinodeAccount returns the stored account label (empty if unset).
func (s *Store) LinodeAccount(ctx context.Context) (string, error) {
	v, _, err := s.GetSetting(ctx, keyLinodeAcct)
	return v, err
}

// ---- migrations ----

// migCols selects only the migration-level fields; per-disk state lives in
// migration_disks and is loaded separately. (The legacy single-disk columns on
// the migrations table remain for older databases but are no longer read.)
const migCols = `id, name, state, source_hostname, source_ip, source_device, source_disk_size,
 image_id, launched_id, last_error, assessed_at, migrate_started, migrate_finished,
 boot_target, plan_class, linode_type, replication_enabled, enrolled_at, created_at`

func scanMigration(row interface{ Scan(...any) error }) (api.Migration, error) {
	var m api.Migration
	var state string
	var assessed, migStart, migFinish, enrolled, created int64
	var replEnabled int
	if err := row.Scan(&m.ID, &m.Name, &state, &m.SourceHostname, &m.SourceIP, &m.SourceDevice, &m.SourceDiskSize,
		&m.ImageID, &m.LaunchedID, &m.LastError, &assessed, &migStart, &migFinish,
		&m.BootTarget, &m.PlanClass, &m.LinodeType, &replEnabled, &enrolled, &created); err != nil {
		return api.Migration{}, err
	}
	m.State = api.MigrationState(state)
	m.AssessedAt = fromUnix(assessed)
	m.MigrateStarted = fromUnix(migStart)
	m.MigrateFinished = fromUnix(migFinish)
	m.ReplicationEnabled = replEnabled != 0
	m.EnrolledAt = fromUnix(enrolled)
	m.CreatedAt = fromUnix(created)
	return m, nil
}

func randToken(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// CreateMigration inserts a migration plus one row per source disk (boot disk
// first) and returns the migration (with disks) and its enrollment token. The
// caller assigns receiver ports per disk after insert (AssignDiskPort).
func (s *Store) CreateMigration(ctx context.Context, r api.CreateMigrationRequest) (api.Migration, string, error) {
	token, err := randToken(24)
	if err != nil {
		return api.Migration{}, "", err
	}
	devices := r.Devices
	if len(devices) == 0 && r.SourceDevice != "" { // single-disk back-compat
		devices = []api.DeviceSpec{{Device: r.SourceDevice, SizeBytes: r.SourceDiskSize}}
	}
	if len(devices) == 0 {
		return api.Migration{}, "", fmt.Errorf("store: at least one source device is required")
	}
	bootTarget := r.BootTarget
	if bootTarget == "" {
		bootTarget = api.BootTargetVolume
	}
	now := time.Now()
	res, err := s.db.ExecContext(ctx, `
INSERT INTO migrations (name, state, source_hostname, source_ip, source_device, source_disk_size,
  enroll_token, boot_target, plan_class, linode_type, created_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		r.Name, string(api.MigCreated), r.SourceHostname, r.SourceIP, devices[0].Device, devices[0].SizeBytes,
		token, bootTarget, r.PlanClass, r.LinodeType, unix(now))
	if err != nil {
		return api.Migration{}, "", err
	}
	id, _ := res.LastInsertId()
	for i, d := range devices {
		if _, err := s.db.ExecContext(ctx, `
INSERT INTO migration_disks (migration_id, idx, source_device, size_bytes) VALUES (?, ?, ?, ?)`,
			id, i, d.Device, d.SizeBytes); err != nil {
			return api.Migration{}, "", err
		}
	}
	m, err := s.Migration(ctx, id)
	return m, token, err
}

const diskCols = `id, idx, source_device, size_bytes, receiver_port, volume_id, volume_device,
 artifact_id, full_sync_done, total_blocks, changed_blocks, bytes_on_wire, last_sync_at, agent_last_seen, agent_connected_at, last_error`

func scanDisk(row interface{ Scan(...any) error }) (api.Disk, error) {
	var d api.Disk
	var full int
	var lastSync, agentSeen, agentConn int64
	if err := row.Scan(&d.ID, &d.Index, &d.SourceDevice, &d.SizeBytes, &d.ReceiverPort, &d.VolumeID,
		&d.VolumeDevice, &d.ArtifactID, &full, &d.TotalBlocks, &d.ChangedBlocks, &d.BytesOnWire,
		&lastSync, &agentSeen, &agentConn, &d.LastError); err != nil {
		return api.Disk{}, err
	}
	d.FullSyncDone = full != 0
	d.LastSyncAt = fromUnix(lastSync)
	d.AgentLastSeen = fromUnix(agentSeen)
	d.AgentConnectedAt = fromUnix(agentConn)
	return d, nil
}

// loadDisks attaches the disk rows to a migration.
func (s *Store) loadDisks(ctx context.Context, m *api.Migration) error {
	rows, err := s.db.QueryContext(ctx, `SELECT `+diskCols+` FROM migration_disks WHERE migration_id=? ORDER BY idx`, m.ID)
	if err != nil {
		return err
	}
	defer rows.Close()
	m.Disks = nil
	for rows.Next() {
		d, err := scanDisk(rows)
		if err != nil {
			return err
		}
		m.Disks = append(m.Disks, d)
	}
	return rows.Err()
}

// Migration looks up a migration (with its disks) by id.
func (s *Store) Migration(ctx context.Context, id int64) (api.Migration, error) {
	row := s.db.QueryRowContext(ctx, `SELECT `+migCols+` FROM migrations WHERE id=?`, id)
	m, err := scanMigration(row)
	if errors.Is(err, sql.ErrNoRows) {
		return api.Migration{}, ErrNotFound
	}
	if err != nil {
		return api.Migration{}, err
	}
	return m, s.loadDisks(ctx, &m)
}

// MigrationByToken looks up a migration (with its disks) by enrollment token.
func (s *Store) MigrationByToken(ctx context.Context, token string) (api.Migration, error) {
	row := s.db.QueryRowContext(ctx, `SELECT `+migCols+` FROM migrations WHERE enroll_token=?`, token)
	m, err := scanMigration(row)
	if errors.Is(err, sql.ErrNoRows) {
		return api.Migration{}, ErrNotFound
	}
	if err != nil {
		return api.Migration{}, err
	}
	return m, s.loadDisks(ctx, &m)
}

// EnrollToken returns the enrollment token for a migration.
func (s *Store) EnrollToken(ctx context.Context, id int64) (string, error) {
	var tok string
	err := s.db.QueryRowContext(ctx, `SELECT enroll_token FROM migrations WHERE id=?`, id).Scan(&tok)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	return tok, err
}

// ListMigrations returns all migrations, newest first.
func (s *Store) ListMigrations(ctx context.Context) ([]api.Migration, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+migCols+` FROM migrations ORDER BY id DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []api.Migration
	for rows.Next() {
		m, err := scanMigration(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for i := range out {
		if err := s.loadDisks(ctx, &out[i]); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// SetMigrationState updates the state (and optionally clears/sets last_error).
func (s *Store) SetMigrationState(ctx context.Context, id int64, state api.MigrationState, errMsg string) error {
	res, err := s.db.ExecContext(ctx, `UPDATE migrations SET state=?, last_error=? WHERE id=?`,
		string(state), errMsg, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// AssignDiskPort sets the receiver port for a disk.
func (s *Store) AssignDiskPort(ctx context.Context, diskID int64, port int) error {
	_, err := s.db.ExecContext(ctx, `UPDATE migration_disks SET receiver_port=? WHERE id=?`, port, diskID)
	return err
}

// SetDiskVolume records the provisioned volume and device path for a disk.
func (s *Store) SetDiskVolume(ctx context.Context, diskID, volumeID int64, device string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE migration_disks SET volume_id=?, volume_device=? WHERE id=?`, volumeID, device, diskID)
	return err
}

// SetDiskArtifact records the cloned-artifact id for a disk.
func (s *Store) SetDiskArtifact(ctx context.Context, diskID int64, artifactID string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE migration_disks SET artifact_id=? WHERE id=?`, artifactID, diskID)
	return err
}

// RecordDiskError records (or clears, with msg="") the last receiver-side
// session error for a disk and refreshes its agent_last_seen.
func (s *Store) RecordDiskError(ctx context.Context, diskID int64, msg string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE migration_disks SET last_error=?, agent_last_seen=? WHERE id=?`,
		msg, unix(time.Now()), diskID)
	return err
}

// RecordAgentConnected stamps a disk's agent_connected_at (and refreshes
// agent_last_seen) to now. It is called on every successful agent handshake —
// including a "held" one before replication is started — so the console can show
// the agent as connected without any data having to flow yet.
func (s *Store) RecordAgentConnected(ctx context.Context, diskID int64) error {
	now := unix(time.Now())
	_, err := s.db.ExecContext(ctx,
		`UPDATE migration_disks SET agent_connected_at=?, agent_last_seen=? WHERE id=?`, now, now, diskID)
	return err
}

// SetReplicationEnabled flips a migration's replication gate. While disabled the
// receiver acknowledges agent connections but holds (applies no data); enabling
// it lets the next agent pass stream.
func (s *Store) SetReplicationEnabled(ctx context.Context, id int64, enabled bool) error {
	res, err := s.db.ExecContext(ctx, `UPDATE migrations SET replication_enabled=? WHERE id=?`, boolToInt(enabled), id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// MarkEnrolled stamps enrolled_at to now the first time the agent is downloaded
// for a migration (i.e. the install command is running on the source), so the
// console can time out an agent that never checks in. Idempotent: only the first
// download sets it; later re-downloads leave the original timestamp.
func (s *Store) MarkEnrolled(ctx context.Context, id int64) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE migrations SET enrolled_at=? WHERE id=? AND enrolled_at=0`, unix(time.Now()), id)
	return err
}

// AddEvent appends an entry to a migration's activity log.
func (s *Store) AddEvent(ctx context.Context, migrationID int64, level, message string) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO migration_events (migration_id, at, level, message) VALUES (?, ?, ?, ?)`,
		migrationID, unix(time.Now()), level, message)
	return err
}

// Events returns a migration's activity log, newest first (capped).
func (s *Store) Events(ctx context.Context, migrationID int64, limit int) ([]api.Event, error) {
	if limit <= 0 {
		limit = 200
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, at, level, message FROM migration_events WHERE migration_id=? ORDER BY id DESC LIMIT ?`,
		migrationID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []api.Event
	for rows.Next() {
		var e api.Event
		var at int64
		if err := rows.Scan(&e.ID, &at, &e.Level, &e.Message); err != nil {
			return nil, err
		}
		e.At = fromUnix(at)
		out = append(out, e)
	}
	return out, rows.Err()
}

// SetMigrationImage records the boot artifact and launched instance.
func (s *Store) SetMigrationImage(ctx context.Context, id int64, imageID string, launchedID int64) error {
	_, err := s.db.ExecContext(ctx, `UPDATE migrations SET image_id=?, launched_id=? WHERE id=?`, imageID, launchedID, id)
	return err
}

// RecordDiskSync updates one disk's progress from a completed replication pass
// and advances the migration to "replicating" on first activity.
func (s *Store) RecordDiskSync(ctx context.Context, migrationID, diskID int64, fullSync bool, total, changed, bytes int64) error {
	now := unix(time.Now())
	if _, err := s.db.ExecContext(ctx, `
UPDATE migration_disks
SET total_blocks=?, changed_blocks=?, bytes_on_wire=bytes_on_wire+?,
    last_sync_at=?, agent_last_seen=?, full_sync_done = (full_sync_done=1 OR ?), last_error=''
WHERE id=?`,
		total, changed, bytes, now, now, boolToInt(fullSync), diskID); err != nil {
		return err
	}
	_, err := s.db.ExecContext(ctx, `
UPDATE migrations SET state = CASE WHEN state IN ('awaiting_agent','created') THEN 'replicating' ELSE state END
WHERE id=?`, migrationID)
	return err
}

// DeleteMigration removes a migration record.
func (s *Store) DeleteMigration(ctx context.Context, id int64) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM migrations WHERE id=?`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// SetAssessed records (or clears, with assessed=false) a passed pre-migration
// assessment.
func (s *Store) SetAssessed(ctx context.Context, id int64, assessed bool) error {
	ts := int64(0)
	if assessed {
		ts = unix(time.Now())
	}
	_, err := s.db.ExecContext(ctx, `UPDATE migrations SET assessed_at=? WHERE id=?`, ts, id)
	return err
}

// SetMigrateStarted stamps the start of a migration run (and clears any
// previous finish time).
func (s *Store) SetMigrateStarted(ctx context.Context, id int64) error {
	_, err := s.db.ExecContext(ctx, `UPDATE migrations SET migrate_started=?, migrate_finished=0 WHERE id=?`, unix(time.Now()), id)
	return err
}

// SetMigrateFinished stamps the end of a migration run.
func (s *Store) SetMigrateFinished(ctx context.Context, id int64) error {
	_, err := s.db.ExecContext(ctx, `UPDATE migrations SET migrate_finished=? WHERE id=?`, unix(time.Now()), id)
	return err
}

// DeleteSetting removes a settings row (e.g. the stored Linode token).
func (s *Store) DeleteSetting(ctx context.Context, key string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM settings WHERE key=?`, key)
	return err
}

// DeleteLinodeToken removes the stored Linode API token and account label.
func (s *Store) DeleteLinodeToken(ctx context.Context) error {
	if err := s.DeleteSetting(ctx, keyLinodeTok); err != nil {
		return err
	}
	return s.DeleteSetting(ctx, keyLinodeAcct)
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
