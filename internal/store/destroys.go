package store

import (
	"database/sql"
	"fmt"
	"time"
)

// RecordDestroyIntent writes down that providerID on providerKey is about to be destroyed, and
// commits it. This runs before the provider is asked, so a process that dies right after still
// leaves a row saying a destroy was in flight for this exact machine.
func (s *Store) RecordDestroyIntent(providerKey, providerID, displayName string, pricePerHour *float64) error {
	_, err := s.db.Exec(
		`INSERT INTO destroy_intent (provider_key, provider_id, display_name, price_per_hour, asked_at, gone_at)
		 VALUES (?, ?, ?, ?, ?, NULL)
		 ON CONFLICT(provider_key, provider_id) DO UPDATE SET
		   display_name = excluded.display_name,
		   price_per_hour = excluded.price_per_hour,
		   asked_at = excluded.asked_at,
		   gone_at = NULL`,
		providerKey, providerID, displayName, pricePerHour, time.Now().UTC().Format(TimeLayout),
	)
	if err != nil {
		return fmt.Errorf("record the destroy intent for %s/%s: %w", providerKey, providerID, err)
	}
	return nil
}

// MarkDestroyGone writes the moment a fresh provider answer first confirmed providerID no longer
// exists on providerKey. It is never called on our own destroy call succeeding alone, only on a
// later ListMachines answer that no longer carries the machine.
func (s *Store) MarkDestroyGone(providerKey, providerID string) error {
	_, err := s.db.Exec(
		"UPDATE destroy_intent SET gone_at = ? WHERE provider_key = ? AND provider_id = ?",
		time.Now().UTC().Format(TimeLayout), providerKey, providerID,
	)
	if err != nil {
		return fmt.Errorf("mark %s/%s gone: %w", providerKey, providerID, err)
	}
	return nil
}

// HasOpenDestroyIntent reports whether providerID on providerKey already has a destroy_intent row
// with no gone_at. It is the inactivity sweep's guard against firing twice for the same machine,
// including across a restart that would otherwise have lost the in-memory destroyNotices that
// guard it the rest of the time.
func (s *Store) HasOpenDestroyIntent(providerKey, providerID string) (bool, error) {
	var exists int
	err := s.db.QueryRow(
		"SELECT 1 FROM destroy_intent WHERE provider_key = ? AND provider_id = ? AND gone_at IS NULL",
		providerKey, providerID,
	).Scan(&exists)
	if err == sql.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("check the destroy intent for %s/%s: %w", providerKey, providerID, err)
	}
	return true, nil
}

// OpenDestroyIntent is one destroy_intent row with no gone_at yet: a destroy that was asked for
// and never confirmed done, whether the process is still retrying it or died before it could.
type OpenDestroyIntent struct {
	ProviderKey  string
	ProviderID   string
	DisplayName  string
	PricePerHour *float64
	AskedAt      string
}

// OpenDestroyIntents lists every destroy_intent row with no gone_at, for resuming a destroy still
// in flight when the process that started it restarted before a fresh answer confirmed the
// machine gone.
func (s *Store) OpenDestroyIntents() ([]OpenDestroyIntent, error) {
	rows, err := s.db.Query(
		"SELECT provider_key, provider_id, display_name, price_per_hour, asked_at FROM destroy_intent WHERE gone_at IS NULL",
	)
	if err != nil {
		return nil, fmt.Errorf("read the open destroy intents: %w", err)
	}
	defer rows.Close()
	var out []OpenDestroyIntent
	for rows.Next() {
		var in OpenDestroyIntent
		if err := rows.Scan(&in.ProviderKey, &in.ProviderID, &in.DisplayName, &in.PricePerHour, &in.AskedAt); err != nil {
			return nil, fmt.Errorf("read an open destroy intent row: %w", err)
		}
		out = append(out, in)
	}
	return out, rows.Err()
}
