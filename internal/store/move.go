package store

import (
	"database/sql"
	"fmt"
	"time"
)

// This file is the auto-move's storage: the per-project switch, the launch request kept on a
// claim, and the move record that links an old claim to its replacement. Why each exists is in
// schema.sql.

// SetProjectAutoMove switches auto-move on or off for slug. A move already under way is a
// machine_move row and is not touched.
func (s *Store) SetProjectAutoMove(slug string, on bool) error {
	v := 0
	if on {
		v = 1
	}
	res, err := s.db.Exec("UPDATE project SET auto_move = ? WHERE slug = ?", v, slug)
	if err != nil {
		return fmt.Errorf("set auto-move for project %s: %w", slug, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return sql.ErrNoRows
	}
	return nil
}

// AutoMoveProjects answers the slugs of every project whose auto-move switch is on.
func (s *Store) AutoMoveProjects() ([]string, error) {
	rows, err := s.db.Query("SELECT slug FROM project WHERE auto_move = 1 ORDER BY created_at")
	if err != nil {
		return nil, fmt.Errorf("read the auto-move projects: %w", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var slug string
		if err := rows.Scan(&slug); err != nil {
			return nil, fmt.Errorf("read an auto-move project row: %w", err)
		}
		out = append(out, slug)
	}
	return out, rows.Err()
}

// ClaimLaunchRequest answers the structured launch request stored on claimID, "" for a claim that
// never stored one (such a claim is never moved).
func (s *Store) ClaimLaunchRequest(claimID int64) (string, error) {
	var req string
	err := s.db.QueryRow("SELECT launch_request FROM machine_claim WHERE id = ?", claimID).Scan(&req)
	if err == sql.ErrNoRows {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("read the launch request of claim %d: %w", claimID, err)
	}
	return req, nil
}

// GetClaim answers one claim by id and whether it exists.
func (s *Store) GetClaim(claimID int64) (Claim, bool, error) {
	var c Claim
	err := s.db.QueryRow(
		`SELECT id, provider_key, shape_code, location, hostname, price_per_hour, asked_at, provider_id, state, detail,
		        project, model_name, image_or_template, serve_args, serve_env_keys, serving_state, loading_since, serving_since, settled_at,
		        startup_script_id, engine_version, idle_minutes
		 FROM machine_claim WHERE id = ?`, claimID,
	).Scan(&c.ID, &c.ProviderKey, &c.ShapeCode, &c.Location, &c.Hostname, &c.PricePerHour, &c.AskedAt, &c.ProviderID, &c.State, &c.Detail,
		&c.Project, &c.ModelName, &c.ImageOrTemplate, &c.ServeArgs, &c.ServeEnvKeys, &c.ServingState, &c.LoadingSince, &c.ServingSince, &c.ConfirmedAt,
		&c.StartupScriptID, &c.EngineVersion, &c.IdleMinutes)
	if err == sql.ErrNoRows {
		return Claim{}, false, nil
	}
	if err != nil {
		return Claim{}, false, fmt.Errorf("read claim %d: %w", claimID, err)
	}
	return c, true, nil
}

// GetClaimMachine answers claimID with only the fields that name its machine and what it costs
// (ProviderKey, ProviderID, ShapeCode, Hostname, PricePerHour, ServingState), read by its own
// short query. The mover falls back to it to destroy a move's new machine when GetClaim keeps
// failing, so a broken wide read can never leave a rented machine with nobody able to name it.
func (s *Store) GetClaimMachine(claimID int64) (Claim, bool, error) {
	c := Claim{ID: claimID}
	err := s.db.QueryRow(
		`SELECT provider_key, shape_code, hostname, price_per_hour, provider_id, serving_state
		 FROM machine_claim WHERE id = ?`, claimID,
	).Scan(&c.ProviderKey, &c.ShapeCode, &c.Hostname, &c.PricePerHour, &c.ProviderID, &c.ServingState)
	if err == sql.ErrNoRows {
		return Claim{}, false, nil
	}
	if err != nil {
		return Claim{}, false, fmt.Errorf("read the machine of claim %d: %w", claimID, err)
	}
	return c, true, nil
}

// SetClaimLastOutputAt writes claimID's last_output_at directly, for the mover carrying the old
// machine's quiet clock onto its replacement.
func (s *Store) SetClaimLastOutputAt(claimID int64, at string) error {
	_, err := s.db.Exec("UPDATE machine_claim SET last_output_at = ? WHERE id = ?", at, claimID)
	if err != nil {
		return fmt.Errorf("set the last output time of claim %d: %w", claimID, err)
	}
	return nil
}

// Move is one row of machine_move.
type Move struct {
	ID         int64
	Project    string
	OldClaimID int64
	NewClaimID int64
	State      string
	StartedAt  string
	SwitchedAt string
}

// RecordMove writes and commits a move record in state 'starting'. The mover calls it after the
// new claim row exists and before the provider is asked to create the machine.
func (s *Store) RecordMove(project string, oldClaimID, newClaimID int64) (int64, error) {
	res, err := s.db.Exec(
		`INSERT INTO machine_move (project, old_claim_id, new_claim_id, state, started_at) VALUES (?, ?, ?, 'starting', ?)`,
		project, oldClaimID, newClaimID, time.Now().UTC().Format(TimeLayout),
	)
	if err != nil {
		return 0, fmt.Errorf("record the move of project %s: %w", project, err)
	}
	return res.LastInsertId()
}

// OpenMoves answers every move still 'starting' or 'draining', oldest first.
func (s *Store) OpenMoves() ([]Move, error) {
	rows, err := s.db.Query(
		`SELECT id, project, old_claim_id, new_claim_id, state, started_at, switched_at
		 FROM machine_move WHERE state IN ('starting', 'draining') ORDER BY id`,
	)
	if err != nil {
		return nil, fmt.Errorf("read the open moves: %w", err)
	}
	defer rows.Close()
	var out []Move
	for rows.Next() {
		var m Move
		var switched sql.NullString
		if err := rows.Scan(&m.ID, &m.Project, &m.OldClaimID, &m.NewClaimID, &m.State, &m.StartedAt, &switched); err != nil {
			return nil, fmt.Errorf("read a move row: %w", err)
		}
		m.SwitchedAt = switched.String
		out = append(out, m)
	}
	return out, rows.Err()
}

// ProjectHasOpenMove reports whether project has a move still under way.
func (s *Store) ProjectHasOpenMove(project string) (bool, error) {
	var one int
	err := s.db.QueryRow("SELECT 1 FROM machine_move WHERE project = ? AND state IN ('starting', 'draining') LIMIT 1", project).Scan(&one)
	if err == sql.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("check for an open move on %s: %w", project, err)
	}
	return true, nil
}

// ClaimIsOldOfSwitchedMove reports whether claimID is the old machine of a move whose new machine
// already serves (draining or done), so that machine is being replaced on purpose.
func (s *Store) ClaimIsOldOfSwitchedMove(claimID int64) (bool, error) {
	var one int
	err := s.db.QueryRow("SELECT 1 FROM machine_move WHERE old_claim_id = ? AND state IN ('draining', 'done') LIMIT 1", claimID).Scan(&one)
	if err == sql.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("check whether claim %d was moved: %w", claimID, err)
	}
	return true, nil
}

