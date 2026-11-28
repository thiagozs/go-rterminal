package store

import (
	"database/sql"
	"errors"
	"time"
)

// UserPrefs returns the JSON the panel saved for an account ("" when it
// never saved any).
func (s *Store) UserPrefs(userID int64) (string, error) {
	var data string
	err := s.db.QueryRow(`SELECT data FROM user_prefs WHERE user_id = ?`, userID).Scan(&data)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return data, err
}

// SetUserPrefs replaces the saved preferences of an account.
func (s *Store) SetUserPrefs(userID int64, data string) error {
	_, err := s.db.Exec(`INSERT INTO user_prefs (user_id, data, updated_at) VALUES (?, ?, ?)
		ON CONFLICT(user_id) DO UPDATE SET data = excluded.data, updated_at = excluded.updated_at`,
		userID, data, unix(time.Now()))
	if isForeignKey(err) {
		return ErrNotFound
	}
	return err
}
