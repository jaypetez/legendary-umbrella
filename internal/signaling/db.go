package signaling

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	_ "modernc.org/sqlite"
)

const schema = `
CREATE TABLE IF NOT EXISTS devices (
	id           TEXT PRIMARY KEY,
	name         TEXT NOT NULL,
	token_hash   TEXT NOT NULL UNIQUE,
	platform     TEXT NOT NULL DEFAULT '',
	created_at   INTEGER NOT NULL,
	last_seen_at INTEGER NOT NULL DEFAULT 0
);

CREATE TABLE IF NOT EXISTS enrollments (
	device_code   TEXT PRIMARY KEY,
	user_code     TEXT NOT NULL UNIQUE,
	expires_at    INTEGER NOT NULL,
	approved      INTEGER NOT NULL DEFAULT 0,
	device_id     TEXT,
	proposed_name TEXT,
	platform      TEXT NOT NULL DEFAULT '',
	hostname      TEXT NOT NULL DEFAULT ''
);
`

type Store struct {
	db *sql.DB
}

func OpenStore(path string) (*Store, error) {
	db, err := sql.Open("sqlite", path+"?_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)")
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	if _, err := db.Exec(schema); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("apply schema: %w", err)
	}
	return &Store{db: db}, nil
}

func (s *Store) Close() error { return s.db.Close() }

func hashToken(raw string) string {
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:])
}

// --- enrollments -----------------------------------------------------------

type Enrollment struct {
	DeviceCode   string
	UserCode     string
	ExpiresAt    time.Time
	Approved     bool
	DeviceID     sql.NullString
	ProposedName sql.NullString
	Platform     string
	Hostname     string
}

func (s *Store) CreateEnrollment(ctx context.Context, e Enrollment) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO enrollments (device_code, user_code, expires_at, platform, hostname)
		VALUES (?, ?, ?, ?, ?)`,
		e.DeviceCode, e.UserCode, e.ExpiresAt.Unix(), e.Platform, e.Hostname,
	)
	return err
}

func (s *Store) GetEnrollmentByDeviceCode(ctx context.Context, code string) (*Enrollment, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT device_code, user_code, expires_at, approved, device_id, proposed_name, platform, hostname
		FROM enrollments WHERE device_code = ?`, code)
	return scanEnrollment(row)
}

func (s *Store) GetEnrollmentByUserCode(ctx context.Context, code string) (*Enrollment, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT device_code, user_code, expires_at, approved, device_id, proposed_name, platform, hostname
		FROM enrollments WHERE user_code = ?`, code)
	return scanEnrollment(row)
}

type rowScanner interface {
	Scan(dest ...any) error
}

func scanEnrollment(r rowScanner) (*Enrollment, error) {
	var e Enrollment
	var exp int64
	var approved int
	err := r.Scan(&e.DeviceCode, &e.UserCode, &exp, &approved, &e.DeviceID, &e.ProposedName, &e.Platform, &e.Hostname)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	e.ExpiresAt = time.Unix(exp, 0)
	e.Approved = approved != 0
	return &e, nil
}

func (s *Store) ApproveEnrollment(ctx context.Context, userCode, proposedName string) error {
	res, err := s.db.ExecContext(ctx, `
		UPDATE enrollments
		SET approved = 1, proposed_name = ?
		WHERE user_code = ? AND approved = 0 AND expires_at > ?`,
		proposedName, userCode, time.Now().Unix(),
	)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// MaterializeDevice converts an approved enrollment into a device record and
// returns the bearer token the agent should store. It's idempotent per
// enrollment: a second call returns ErrAlreadyClaimed.
func (s *Store) MaterializeDevice(ctx context.Context, deviceCode, token string) (*Device, error) {
	e, err := s.GetEnrollmentByDeviceCode(ctx, deviceCode)
	if err != nil {
		return nil, err
	}
	if !e.Approved {
		return nil, ErrNotApproved
	}
	if e.DeviceID.Valid {
		return nil, ErrAlreadyClaimed
	}
	if time.Now().After(e.ExpiresAt) {
		return nil, ErrExpired
	}

	id := randomUUID()
	name := e.ProposedName.String
	if name == "" {
		name = e.Hostname
	}
	if name == "" {
		name = "device-" + id[:8]
	}
	now := time.Now().Unix()

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	if _, err := tx.ExecContext(ctx, `
		INSERT INTO devices (id, name, token_hash, platform, created_at)
		VALUES (?, ?, ?, ?, ?)`,
		id, name, hashToken(token), e.Platform, now,
	); err != nil {
		return nil, err
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE enrollments SET device_id = ? WHERE device_code = ?`, id, deviceCode); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return &Device{ID: id, Name: name, Platform: e.Platform, CreatedAt: now}, nil
}

// --- devices ---------------------------------------------------------------

type Device struct {
	ID         string
	Name       string
	Platform   string
	CreatedAt  int64
	LastSeenAt int64
}

func (s *Store) AuthenticateDevice(ctx context.Context, token string) (*Device, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT id, name, platform, created_at, last_seen_at
		FROM devices WHERE token_hash = ?`, hashToken(token))
	var d Device
	err := row.Scan(&d.ID, &d.Name, &d.Platform, &d.CreatedAt, &d.LastSeenAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &d, nil
}

func (s *Store) TouchDevice(ctx context.Context, id string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE devices SET last_seen_at = ? WHERE id = ?`, time.Now().Unix(), id)
	return err
}

func (s *Store) ListDevices(ctx context.Context) ([]Device, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, name, platform, created_at, last_seen_at
		FROM devices ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Device
	for rows.Next() {
		var d Device
		if err := rows.Scan(&d.ID, &d.Name, &d.Platform, &d.CreatedAt, &d.LastSeenAt); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// --- sentinel errors -------------------------------------------------------

var (
	ErrNotFound       = errors.New("not found")
	ErrNotApproved    = errors.New("enrollment not approved")
	ErrAlreadyClaimed = errors.New("enrollment already claimed")
	ErrExpired        = errors.New("enrollment expired")
)
