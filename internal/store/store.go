// Package store persists admins, groups, machines and the audit log in SQLite.
package store

import (
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"golang.org/x/crypto/bcrypt"
	sqlite "modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"
)

var (
	ErrNotFound           = errors.New("not found")
	ErrConflict           = errors.New("already exists")
	ErrInvalid            = errors.New("invalid input")
	ErrInvalidCredentials = errors.New("invalid credentials")
	// ErrLastAdmin protects the panel from being left without an administrator.
	ErrLastAdmin = errors.New("cannot remove the last administrator")
)

// BcryptCost is the bcrypt work factor for admin passwords; tests lower it.
var BcryptCost = bcrypt.DefaultCost

const schema = `
CREATE TABLE IF NOT EXISTS admins (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	username TEXT NOT NULL UNIQUE,
	password_hash TEXT NOT NULL,
	created_at INTEGER NOT NULL,
	must_change_password INTEGER NOT NULL DEFAULT 0,
	role TEXT NOT NULL DEFAULT 'admin',
	email TEXT NOT NULL DEFAULT '',
	totp_secret TEXT NOT NULL DEFAULT '',
	totp_pending TEXT NOT NULL DEFAULT '',
	totp_enabled INTEGER NOT NULL DEFAULT 0,
	totp_last_step INTEGER NOT NULL DEFAULT 0,
	email_2fa INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE IF NOT EXISTS recovery_codes (
	user_id INTEGER NOT NULL REFERENCES admins(id) ON DELETE CASCADE,
	code_hash TEXT NOT NULL,
	used INTEGER NOT NULL DEFAULT 0,
	PRIMARY KEY (user_id, code_hash)
);
CREATE TABLE IF NOT EXISTS user_groups (
	user_id INTEGER NOT NULL REFERENCES admins(id) ON DELETE CASCADE,
	group_id INTEGER NOT NULL REFERENCES groups(id) ON DELETE CASCADE,
	PRIMARY KEY (user_id, group_id)
);
CREATE TABLE IF NOT EXISTS web_sessions (
	token_hash TEXT PRIMARY KEY,
	admin_id INTEGER NOT NULL REFERENCES admins(id) ON DELETE CASCADE,
	expires_at INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS groups (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	name TEXT NOT NULL UNIQUE,
	description TEXT NOT NULL DEFAULT '',
	created_at INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS machines (
	id TEXT PRIMARY KEY,
	name TEXT NOT NULL,
	group_id INTEGER REFERENCES groups(id) ON DELETE SET NULL,
	token_hash TEXT NOT NULL UNIQUE,
	created_at INTEGER NOT NULL,
	last_seen INTEGER,
	hostname TEXT NOT NULL DEFAULT '',
	os TEXT NOT NULL DEFAULT '',
	arch TEXT NOT NULL DEFAULT '',
	version TEXT NOT NULL DEFAULT ''
);
CREATE TABLE IF NOT EXISTS recordings (
	id TEXT PRIMARY KEY,
	machine_id TEXT NOT NULL,
	machine_name TEXT NOT NULL,
	username TEXT NOT NULL,
	started_at INTEGER NOT NULL,
	ended_at INTEGER,
	bytes INTEGER NOT NULL DEFAULT 0,
	truncated INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS recordings_started ON recordings(started_at);
CREATE TABLE IF NOT EXISTS audit_log (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	at INTEGER NOT NULL,
	actor TEXT NOT NULL,
	action TEXT NOT NULL,
	target TEXT NOT NULL DEFAULT '',
	detail TEXT NOT NULL DEFAULT ''
);
`

type Store struct {
	db *sql.DB
}

// Open opens (creating if needed) the SQLite database at path.
func Open(path string) (*Store, error) {
	dsn := "file:" + path + "?_pragma=foreign_keys(1)&_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("abrir banco: %w", err)
	}
	db.SetMaxOpenConns(1) // SQLite allows one writer; keeps ordering simple.
	if _, err := db.Exec(schema); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("criar schema: %w", err)
	}
	if err := migrate(db); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("migrar schema: %w", err)
	}
	return &Store{db: db}, nil
}

// migrate upgrades databases created by earlier versions.
func migrate(db *sql.DB) error {
	if err := addColumnIfMissing(db, "admins", "must_change_password", "INTEGER NOT NULL DEFAULT 0"); err != nil {
		return err
	}
	// Accounts created before roles existed are administrators.
	if err := addColumnIfMissing(db, "admins", "role", "TEXT NOT NULL DEFAULT 'admin'"); err != nil {
		return err
	}
	for column, definition := range map[string]string{
		"email": "TEXT NOT NULL DEFAULT ''", "totp_secret": "TEXT NOT NULL DEFAULT ''", "totp_pending": "TEXT NOT NULL DEFAULT ''",
		"totp_enabled": "INTEGER NOT NULL DEFAULT 0", "totp_last_step": "INTEGER NOT NULL DEFAULT 0", "email_2fa": "INTEGER NOT NULL DEFAULT 0",
	} {
		if err := addColumnIfMissing(db, "admins", column, definition); err != nil {
			return err
		}
	}
	return nil
}

func addColumnIfMissing(db *sql.DB, table, column, definition string) error {
	rows, err := db.Query(`SELECT name FROM pragma_table_info(?)`, table)
	if err != nil {
		return err
	}
	found := false
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			rows.Close()
			return err
		}
		if name == column {
			found = true
		}
	}
	rows.Close()
	if found {
		return rows.Err()
	}
	_, err = db.Exec(`ALTER TABLE ` + table + ` ADD COLUMN ` + column + ` ` + definition)
	return err
}

func (s *Store) Close() error { return s.db.Close() }

// ---------- helpers ----------

// NewToken returns a random 256-bit hex secret.
func NewToken() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
}

// HashToken hashes a high-entropy secret for storage and lookup.
func HashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

func isUnique(err error) bool {
	var sqlErr *sqlite.Error
	if errors.As(err, &sqlErr) {
		return sqlErr.Code() == sqlite3.SQLITE_CONSTRAINT_UNIQUE || sqlErr.Code() == sqlite3.SQLITE_CONSTRAINT_PRIMARYKEY
	}
	return false
}

func unix(t time.Time) int64 { return t.Unix() }

func fromUnix(v int64) time.Time { return time.Unix(v, 0).UTC() }

// Slug turns a display name into a machine id: lowercase [a-z0-9-], max 32.
func Slug(name string) string {
	var b strings.Builder
	dash := false
	for _, r := range strings.ToLower(strings.TrimSpace(name)) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
			dash = false
		default:
			if b.Len() > 0 && !dash {
				b.WriteByte('-')
				dash = true
			}
		}
	}
	slug := strings.Trim(b.String(), "-")
	if len(slug) > 32 {
		slug = strings.Trim(slug[:32], "-")
	}
	return slug
}

func isForeignKey(err error) bool {
	var sqlErr *sqlite.Error
	return errors.As(err, &sqlErr) && sqlErr.Code() == sqlite3.SQLITE_CONSTRAINT_FOREIGNKEY
}
