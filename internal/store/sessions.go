package store

import (
	"database/sql"
	"fmt"
)

// sessionsSinceKey is the one row `setting` holds for the Sessions view's "Clear list" button:
// the moment it was last pressed. Its value is UTC to the second in the same shape as every other
// stored timestamp (RecordClaim's `asked_at`, SetLastOutputAt, and so on), so ListSessions can
// compare it against `machine_claim.asked_at` as a plain SQL string `>=`: that fixed-width,
// zero-padded, always-UTC shape sorts lexicographically in the order it sorts chronologically. A
// missing row means "never cleared": every claim is listed.
const sessionsSinceKey = "sessions_since"

// SessionsSince returns the moment Clear list was last pressed, "" when it never has been.
// Unset means no lower bound at all, never a default figure.
func (s *Store) SessionsSince() (string, error) {
	var value string
	err := s.db.QueryRow("SELECT value FROM setting WHERE key = ?", sessionsSinceKey).Scan(&value)
	if err == sql.ErrNoRows {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("read the sessions-since setting: %w", err)
	}
	return value, nil
}

// SetSessionsSince writes at, UTC to the second, as the new sessions_since setting.
func (s *Store) SetSessionsSince(at string) error {
	_, err := s.db.Exec(
		`INSERT INTO setting (key, value) VALUES (?, ?)
		 ON CONFLICT(key) DO UPDATE SET value = excluded.value`,
		sessionsSinceKey, at,
	)
	if err != nil {
		return fmt.Errorf("write the sessions-since setting: %w", err)
	}
	return nil
}

// Session is one rental's whole life, past or present, for GET /api/sessions: the timing and
// cost machine_claim and destroy_intent carry, joined by provider_id, plus the three token
// counters. GoneAt is nil both while a claim is still running and when its end was never
// confirmed by a fresh answer. ListSessions cannot tell the two apart, and console.handleSessions
// reads ProviderID and ServingState to say which.
type Session struct {
	ID           int64
	Project      string
	ModelName    string
	ProviderKey  string
	ProviderID   *string
	ShapeCode    string
	Location     string
	PricePerHour *float64
	AskedAt      string
	ServingState string
	ServingSince *string
	GoneAt       *string
	TokensIn     int
	TokensOut    int
	TokensCached int
}

// ListSessions answers up to limit claims that ever reached 'created', newest first. 'refused'
// and 'abandoned' claims never reached a real machine and are excluded by the state filter. A
// claim still 'created' (a provider_id exists, no fresh poll has confirmed it yet) is listed too:
// a machine may already be billing for it.
//
// since is the sessions_since setting, "" for no lower bound (every claim). It is compared to
// mc.asked_at as a plain SQL string >=, which works because both have the same UTC-to-the-second
// shape (see sessionsSinceKey). This never deletes a row: Clear list only moves where the list
// starts reading from.
func (s *Store) ListSessions(limit int, since string) ([]Session, error) {
	rows, err := s.db.Query(
		`SELECT mc.id, mc.project, mc.model_name, mc.provider_key, mc.provider_id, mc.shape_code, mc.location,
		        mc.price_per_hour, mc.asked_at, mc.serving_state, mc.serving_since, di.gone_at,
		        mc.tokens_in, mc.tokens_out, mc.tokens_cached
		 FROM machine_claim mc
		 LEFT JOIN destroy_intent di ON di.provider_key = mc.provider_key AND di.provider_id = mc.provider_id
		 WHERE mc.state IN ('created', 'confirmed') AND (? = '' OR mc.asked_at >= ?)
		 ORDER BY mc.asked_at DESC
		 LIMIT ?`,
		since, since, limit,
	)
	if err != nil {
		return nil, fmt.Errorf("read the sessions: %w", err)
	}
	defer rows.Close()
	var out []Session
	for rows.Next() {
		var sess Session
		if err := rows.Scan(&sess.ID, &sess.Project, &sess.ModelName, &sess.ProviderKey, &sess.ProviderID, &sess.ShapeCode, &sess.Location,
			&sess.PricePerHour, &sess.AskedAt, &sess.ServingState, &sess.ServingSince, &sess.GoneAt,
			&sess.TokensIn, &sess.TokensOut, &sess.TokensCached); err != nil {
			return nil, fmt.Errorf("read a session row: %w", err)
		}
		out = append(out, sess)
	}
	return out, rows.Err()
}
