package store

import (
	"database/sql"
	"fmt"
	"time"
)

// SetShapePermission records whether shapeCode is ticked, account-wide. `checked`
// true inserts or keeps the row; false removes it, so the ticked set is exactly the rows
// present.
func (s *Store) SetShapePermission(shapeCode string, checked bool) error {
	if checked {
		_, err := s.db.Exec(
			`INSERT INTO shape_tick (shape_code, ticked_at) VALUES (?, ?)
			 ON CONFLICT(shape_code) DO NOTHING`,
			shapeCode, time.Now().UTC().Format(TimeLayout),
		)
		if err != nil {
			return fmt.Errorf("tick shape %s: %w", shapeCode, err)
		}
		return nil
	}
	_, err := s.db.Exec("DELETE FROM shape_tick WHERE shape_code = ?", shapeCode)
	if err != nil {
		return fmt.Errorf("untick shape %s: %w", shapeCode, err)
	}
	return nil
}

// IsShapeTicked reports whether shapeCode is currently ticked, account-wide.
func (s *Store) IsShapeTicked(shapeCode string) (bool, error) {
	var exists int
	err := s.db.QueryRow("SELECT 1 FROM shape_tick WHERE shape_code = ?", shapeCode).Scan(&exists)
	if err == sql.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("read shape tick %s: %w", shapeCode, err)
	}
	return true, nil
}

// TickedShapeCodes returns every shape code currently ticked, account-wide, as a set.
// One query rather than one per shape, for the table's whole row list at once.
func (s *Store) TickedShapeCodes() (map[string]bool, error) {
	rows, err := s.db.Query("SELECT shape_code FROM shape_tick")
	if err != nil {
		return nil, fmt.Errorf("read shape ticks: %w", err)
	}
	defer rows.Close()
	ticked := map[string]bool{}
	for rows.Next() {
		var code string
		if err := rows.Scan(&code); err != nil {
			return nil, fmt.Errorf("read a shape tick row: %w", err)
		}
		ticked[code] = true
	}
	return ticked, rows.Err()
}
