package store

import (
	"database/sql"
	"errors"
	"strings"
	"time"
)

type Group struct {
	ID          int64     `json:"id"`
	Name        string    `json:"name"`
	Description string    `json:"description"`
	CreatedAt   time.Time `json:"created_at"`
}

func (s *Store) CreateGroup(name, description string) (*Group, error) {
	name = strings.TrimSpace(name)
	if name == "" || len(name) > 64 {
		return nil, ErrInvalid
	}
	now := time.Now()
	res, err := s.db.Exec(`INSERT INTO groups(name, description, created_at) VALUES(?,?,?)`,
		name, strings.TrimSpace(description), unix(now))
	if err != nil {
		if isUnique(err) {
			return nil, ErrConflict
		}
		return nil, err
	}
	id, _ := res.LastInsertId()
	return &Group{ID: id, Name: name, Description: strings.TrimSpace(description), CreatedAt: now.UTC()}, nil
}

func (s *Store) ListGroups() ([]Group, error) {
	rows, err := s.db.Query(`SELECT id, name, description, created_at FROM groups ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	groups := []Group{}
	for rows.Next() {
		var g Group
		var created int64
		if err := rows.Scan(&g.ID, &g.Name, &g.Description, &created); err != nil {
			return nil, err
		}
		g.CreatedAt = fromUnix(created)
		groups = append(groups, g)
	}
	return groups, rows.Err()
}

func (s *Store) GetGroup(id int64) (*Group, error) {
	var g Group
	var created int64
	err := s.db.QueryRow(`SELECT id, name, description, created_at FROM groups WHERE id = ?`, id).
		Scan(&g.ID, &g.Name, &g.Description, &created)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	g.CreatedAt = fromUnix(created)
	return &g, err
}

func (s *Store) UpdateGroup(id int64, name, description string) error {
	name = strings.TrimSpace(name)
	if name == "" || len(name) > 64 {
		return ErrInvalid
	}
	res, err := s.db.Exec(`UPDATE groups SET name = ?, description = ? WHERE id = ?`,
		name, strings.TrimSpace(description), id)
	if err != nil {
		if isUnique(err) {
			return ErrConflict
		}
		return err
	}
	return affected(res)
}

// DeleteGroup removes the group; its machines become ungrouped.
func (s *Store) DeleteGroup(id int64) error {
	res, err := s.db.Exec(`DELETE FROM groups WHERE id = ?`, id)
	if err != nil {
		return err
	}
	return affected(res)
}

func affected(res sql.Result) error {
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}
