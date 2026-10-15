package store

import (
	"fmt"
	"time"

	"github.com/DavidTimar1/gpugate/internal/provider"
)

// ReplaceProviderMachines overwrites what this provider's last answer said it holds. It runs
// only after a provider answered successfully, and it replaces the whole set for that provider
// in one transaction, so the table never mixes rows from two different answers.
func (s *Store) ReplaceProviderMachines(providerKey string, machines []provider.RunningMachine) error {
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("begin replacing %s's machines: %w", providerKey, err)
	}
	defer tx.Rollback()
	if _, err := tx.Exec("DELETE FROM machine WHERE provider_key = ?", providerKey); err != nil {
		return fmt.Errorf("clear %s's machines: %w", providerKey, err)
	}
	now := time.Now().UTC().Format(TimeLayout)
	for _, m := range machines {
		if _, err := tx.Exec(
			`INSERT INTO machine (provider_key, provider_id, shape_code, display_name, gpu_name, location, price_per_hour, state, last_seen_at)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			m.ProviderKey, m.ProviderID, m.ShapeCode, m.DisplayName, m.GPUName, m.Location, m.PricePerHour, m.State, now,
		); err != nil {
			return fmt.Errorf("write %s's machine %s: %w", providerKey, m.ProviderID, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit %s's machines: %w", providerKey, err)
	}
	return nil
}

// LastKnownMachines returns what the last successful answer from providerKey said it held, and
// when that answer arrived. It is used only when that provider could not be asked just now:
// these are our notes, not a fresh answer, and the caller marks them unconfirmed rather than
// showing them as running.
func (s *Store) LastKnownMachines(providerKey string) ([]provider.RunningMachine, string, error) {
	rows, err := s.db.Query(
		`SELECT provider_id, shape_code, display_name, gpu_name, location, price_per_hour, state, last_seen_at
		 FROM machine WHERE provider_key = ? ORDER BY provider_id`,
		providerKey,
	)
	if err != nil {
		return nil, "", fmt.Errorf("read %s's last known machines: %w", providerKey, err)
	}
	defer rows.Close()
	var machines []provider.RunningMachine
	var lastSeenAt string
	for rows.Next() {
		var m provider.RunningMachine
		m.ProviderKey = providerKey
		if err := rows.Scan(&m.ProviderID, &m.ShapeCode, &m.DisplayName, &m.GPUName, &m.Location, &m.PricePerHour, &m.State, &lastSeenAt); err != nil {
			return nil, "", fmt.Errorf("read a machine row for %s: %w", providerKey, err)
		}
		machines = append(machines, m)
	}
	return machines, lastSeenAt, rows.Err()
}
