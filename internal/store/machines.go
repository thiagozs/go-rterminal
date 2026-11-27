package store

import (
	"database/sql"
	"errors"
	"regexp"
	"strings"
	"time"
)

type Machine struct {
	ID        string     `json:"id"`
	Name      string     `json:"name"`
	GroupID   *int64     `json:"group_id"`
	CreatedAt time.Time  `json:"created_at"`
	LastSeen  *time.Time `json:"last_seen"`
	Hostname  string     `json:"hostname"`
	OS        string     `json:"os"`
	Arch      string     `json:"arch"`
	Version   string     `json:"version"`
	// ShellUser is the account the agent opens terminals and file transfers
	// as (--shell-user). Empty means the agent's own. It only shapes the
	// install commands the panel generates.
	ShellUser string `json:"shell_user"`
}

const machineColumns = `id, name, group_id, created_at, last_seen, hostname, os, arch, version, shell_user`

type scanner interface{ Scan(dest ...any) error }

func scanMachine(r scanner) (*Machine, error) {
	var m Machine
	var group, lastSeen sql.NullInt64
	var created int64
	if err := r.Scan(&m.ID, &m.Name, &group, &created, &lastSeen, &m.Hostname, &m.OS, &m.Arch, &m.Version, &m.ShellUser); err != nil {
		return nil, err
	}
	m.CreatedAt = fromUnix(created)
	if group.Valid {
		v := group.Int64
		m.GroupID = &v
	}
	if lastSeen.Valid {
		t := fromUnix(lastSeen.Int64)
		m.LastSeen = &t
	}
	return &m, nil
}

// shellUserName matches portable POSIX account names, which is also what makes
// the value safe to put into the generated shell commands and systemd unit.
var shellUserName = regexp.MustCompile(`^[a-z_][a-z0-9_-]{0,31}$`)

// ValidShellUser reports whether name can be used as a shell user ("" is valid: none).
func ValidShellUser(name string) bool {
	name = strings.TrimSpace(name)
	return name == "" || shellUserName.MatchString(name)
}

// SetMachineShellUser records which account the agent of a machine opens
// terminals as ("" clears it).
func (s *Store) SetMachineShellUser(id, shellUser string) error {
	shellUser = strings.TrimSpace(shellUser)
	if !ValidShellUser(shellUser) {
		return ErrInvalid
	}
	res, err := s.db.Exec(`UPDATE machines SET shell_user = ? WHERE id = ?`, shellUser, id)
	if err != nil {
		return err
	}
	return affected(res)
}

// CreateMachine registers a machine and returns its credential. The credential
// is only stored hashed, so this is the only time it can be shown.
func (s *Store) CreateMachine(name string, groupID *int64) (*Machine, string, error) {
	name = strings.TrimSpace(name)
	if name == "" || len(name) > 64 {
		return nil, "", ErrInvalid
	}
	base := Slug(name)
	if base == "" {
		return nil, "", ErrInvalid
	}
	if groupID != nil {
		if _, err := s.GetGroup(*groupID); err != nil {
			return nil, "", err
		}
	}
	token, err := NewToken()
	if err != nil {
		return nil, "", err
	}
	now := time.Now()
	id := base
	for attempt := 0; ; attempt++ {
		if attempt > 0 {
			suffix, err := NewToken()
			if err != nil {
				return nil, "", err
			}
			id = trimTo(base, 27) + "-" + suffix[:4]
		}
		_, err = s.db.Exec(`INSERT INTO machines(id, name, group_id, token_hash, created_at) VALUES(?,?,?,?,?)`,
			id, name, groupID, HashToken(token), unix(now))
		if err == nil {
			break
		}
		if !isUnique(err) || attempt >= 5 {
			return nil, "", err
		}
	}
	m, err := s.GetMachine(id)
	return m, token, err
}

func trimTo(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}

func (s *Store) ListMachines() ([]Machine, error) {
	rows, err := s.db.Query(`SELECT ` + machineColumns + ` FROM machines ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	machines := []Machine{}
	for rows.Next() {
		m, err := scanMachine(rows)
		if err != nil {
			return nil, err
		}
		machines = append(machines, *m)
	}
	return machines, rows.Err()
}

func (s *Store) GetMachine(id string) (*Machine, error) {
	m, err := scanMachine(s.db.QueryRow(`SELECT `+machineColumns+` FROM machines WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return m, err
}

// MachineByToken resolves a machine credential.
func (s *Store) MachineByToken(token string) (*Machine, error) {
	if token == "" {
		return nil, ErrNotFound
	}
	m, err := scanMachine(s.db.QueryRow(`SELECT `+machineColumns+` FROM machines WHERE token_hash = ?`, HashToken(token)))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return m, err
}

func (s *Store) UpdateMachine(id, name string, groupID *int64) error {
	name = strings.TrimSpace(name)
	if name == "" || len(name) > 64 {
		return ErrInvalid
	}
	if groupID != nil {
		if _, err := s.GetGroup(*groupID); err != nil {
			return err
		}
	}
	res, err := s.db.Exec(`UPDATE machines SET name = ?, group_id = ? WHERE id = ?`, name, groupID, id)
	if err != nil {
		return err
	}
	return affected(res)
}

// RotateMachineToken replaces the credential, invalidating the old one.
func (s *Store) RotateMachineToken(id string) (string, error) {
	token, err := NewToken()
	if err != nil {
		return "", err
	}
	res, err := s.db.Exec(`UPDATE machines SET token_hash = ? WHERE id = ?`, HashToken(token), id)
	if err != nil {
		return "", err
	}
	return token, affected(res)
}

func (s *Store) DeleteMachine(id string) error {
	res, err := s.db.Exec(`DELETE FROM machines WHERE id = ?`, id)
	if err != nil {
		return err
	}
	return affected(res)
}

// TouchMachine records a connection with the facts the agent reported.
func (s *Store) TouchMachine(id, hostname, osName, arch, version string) error {
	_, err := s.db.Exec(`UPDATE machines SET last_seen = ?, hostname = ?, os = ?, arch = ?, version = ? WHERE id = ?`,
		unix(time.Now()), trimTo(hostname, 128), trimTo(osName, 32), trimTo(arch, 32), trimTo(version, 32), id)
	return err
}
