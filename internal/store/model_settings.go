package store

import "fmt"

// ModelSettingKeys are the keys `model_setting` may hold: the launch panel's context and chat
// boxes, the five sampling values, gpu_memory_utilization, and thinking (`1` on or `0` off, never
// saved means on). A key outside this list is never read or written by the functions below, so a
// typo in a caller cannot silently create an unused row.
var ModelSettingKeys = []string{
	"context_thousands", "chats", "temperature", "top_k", "top_p", "min_p",
	"repeat_penalty", "gpu_memory_utilization", "thinking",
}

// ModelSettings answers every saved row for modelName, keyed by its setting name. A key absent
// from the map has no saved row ("a missing row means not set").
func (s *Store) ModelSettings(modelName string) (map[string]string, error) {
	rows, err := s.db.Query("SELECT key, value FROM model_setting WHERE model_name = ?", modelName)
	if err != nil {
		return nil, fmt.Errorf("read model settings for %s: %w", modelName, err)
	}
	defer rows.Close()
	out := make(map[string]string)
	for rows.Next() {
		var key, value string
		if err := rows.Scan(&key, &value); err != nil {
			return nil, fmt.Errorf("scan a model setting for %s: %w", modelName, err)
		}
		out[key] = value
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read model settings for %s: %w", modelName, err)
	}
	return out, nil
}

// SaveModelSettings writes modelName's new saved settings from values: a key ModelSettingKeys
// names is saved when values holds a non-empty string for it, and its row is deleted otherwise
// (an empty or absent value deletes that key's row). A key values carries that ModelSettingKeys
// does not name is ignored.
func (s *Store) SaveModelSettings(modelName string, values map[string]string) error {
	for _, key := range ModelSettingKeys {
		value, present := values[key]
		if present && value != "" {
			if _, err := s.db.Exec(
				`INSERT INTO model_setting (model_name, key, value) VALUES (?, ?, ?)
				 ON CONFLICT(model_name, key) DO UPDATE SET value = excluded.value`,
				modelName, key, value,
			); err != nil {
				return fmt.Errorf("save %s for %s: %w", key, modelName, err)
			}
			continue
		}
		if _, err := s.db.Exec(
			"DELETE FROM model_setting WHERE model_name = ? AND key = ?", modelName, key,
		); err != nil {
			return fmt.Errorf("clear %s for %s: %w", key, modelName, err)
		}
	}
	return nil
}
