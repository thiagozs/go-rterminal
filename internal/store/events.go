package store

import (
	"time"
)

const (
	// maxMachineEvents and machineEventRetention bound the connection
	// history kept per machine.
	maxMachineEvents      = 1000
	machineEventRetention = 30 * 24 * time.Hour
)

// MachineEvent is one entry of a machine's connection history. Source is
// "agent" (what the agent noticed) or "server" (what the server observed).
type MachineEvent struct {
	ID        int64     `json:"id"`
	MachineID string    `json:"machine_id"`
	At        time.Time `json:"at"`
	Source    string    `json:"source"`
	Kind      string    `json:"kind"`
	Detail    string    `json:"detail"`
}

// AddMachineEvents stores events for a machine and trims its history. Event
// times come from the agent's clock, so one in the future is clamped to now.
func (s *Store) AddMachineEvents(machineID, source string, events []MachineEvent) error {
	if len(events) == 0 {
		return nil
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	now := time.Now()
	for _, e := range events {
		at := e.At
		if at.IsZero() || at.After(now) {
			at = now
		}
		if _, err := tx.Exec(`INSERT INTO machine_events (machine_id, at, source, kind, detail) VALUES (?, ?, ?, ?, ?)`,
			machineID, at.UnixMilli(), source, trimTo(e.Kind, 32), trimTo(e.Detail, 700)); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(`DELETE FROM machine_events WHERE machine_id = ? AND (at < ? OR id NOT IN (
		SELECT id FROM machine_events WHERE machine_id = ? ORDER BY at DESC, id DESC LIMIT ?))`,
		machineID, now.Add(-machineEventRetention).UnixMilli(), machineID, maxMachineEvents); err != nil {
		return err
	}
	return tx.Commit()
}

// ListMachineEvents returns the newest events of a machine first.
func (s *Store) ListMachineEvents(machineID string, limit int) ([]MachineEvent, error) {
	if limit <= 0 || limit > maxMachineEvents {
		limit = 200
	}
	rows, err := s.db.Query(`SELECT id, machine_id, at, source, kind, detail FROM machine_events
		WHERE machine_id = ? ORDER BY at DESC, id DESC LIMIT ?`, machineID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []MachineEvent{}
	for rows.Next() {
		var e MachineEvent
		var at int64
		if err := rows.Scan(&e.ID, &e.MachineID, &at, &e.Source, &e.Kind, &e.Detail); err != nil {
			return nil, err
		}
		e.At = time.UnixMilli(at).UTC()
		out = append(out, e)
	}
	return out, rows.Err()
}
