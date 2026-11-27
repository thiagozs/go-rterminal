package store

import (
	"database/sql"
	"errors"
	"time"

	"golang.org/x/crypto/bcrypt"
)

type Admin struct {
	ID        int64     `json:"id"`
	Username  string    `json:"username"`
	CreatedAt time.Time `json:"created_at"`
	// MustChangePassword is set for temporary passwords; the admin API only
	// allows changing the password until it is cleared.
	MustChangePassword bool `json:"must_change_password"`
	// Role is RoleAdmin (full access) or RoleOperator (terminals of the
	// machines in the groups assigned to the account, nothing else).
	Role string `json:"role"`
}

const (
	RoleAdmin    = "admin"
	RoleOperator = "operator"
)

// IsAdmin reports whether the account has full access.
func (a *Admin) IsAdmin() bool { return a.Role == RoleAdmin }

// MinPasswordLength is the shortest accepted admin password.
const MinPasswordLength = 8

func (s *Store) CountAdmins() (int, error) {
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM admins`).Scan(&n)
	return n, err
}

// CreateAdmin adds an administrator. mustChange marks the password as
// temporary, forcing a change on first login.
func (s *Store) CreateAdmin(username, password string, mustChange bool) (*Admin, error) {
	return s.CreateUser(username, password, RoleAdmin, nil, mustChange)
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// dummyHash keeps VerifyAdmin's timing similar for unknown usernames.
var dummyHash, _ = bcrypt.GenerateFromPassword([]byte("dummy-password"), bcrypt.MinCost)

func (s *Store) VerifyAdmin(username, password string) (*Admin, error) {
	var a Admin
	var hash string
	var created, mustChange int64
	err := s.db.QueryRow(`SELECT id, username, password_hash, created_at, must_change_password, role FROM admins WHERE username = ?`, username).
		Scan(&a.ID, &a.Username, &hash, &created, &mustChange, &a.Role)
	if errors.Is(err, sql.ErrNoRows) {
		_ = bcrypt.CompareHashAndPassword(dummyHash, []byte(password))
		return nil, ErrInvalidCredentials
	}
	if err != nil {
		return nil, err
	}
	if bcrypt.CompareHashAndPassword([]byte(hash), []byte(password)) != nil {
		return nil, ErrInvalidCredentials
	}
	a.CreatedAt = fromUnix(created)
	a.MustChangePassword = mustChange != 0
	return &a, nil
}

// ChangePassword checks the current password, sets the new one, clears the
// temporary flag and ends every other session of that admin (keepToken is the
// caller's own session).
func (s *Store) ChangePassword(adminID int64, current, newPassword, keepToken string) error {
	if len(newPassword) < MinPasswordLength || newPassword == current {
		return ErrInvalid
	}
	var hash string
	err := s.db.QueryRow(`SELECT password_hash FROM admins WHERE id = ?`, adminID).Scan(&hash)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	if bcrypt.CompareHashAndPassword([]byte(hash), []byte(current)) != nil {
		return ErrInvalidCredentials
	}
	newHash, err := bcrypt.GenerateFromPassword([]byte(newPassword), BcryptCost)
	if err != nil {
		return err
	}
	if _, err := s.db.Exec(`UPDATE admins SET password_hash = ?, must_change_password = 0 WHERE id = ?`, string(newHash), adminID); err != nil {
		return err
	}
	_, err = s.db.Exec(`DELETE FROM web_sessions WHERE admin_id = ? AND token_hash != ?`, adminID, HashToken(keepToken))
	return err
}

// ResetAdminPassword sets a temporary password for username (recovery path):
// the admin must change it at the next login and all sessions are ended.
func (s *Store) ResetAdminPassword(username, temporary string) error {
	if len(temporary) < MinPasswordLength {
		return ErrInvalid
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(temporary), BcryptCost)
	if err != nil {
		return err
	}
	var id int64
	err = s.db.QueryRow(`SELECT id FROM admins WHERE username = ?`, username).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	if _, err := s.db.Exec(`UPDATE admins SET password_hash = ?, must_change_password = 1 WHERE id = ?`, string(hash), id); err != nil {
		return err
	}
	_, err = s.db.Exec(`DELETE FROM web_sessions WHERE admin_id = ?`, id)
	return err
}

// CreateWebSession issues a browser session token valid for ttl.
func (s *Store) CreateWebSession(adminID int64, ttl time.Duration) (string, error) {
	token, err := NewToken()
	if err != nil {
		return "", err
	}
	_, err = s.db.Exec(`INSERT INTO web_sessions(token_hash, admin_id, expires_at) VALUES(?,?,?)`,
		HashToken(token), adminID, unix(time.Now().Add(ttl)))
	return token, err
}

func (s *Store) LookupWebSession(token string) (*Admin, error) {
	var a Admin
	var created, mustChange int64
	err := s.db.QueryRow(`
		SELECT a.id, a.username, a.created_at, a.must_change_password, a.role FROM web_sessions w
		JOIN admins a ON a.id = w.admin_id
		WHERE w.token_hash = ? AND w.expires_at > ?`, HashToken(token), unix(time.Now())).
		Scan(&a.ID, &a.Username, &created, &mustChange, &a.Role)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	a.CreatedAt = fromUnix(created)
	a.MustChangePassword = mustChange != 0
	return &a, nil
}

func (s *Store) DeleteWebSession(token string) error {
	_, err := s.db.Exec(`DELETE FROM web_sessions WHERE token_hash = ?`, HashToken(token))
	return err
}

func (s *Store) PurgeExpiredSessions() error {
	_, err := s.db.Exec(`DELETE FROM web_sessions WHERE expires_at <= ?`, unix(time.Now()))
	return err
}
