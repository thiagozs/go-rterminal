package store

import (
	"database/sql"
	"errors"
	"time"
)

// Recording is the catalogue entry of a recorded terminal session; the
// content lives in a file managed by the recording package. Machine names are
// copied so a recording stays readable after the machine is deleted.
type Recording struct {
	ID          string     `json:"id"`
	MachineID   string     `json:"machine_id"`
	MachineName string     `json:"machine_name"`
	Username    string     `json:"username"`
	StartedAt   time.Time  `json:"started_at"`
	EndedAt     *time.Time `json:"ended_at"` // nil while the session is running
	Bytes       int64      `json:"bytes"`
	Truncated   bool       `json:"truncated"` // stopped at the size limit
}

const recordingColumns = `id, machine_id, machine_name, username, started_at, ended_at, bytes, truncated`

func scanRecording(r scanner) (*Recording, error) {
	var rec Recording
	var started, truncated int64
	var ended sql.NullInt64
	if err := r.Scan(&rec.ID, &rec.MachineID, &rec.MachineName, &rec.Username, &started, &ended, &rec.Bytes, &truncated); err != nil {
		return nil, err
	}
	rec.StartedAt = fromUnix(started)
	if ended.Valid {
		t := fromUnix(ended.Int64)
		rec.EndedAt = &t
	}
	rec.Truncated = truncated != 0
	return &rec, nil
}

func (s *Store) AddRecording(rec Recording) error {
	_, err := s.db.Exec(`INSERT INTO recordings(`+recordingColumns+`) VALUES(?,?,?,?,?,NULL,0,0)`,
		rec.ID, rec.MachineID, rec.MachineName, rec.Username, unix(rec.StartedAt))
	return err
}

// FinishRecording marks the end of a session and its final size.
func (s *Store) FinishRecording(id string, ended time.Time, bytes int64, truncated bool) error {
	_, err := s.db.Exec(`UPDATE recordings SET ended_at = ?, bytes = ?, truncated = ? WHERE id = ?`,
		unix(ended), bytes, boolInt(truncated), id)
	return err
}

// ListRecordings returns the newest first; machineID filters when not empty.
func (s *Store) ListRecordings(machineID string, limit int) ([]Recording, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	query := `SELECT ` + recordingColumns + ` FROM recordings`
	args := []any{}
	if machineID != "" {
		query += ` WHERE machine_id = ?`
		args = append(args, machineID)
	}
	rows, err := s.db.Query(query+` ORDER BY started_at DESC, id LIMIT ?`, append(args, limit)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	list := []Recording{}
	for rows.Next() {
		rec, err := scanRecording(rows)
		if err != nil {
			return nil, err
		}
		list = append(list, *rec)
	}
	return list, rows.Err()
}

func (s *Store) GetRecording(id string) (*Recording, error) {
	rec, err := scanRecording(s.db.QueryRow(`SELECT `+recordingColumns+` FROM recordings WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return rec, err
}

func (s *Store) DeleteRecording(id string) error {
	res, err := s.db.Exec(`DELETE FROM recordings WHERE id = ?`, id)
	if err != nil {
		return err
	}
	return affected(res)
}

// RecordingsStartedBefore lists recordings that began before t (retention).
func (s *Store) RecordingsStartedBefore(t time.Time) ([]string, error) {
	return s.recordingIDs(`SELECT id FROM recordings WHERE started_at < ?`, unix(t))
}

// UnfinishedRecordings lists recordings without an end, i.e. running now or
// interrupted by a crash or restart.
func (s *Store) UnfinishedRecordings() ([]Recording, error) {
	rows, err := s.db.Query(`SELECT ` + recordingColumns + ` FROM recordings WHERE ended_at IS NULL`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var list []Recording
	for rows.Next() {
		rec, err := scanRecording(rows)
		if err != nil {
			return nil, err
		}
		list = append(list, *rec)
	}
	return list, rows.Err()
}

func (s *Store) recordingIDs(query string, args ...any) ([]string, error) {
	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}
