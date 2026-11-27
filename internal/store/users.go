package store

import (
	"database/sql"
	"errors"
	"sort"
	"strings"
	"time"

	"golang.org/x/crypto/bcrypt"
)

// User is an account as listed to administrators.
type User struct {
	Admin
	// GroupIDs are the groups an operator may open terminals in. Administrators
	// see every machine, so it is empty for them.
	GroupIDs []int64 `json:"group_ids"`
}

func validRole(role string) bool { return role == RoleAdmin || role == RoleOperator }

// CreateUser adds an account. groupIDs only matter for operators.
func (s *Store) CreateUser(username, password, role string, groupIDs []int64, mustChange bool) (*Admin, error) {
	username = strings.TrimSpace(username)
	if username == "" || len(password) < MinPasswordLength || !validRole(role) {
		return nil, ErrInvalid
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(password), BcryptCost)
	if err != nil {
		return nil, err
	}
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	now := time.Now()
	res, err := tx.Exec(`INSERT INTO admins(username, password_hash, created_at, must_change_password, role) VALUES(?,?,?,?,?)`,
		username, string(hash), unix(now), boolInt(mustChange), role)
	if err != nil {
		if isUnique(err) {
			return nil, ErrConflict
		}
		return nil, err
	}
	id, _ := res.LastInsertId()
	if err := setGroups(tx, id, role, groupIDs); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return &Admin{ID: id, Username: username, CreatedAt: now.UTC(), MustChangePassword: mustChange, Role: role}, nil
}

// setGroups replaces the groups of an operator (administrators keep none).
func setGroups(tx *sql.Tx, userID int64, role string, groupIDs []int64) error {
	if _, err := tx.Exec(`DELETE FROM user_groups WHERE user_id = ?`, userID); err != nil {
		return err
	}
	if role != RoleOperator {
		return nil
	}
	for _, g := range groupIDs {
		if _, err := tx.Exec(`INSERT OR IGNORE INTO user_groups(user_id, group_id) VALUES(?,?)`, userID, g); err != nil {
			if isForeignKey(err) {
				return ErrInvalid // unknown group
			}
			return err
		}
	}
	return nil
}

func (s *Store) adminCount(tx *sql.Tx) (int, error) {
	var n int
	err := tx.QueryRow(`SELECT COUNT(*) FROM admins WHERE role = ?`, RoleAdmin).Scan(&n)
	return n, err
}

// ListUsers returns every account with its groups.
func (s *Store) ListUsers() ([]User, error) {
	rows, err := s.db.Query(`SELECT id, username, created_at, must_change_password, role FROM admins ORDER BY username`)
	if err != nil {
		return nil, err
	}
	var users []User
	for rows.Next() {
		var u User
		var created, mustChange int64
		if err := rows.Scan(&u.ID, &u.Username, &created, &mustChange, &u.Role); err != nil {
			rows.Close()
			return nil, err
		}
		u.CreatedAt = fromUnix(created)
		u.MustChangePassword = mustChange != 0
		u.GroupIDs = []int64{}
		users = append(users, u)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	byID := map[int64]*User{}
	for i := range users {
		byID[users[i].ID] = &users[i]
	}
	grows, err := s.db.Query(`SELECT user_id, group_id FROM user_groups ORDER BY group_id`)
	if err != nil {
		return nil, err
	}
	defer grows.Close()
	for grows.Next() {
		var uid, gid int64
		if err := grows.Scan(&uid, &gid); err != nil {
			return nil, err
		}
		if u := byID[uid]; u != nil {
			u.GroupIDs = append(u.GroupIDs, gid)
		}
	}
	return users, grows.Err()
}

// GetUser returns one account.
func (s *Store) GetUser(id int64) (*Admin, error) {
	var a Admin
	var created, mustChange int64
	err := s.db.QueryRow(`SELECT id, username, created_at, must_change_password, role FROM admins WHERE id = ?`, id).
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

// UserGroupIDs lists the groups an account may reach, sorted.
func (s *Store) UserGroupIDs(userID int64) ([]int64, error) {
	rows, err := s.db.Query(`SELECT group_id FROM user_groups WHERE user_id = ?`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	ids := []int64{}
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	return ids, rows.Err()
}

// UpdateUser changes the role and groups of an account. The last
// administrator cannot be demoted. Other sessions of the account stay valid:
// the role and groups are read on every request, so the change is immediate.
func (s *Store) UpdateUser(id int64, role string, groupIDs []int64) error {
	if !validRole(role) {
		return ErrInvalid
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var current string
	if err := tx.QueryRow(`SELECT role FROM admins WHERE id = ?`, id).Scan(&current); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		return err
	}
	if current == RoleAdmin && role != RoleAdmin {
		if n, err := s.adminCount(tx); err != nil {
			return err
		} else if n <= 1 {
			return ErrLastAdmin
		}
	}
	if _, err := tx.Exec(`UPDATE admins SET role = ? WHERE id = ?`, role, id); err != nil {
		return err
	}
	if err := setGroups(tx, id, role, groupIDs); err != nil {
		return err
	}
	return tx.Commit()
}

// DeleteUser removes an account and its sessions. The last administrator
// cannot be deleted.
func (s *Store) DeleteUser(id int64) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var role string
	if err := tx.QueryRow(`SELECT role FROM admins WHERE id = ?`, id).Scan(&role); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		return err
	}
	if role == RoleAdmin {
		if n, err := s.adminCount(tx); err != nil {
			return err
		} else if n <= 1 {
			return ErrLastAdmin
		}
	}
	if _, err := tx.Exec(`DELETE FROM admins WHERE id = ?`, id); err != nil {
		return err
	}
	return tx.Commit()
}

// CanAccessMachine reports whether the account may open a terminal on m:
// administrators always, operators only when the machine is in one of their groups.
func (s *Store) CanAccessMachine(a *Admin, m *Machine) (bool, error) {
	if a.IsAdmin() {
		return true, nil
	}
	if m.GroupID == nil {
		return false, nil
	}
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM user_groups WHERE user_id = ? AND group_id = ?`, a.ID, *m.GroupID).Scan(&n)
	return n > 0, err
}
