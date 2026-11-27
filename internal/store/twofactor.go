package store

import (
	"database/sql"
	"errors"
	"strings"
)

// The second-factor state of an account. Secrets arrive and leave already
// sealed (encrypted) by the caller; this package only stores them.

// SetEmail stores the address that receives login PINs ("" clears it, which
// also turns the e-mail factor off).
func (s *Store) SetEmail(id int64, email string) error {
	email = strings.TrimSpace(email)
	res, err := s.db.Exec(`UPDATE admins SET email = ?, email_2fa = CASE WHEN ? = '' THEN 0 ELSE email_2fa END WHERE id = ?`, email, email, id)
	if err != nil {
		return err
	}
	return affected(res)
}

// SetTOTPPending keeps a freshly generated secret until the user proves their
// app produces valid codes.
func (s *Store) SetTOTPPending(id int64, sealed string) error {
	_, err := s.db.Exec(`UPDATE admins SET totp_pending = ? WHERE id = ?`, sealed, id)
	return err
}

// TOTPPending returns the sealed secret waiting for confirmation ("" if none).
func (s *Store) TOTPPending(id int64) (string, error) {
	var sealed string
	err := s.db.QueryRow(`SELECT totp_pending FROM admins WHERE id = ?`, id).Scan(&sealed)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	return sealed, err
}

// EnableTOTP activates the confirmed secret; step is the code step just used,
// so that same code cannot be replayed at the next login.
func (s *Store) EnableTOTP(id int64, sealed string, step int64) error {
	_, err := s.db.Exec(`UPDATE admins SET totp_secret = ?, totp_pending = '', totp_enabled = 1, totp_last_step = ? WHERE id = ?`, sealed, step, id)
	return err
}

// TOTPSecret returns the sealed secret and the last accepted step.
func (s *Store) TOTPSecret(id int64) (sealed string, lastStep int64, enabled bool, err error) {
	var en int64
	err = s.db.QueryRow(`SELECT totp_secret, totp_last_step, totp_enabled FROM admins WHERE id = ?`, id).Scan(&sealed, &lastStep, &en)
	if errors.Is(err, sql.ErrNoRows) {
		err = ErrNotFound
	}
	return sealed, lastStep, en != 0, err
}

// AdvanceTOTPStep records step as used. It reports false when a step at least
// as new was already accepted, which makes replay protection atomic even with
// two concurrent logins.
func (s *Store) AdvanceTOTPStep(id, step int64) (bool, error) {
	res, err := s.db.Exec(`UPDATE admins SET totp_last_step = ? WHERE id = ? AND totp_last_step < ?`, step, id, step)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// DisableTOTP turns the app factor off and forgets the secret.
func (s *Store) DisableTOTP(id int64) error {
	_, err := s.db.Exec(`UPDATE admins SET totp_secret = '', totp_pending = '', totp_enabled = 0, totp_last_step = 0 WHERE id = ?`, id)
	return err
}

func (s *Store) SetEmailPIN(id int64, enabled bool) error {
	_, err := s.db.Exec(`UPDATE admins SET email_2fa = ? WHERE id = ?`, boolInt(enabled), id)
	return err
}

// ReplaceRecoveryCodes discards every old code and stores the new hashes.
func (s *Store) ReplaceRecoveryCodes(id int64, hashes []string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`DELETE FROM recovery_codes WHERE user_id = ?`, id); err != nil {
		return err
	}
	for _, h := range hashes {
		if _, err := tx.Exec(`INSERT INTO recovery_codes(user_id, code_hash) VALUES(?,?)`, id, h); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// UseRecoveryCode burns a code; false when it does not exist or was used.
func (s *Store) UseRecoveryCode(id int64, hash string) (bool, error) {
	res, err := s.db.Exec(`UPDATE recovery_codes SET used = 1 WHERE user_id = ? AND code_hash = ? AND used = 0`, id, hash)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

func (s *Store) RecoveryCodesLeft(id int64) (int, error) {
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM recovery_codes WHERE user_id = ? AND used = 0`, id).Scan(&n)
	return n, err
}

// ClearTwoFactor removes every second factor and recovery code of an account
// and ends its sessions (an administrator rescuing a locked-out user).
func (s *Store) ClearTwoFactor(id int64) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	res, err := tx.Exec(`UPDATE admins SET totp_secret = '', totp_pending = '', totp_enabled = 0, totp_last_step = 0, email_2fa = 0 WHERE id = ?`, id)
	if err != nil {
		return err
	}
	if err := affected(res); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM recovery_codes WHERE user_id = ?`, id); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM web_sessions WHERE admin_id = ?`, id); err != nil {
		return err
	}
	return tx.Commit()
}

// UserByName finds an account by username (CLI recovery commands).
func (s *Store) UserByName(username string) (*Admin, error) {
	a, err := scanAdmin(s.db.QueryRow(`SELECT `+adminColumns+` FROM admins WHERE username = ?`, username))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return a, err
}
