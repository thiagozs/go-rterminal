package store

import "time"

type AuditEntry struct {
	ID     int64     `json:"id"`
	At     time.Time `json:"at"`
	Actor  string    `json:"actor"`
	Action string    `json:"action"`
	Target string    `json:"target"`
	Detail string    `json:"detail"`
}

func (s *Store) AddAudit(actor, action, target, detail string) error {
	_, err := s.db.Exec(`INSERT INTO audit_log(at, actor, action, target, detail) VALUES(?,?,?,?,?)`,
		unix(time.Now()), actor, action, target, detail)
	return err
}

// ListAudit returns the newest entries first.
func (s *Store) ListAudit(limit int) ([]AuditEntry, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	rows, err := s.db.Query(`SELECT id, at, actor, action, target, detail FROM audit_log ORDER BY id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	entries := []AuditEntry{}
	for rows.Next() {
		var e AuditEntry
		var at int64
		if err := rows.Scan(&e.ID, &at, &e.Actor, &e.Action, &e.Target, &e.Detail); err != nil {
			return nil, err
		}
		e.At = fromUnix(at)
		entries = append(entries, e)
	}
	return entries, rows.Err()
}