// ClaimIsNewOfMove reports whether claimID is the new machine's claim of any move, whatever the
// move's state: the rental the mover itself asked for, never one launched from the console.
func (s *Store) ClaimIsNewOfMove(claimID int64) (bool, error) {
	var one int
	err := s.db.QueryRow("SELECT 1 FROM machine_move WHERE new_claim_id = ? LIMIT 1", claimID).Scan(&one)
	if err == sql.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("check whether claim %d belongs to a move: %w", claimID, err)
	}
	return true, nil
}

// MarkMoveDraining writes that the new machine serves and new requests go to it.
func (s *Store) MarkMoveDraining(moveID int64) error {
	_, err := s.db.Exec("UPDATE machine_move SET state = 'draining', switched_at = ? WHERE id = ?",
		time.Now().UTC().Format(TimeLayout), moveID)
	if err != nil {
		return fmt.Errorf("mark move %d draining: %w", moveID, err)
	}
	return nil
}

// FinishMove settles moveID as 'done' or 'failed' with a one-sentence detail.
func (s *Store) FinishMove(moveID int64, state, detail string) error {
	_, err := s.db.Exec("UPDATE machine_move SET state = ?, finished_at = ?, detail = ? WHERE id = ?",
		state, time.Now().UTC().Format(TimeLayout), detail, moveID)
	if err != nil {
		return fmt.Errorf("settle move %d as %s: %w", moveID, state, err)
	}
	return nil
}

// DestroyIntentState answers whether any destroy_intent row exists for providerID on providerKey,
// and whether that row is confirmed gone (gone_at set).
func (s *Store) DestroyIntentState(providerKey, providerID string) (exists, gone bool, err error) {
	var goneAt sql.NullString
	err = s.db.QueryRow(
		"SELECT gone_at FROM destroy_intent WHERE provider_key = ? AND provider_id = ?",
		providerKey, providerID,
	).Scan(&goneAt)
	if err == sql.ErrNoRows {
		return false, false, nil
	}
	if err != nil {
		return false, false, fmt.Errorf("read the destroy intent for %s/%s: %w", providerKey, providerID, err)
	}
	return true, goneAt.Valid, nil
}
