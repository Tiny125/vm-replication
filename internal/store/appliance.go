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
	keyAdminSalt = "admin_pw_salt"
	keyAdminHash = "admin_pw_hash"
	keyLinodeTok = "linode_token_enc"
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

// ---- migrations ----

const migCols = `id, name, state, source_hostname, source_device, source_disk_size,
 enroll_token, receiver_port, volume_id, volume_device, image_id, launched_id,
 agent_last_seen, full_sync_done, total_blocks, changed_blocks, bytes_on_wire,
 last_sync_at, last_error, assessed_at, migrate_started, migrate_finished, created_at`

func scanMigration(row interface{ Scan(...any) error }) (api.Migration, error) {
	var m api.Migration
	var state, enrollToken string // token is kept out of the JSON DTO
	var fullDone int
	var agentSeen, lastSync, assessed, migStart, migFinish, created int64
	if err := row.Scan(&m.ID, &m.Name, &state, &m.SourceHostname, &m.SourceDevice, &m.SourceDiskSize,
		&enrollToken, &m.ReceiverPort, &m.VolumeID, &m.VolumeDevice, &m.ImageID, &m.LaunchedID,
		&agentSeen, &fullDone, &m.TotalBlocks, &m.ChangedBlocks, &m.BytesOnWire,
		&lastSync, &m.LastError, &assessed, &migStart, &migFinish, &created); err != nil {
		return api.Migration{}, err
	}
	m.State = api.MigrationState(state)
	m.FullSyncDone = fullDone != 0
	m.AgentLastSeen = fromUnix(agentSeen)
	m.LastSyncAt = fromUnix(lastSync)
	m.AssessedAt = fromUnix(assessed)
	m.MigrateStarted = fromUnix(migStart)
	m.MigrateFinished = fromUnix(migFinish)
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

// CreateMigration inserts a new migration with a fresh enrollment token and an
// assigned receiver port (basePort + id offset is resolved by the caller via
// AssignReceiverPort after insert).
func (s *Store) CreateMigration(ctx context.Context, r api.CreateMigrationRequest) (api.Migration, string, error) {
	token, err := randToken(24)
	if err != nil {
		return api.Migration{}, "", err
	}
	now := time.Now()
	res, err := s.db.ExecContext(ctx, `
INSERT INTO migrations (name, state, source_hostname, source_device, source_disk_size, enroll_token, created_at)
VALUES (?, ?, ?, ?, ?, ?, ?)`,
		r.Name, string(api.MigCreated), r.SourceHostname, r.SourceDevice, r.SourceDiskSize, token, unix(now))
	if err != nil {
		return api.Migration{}, "", err
	}
	id, _ := res.LastInsertId()
	m, err := s.Migration(ctx, id)
	return m, token, err
}

// Migration looks up a migration by id.
func (s *Store) Migration(ctx context.Context, id int64) (api.Migration, error) {
	row := s.db.QueryRowContext(ctx, `SELECT `+migCols+` FROM migrations WHERE id=?`, id)
	m, err := scanMigration(row)
	if errors.Is(err, sql.ErrNoRows) {
		return api.Migration{}, ErrNotFound
	}
	return m, err
}

// MigrationByToken looks up a migration by its enrollment token (constant-time
// is unnecessary here: tokens are 24 random bytes and looked up by exact match).
func (s *Store) MigrationByToken(ctx context.Context, token string) (api.Migration, error) {
	row := s.db.QueryRowContext(ctx, `SELECT `+migCols+` FROM migrations WHERE enroll_token=?`, token)
	m, err := scanMigration(row)
	if errors.Is(err, sql.ErrNoRows) {
		return api.Migration{}, ErrNotFound
	}
	return m, err
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
	return out, rows.Err()
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

// AssignReceiverPort sets the receiver port for a migration.
func (s *Store) AssignReceiverPort(ctx context.Context, id int64, port int) error {
	_, err := s.db.ExecContext(ctx, `UPDATE migrations SET receiver_port=? WHERE id=?`, port, id)
	return err
}

// SetMigrationVolume records the provisioned volume and its device path.
func (s *Store) SetMigrationVolume(ctx context.Context, id, volumeID int64, device string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE migrations SET volume_id=?, volume_device=? WHERE id=?`, volumeID, device, id)
	return err
}

// SetMigrationImage records the resulting image and launched instance.
func (s *Store) SetMigrationImage(ctx context.Context, id int64, imageID string, launchedID int64) error {
	_, err := s.db.ExecContext(ctx, `UPDATE migrations SET image_id=?, launched_id=? WHERE id=?`, imageID, launchedID, id)
	return err
}

// RecordMigrationSync updates progress from a completed replication pass.
func (s *Store) RecordMigrationSync(ctx context.Context, id int64, fullSync bool, total, changed, bytes int64) error {
	now := unix(time.Now())
	// full_sync_done becomes (and stays) true once any full sync completes.
	_, err := s.db.ExecContext(ctx, `
UPDATE migrations
SET total_blocks=?, changed_blocks=?, bytes_on_wire=bytes_on_wire+?,
    last_sync_at=?, agent_last_seen=?,
    full_sync_done = (full_sync_done=1 OR ?),
    state = CASE WHEN state IN ('awaiting_agent','created') THEN 'replicating' ELSE state END
WHERE id=?`,
		total, changed, bytes, now, now, boolToInt(fullSync), id)
	return err
}

// TouchAgent records that the agent for a migration checked in.
func (s *Store) TouchAgent(ctx context.Context, id int64) error {
	_, err := s.db.ExecContext(ctx, `UPDATE migrations SET agent_last_seen=? WHERE id=?`, unix(time.Now()), id)
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

// DeleteLinodeToken removes the stored Linode API token.
func (s *Store) DeleteLinodeToken(ctx context.Context) error {
	return s.DeleteSetting(ctx, keyLinodeTok)
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
