package store

import (
	"database/sql"
	"fmt"
	"time"
)

// Claim is one row of machine_claim, read back for the running strip (to show a launch that has
// not yet been confirmed by a fresh provider answer) and for the watcher (to reconcile an open
// one against what a provider just said).
//
// ModelName, ImageOrTemplate, ServeArgs and ServeEnvKeys are the serving record: what a claim
// asked to be served. ServeEnvKeys is the comma-separated NAMES of the environment variables the
// create sends, never a value. ServingState is `""`, `"loading"`, `"serving"` or `"gave_up"`
// (see schema.sql).
type Claim struct {
	ID           int64
	ProviderKey  string
	ShapeCode    string
	Location     string
	Hostname     string
	PricePerHour *float64
	AskedAt      string
	ProviderID   *string
	State        string
	Detail       *string
	// Project is the project slug this claim was launched for.
	Project         string
	ModelName       string
	ImageOrTemplate string
	ServeArgs       string
	ServeEnvKeys    string
	ServingState    string
	LoadingSince    *string
	ServingSince    *string
	// ConfirmedAt is settled_at: the moment a fresh answer confirmed this claim
	// (MarkClaimConfirmed), read back by ClaimsAwaitingServing so a resumed readiness poll can
	// judge which later live-poll answers are fresh enough to trust that the machine has
	// vanished (see console.pollServingReadiness); and, on a claim settled 'refused' or
	// 'abandoned', the moment it settled. Left unpopulated by OpenClaims and ServingClaims, which
	// have no use for it.
	ConfirmedAt *string
	// StartupScriptID is the Verda startup script this claim's create used, nil on RunPod or
	// before it is created; kept as history after the script is deleted.
	StartupScriptID *string
	// EngineVersion is what the serving engine reported at readiness ("vllm <version>" or
	// "llama.cpp <build_info>"), "" before readiness or when it could not be read.
	EngineVersion string
	// IdleMinutes is this machine's idle timeout, 0 meaning never. Read by GetClaim only.
	IdleMinutes int
}

// RecordClaim writes down one launch, in the 'asked' state, and commits it. This runs before the
// provider is asked to create anything, so a process that dies right after still leaves a row
// saying exactly what was being rented. If this cannot be written, nothing is asked.
//
// location is what was ASKED for: "any", "any-eu", "any-us", or one location name, never the
// place the provider actually chose, which is not known yet. SettleClaimCreated records that once
// the create answers.
//
// modelName, imageOrTemplate, serveArgs and serveEnvKeys are the serving record: what a claim
// asked to be served, written before the create so the record says what was asked for, not only
// what machine. serveEnvKeys is the comma-separated NAMES of the environment variables the create
// sends; a value must never be passed here, least of all the machine's VLLM_API_KEY.
//
// project is the slug this claim is launched for, already validated by the caller against the
// project table.
//
// launchRequest is the launch's structured request as JSON (console.launchRequest), kept so the
// auto-move can rebuild a launch for a cheaper shape; it holds no credential.
//
// idleMinutes is this machine's idle timeout, written with the claim like everything else.
func (s *Store) RecordClaim(providerKey, shapeCode, location, hostname, project string, pricePerHour *float64, modelName, imageOrTemplate, serveArgs, serveEnvKeys, launchRequest string, idleMinutes int) (int64, error) {
	res, err := s.db.Exec(
		`INSERT INTO machine_claim (provider_key, shape_code, location, hostname, price_per_hour, asked_at, project, state, model_name, image_or_template, serve_args, serve_env_keys, launch_request, idle_minutes)
		 VALUES (?, ?, ?, ?, ?, ?, ?, 'asked', ?, ?, ?, ?, ?, ?)`,
		providerKey, shapeCode, location, hostname, pricePerHour, time.Now().UTC().Format(TimeLayout),
		project, modelName, imageOrTemplate, serveArgs, serveEnvKeys, launchRequest, idleMinutes,
	)
	if err != nil {
		return 0, fmt.Errorf("record the claim for %s/%s: %w", providerKey, shapeCode, err)
	}
	return res.LastInsertId()
}

// SettleClaimCreated writes that the provider answered a claim with a real machine id. The claim
// stays visible on the strip as "starting up" (state 'created') until a fresh poll confirms the
// machine (MarkClaimConfirmed); our own create call succeeding is never the proof.
//
// actualLocation is the data centre the provider's create answer actually named, never the
// "any", "any-eu", "any-us" or name this claim's location column already holds from what was
// asked. It is written into detail so a look at the claim says both what was asked for and what
// was chosen. "" (the provider's answer named none, for example when Verda's read-back failed) is
// recorded plainly rather than guessed at.
func (s *Store) SettleClaimCreated(claimID int64, providerID, actualLocation string) error {
	detail := "the provider's own answer named no location"
	if actualLocation != "" {
		detail = "created in " + actualLocation
	}
	_, err := s.db.Exec(
		`UPDATE machine_claim SET provider_id = ?, state = 'created', detail = ?, settled_at = ? WHERE id = ?`,
		providerID, detail, time.Now().UTC().Format(TimeLayout), claimID,
	)
	if err != nil {
		return fmt.Errorf("settle claim %d as created: %w", claimID, err)
	}
	return nil
}

// SettleClaimRefused writes that the provider refused the claim outright: nothing was created.
func (s *Store) SettleClaimRefused(claimID int64, detail string) error {
	_, err := s.db.Exec(
		`UPDATE machine_claim SET state = 'refused', detail = ?, settled_at = ? WHERE id = ?`,
		detail, time.Now().UTC().Format(TimeLayout), claimID,
	)
	if err != nil {
		return fmt.Errorf("settle claim %d as refused: %w", claimID, err)
	}
	return nil
}

// OpenClaims returns every claim the watcher still needs to reconcile: 'asked' (the provider
// has not answered, or the process asking it died before it could record the answer) and
// 'created' (a provider_id is known but no fresh poll has confirmed it yet).
func (s *Store) OpenClaims() ([]Claim, error) {
	rows, err := s.db.Query(
		`SELECT id, provider_key, shape_code, location, hostname, price_per_hour, asked_at, provider_id, state, detail,
		        project, model_name, image_or_template, serve_args, serve_env_keys, serving_state, loading_since, serving_since,
		        startup_script_id, engine_version
		 FROM machine_claim WHERE state IN ('asked', 'created') ORDER BY id`,
	)
	if err != nil {
		return nil, fmt.Errorf("read the open claims: %w", err)
	}
	defer rows.Close()
	var claims []Claim
	for rows.Next() {
		var c Claim
		if err := rows.Scan(&c.ID, &c.ProviderKey, &c.ShapeCode, &c.Location, &c.Hostname, &c.PricePerHour, &c.AskedAt, &c.ProviderID, &c.State, &c.Detail,
			&c.Project, &c.ModelName, &c.ImageOrTemplate, &c.ServeArgs, &c.ServeEnvKeys, &c.ServingState, &c.LoadingSince, &c.ServingSince,
			&c.StartupScriptID, &c.EngineVersion); err != nil {
			return nil, fmt.Errorf("read an open claim row: %w", err)
		}
		claims = append(claims, c)
	}
	return claims, rows.Err()
}

// ClaimsAwaitingServing returns every confirmed claim, on either provider, whose model was asked
// to be served but whose readiness poll has not yet seen it answer: state 'confirmed',
// serving_state 'loading'. Read once at start-up: a machine that outlives GPUGate keeps running,
// and starting GPUGate again resumes waiting for it rather than forgetting it.
func (s *Store) ClaimsAwaitingServing() ([]Claim, error) {
	rows, err := s.db.Query(
		`SELECT id, provider_key, shape_code, location, hostname, price_per_hour, asked_at, provider_id, state, detail,
		        project, model_name, image_or_template, serve_args, serve_env_keys, serving_state, loading_since, serving_since, settled_at,
		        startup_script_id, engine_version
		 FROM machine_claim WHERE state = 'confirmed' AND serving_state = 'loading' ORDER BY id`,
	)
	if err != nil {
		return nil, fmt.Errorf("read the claims awaiting serving: %w", err)
	}
	defer rows.Close()
	var claims []Claim
	for rows.Next() {
		var c Claim
		if err := rows.Scan(&c.ID, &c.ProviderKey, &c.ShapeCode, &c.Location, &c.Hostname, &c.PricePerHour, &c.AskedAt, &c.ProviderID, &c.State, &c.Detail,
			&c.Project, &c.ModelName, &c.ImageOrTemplate, &c.ServeArgs, &c.ServeEnvKeys, &c.ServingState, &c.LoadingSince, &c.ServingSince, &c.ConfirmedAt,
			&c.StartupScriptID, &c.EngineVersion); err != nil {
			return nil, fmt.Errorf("read a claim awaiting serving: %w", err)
		}
		claims = append(claims, c)
	}
	return claims, rows.Err()
}

// ServingClaims answers the confirmed claims on project whose model the readiness poll has seen
// serving, on either provider, for /api/projects and the fixed address's routing (only claims of
// the project a /p/<slug> request named). Same rows as ClaimsAwaitingServing, one state later.
func (s *Store) ServingClaims(project string) ([]Claim, error) {
	rows, err := s.db.Query(
		`SELECT id, provider_key, shape_code, location, hostname, price_per_hour, asked_at, provider_id, state, detail,
		        project, model_name, image_or_template, serve_args, serve_env_keys, serving_state, loading_since, serving_since,
		        startup_script_id, engine_version
		 FROM machine_claim WHERE project = ? AND state = 'confirmed' AND serving_state = 'serving' ORDER BY id`,
		project,
	)
	if err != nil {
		return nil, fmt.Errorf("read the serving claims on %s: %w", project, err)
	}
	defer rows.Close()
	var claims []Claim
	for rows.Next() {
		var c Claim
		if err := rows.Scan(&c.ID, &c.ProviderKey, &c.ShapeCode, &c.Location, &c.Hostname, &c.PricePerHour, &c.AskedAt, &c.ProviderID, &c.State, &c.Detail,
			&c.Project, &c.ModelName, &c.ImageOrTemplate, &c.ServeArgs, &c.ServeEnvKeys, &c.ServingState, &c.LoadingSince, &c.ServingSince,
			&c.StartupScriptID, &c.EngineVersion); err != nil {
			return nil, fmt.Errorf("read a serving claim on %s: %w", project, err)
		}
		claims = append(claims, c)
	}
	return claims, rows.Err()
}

// MarkClaimLoading writes that claimID's machine has been confirmed by a fresh answer and the
// readiness poll for its model is starting now. It only ever leaves `""` once, so a second call
// (the same claim reconciled on a later live-poll pass, before its goroutine has set 'serving')
// never resets loading_since.
func (s *Store) MarkClaimLoading(claimID int64) error {
	_, err := s.db.Exec(
		`UPDATE machine_claim SET serving_state = 'loading', loading_since = ? WHERE id = ? AND serving_state = ''`,
		time.Now().UTC().Format(TimeLayout), claimID,
	)
	if err != nil {
		return fmt.Errorf("mark claim %d loading: %w", claimID, err)
	}
	return nil
}

// MarkClaimServing writes that a fresh answer from the machine's model list (never this
// program's own say-so) has now listed the model claimID asked to serve, and stores the engine
// version read at readiness; "" when it could not be read.
func (s *Store) MarkClaimServing(claimID int64, engineVersion string) error {
	_, err := s.db.Exec(
		`UPDATE machine_claim SET serving_state = 'serving', serving_since = ?, engine_version = ? WHERE id = ?`,
		time.Now().UTC().Format(TimeLayout), engineVersion, claimID,
	)
	if err != nil {
		return fmt.Errorf("mark claim %d serving: %w", claimID, err)
	}
	return nil
}

// SetClaimStartupScript records the Verda startup script id created for claimID, before the
// instance is asked for.
func (s *Store) SetClaimStartupScript(claimID int64, scriptID string) error {
	_, err := s.db.Exec(`UPDATE machine_claim SET startup_script_id = ? WHERE id = ?`, scriptID, claimID)
	if err != nil {
		return fmt.Errorf("record the startup script for claim %d: %w", claimID, err)
	}
	return nil
}

// ClaimStartupScriptID answers claimID's startup script id, nil when it never had one (a RunPod
// claim, or a Verda claim not yet that far). Read by the script-delete helper so callers along
// the readiness poll and the watcher need only a claim id, never the whole Claim row, to know
// what to delete.
func (s *Store) ClaimStartupScriptID(claimID int64) (*string, error) {
	var id sql.NullString
	err := s.db.QueryRow(`SELECT startup_script_id FROM machine_claim WHERE id = ?`, claimID).Scan(&id)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read the startup script id for claim %d: %w", claimID, err)
	}
	if !id.Valid || id.String == "" {
		return nil, nil
	}
	v := id.String
	return &v, nil
}

// SetSSHHostKey pins providerID's SSH host key, in OpenSSH authorized_keys form, onto the newest
// claim for it on providerKey. Called once, the first time this program's tunnel to a Verda
// machine accepts a host key with none pinned yet.
func (s *Store) SetSSHHostKey(providerKey, providerID, line string) error {
	_, err := s.db.Exec(
		`UPDATE machine_claim SET ssh_host_key = ? WHERE id = (
		     SELECT id FROM machine_claim WHERE provider_key = ? AND provider_id = ? ORDER BY id DESC LIMIT 1
		 )`,
		line, providerKey, providerID,
	)
	if err != nil {
		return fmt.Errorf("pin the SSH host key for %s/%s: %w", providerKey, providerID, err)
	}
	return nil
}

// SSHHostKey answers providerID's pinned SSH host key on providerKey, "" when none is pinned yet.
// The tunnel's host-key policy reads it on every connect and reconnect, including after a
// restart.
func (s *Store) SSHHostKey(providerKey, providerID string) (string, error) {
	var line sql.NullString
	err := s.db.QueryRow(
		`SELECT ssh_host_key FROM machine_claim WHERE provider_key = ? AND provider_id = ? ORDER BY id DESC LIMIT 1`,
		providerKey, providerID,
	).Scan(&line)
	if err == sql.ErrNoRows {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("read the pinned SSH host key for %s/%s: %w", providerKey, providerID, err)
	}
	return line.String, nil
}

// MarkClaimGaveUp writes that the readiness poll stopped waiting because the machine will never
// serve: no key on file, the machine exited, a direct read found it not found, or the one-hour
// ceiling passed (the caller destroys the machine in all but the no-key case). It leaves
// `settled_at` and `detail` untouched: this is not a claim outcome (state stays 'confirmed'),
// only a note that nothing more will be waited for. Excluded from ClaimsAwaitingServing's
// 'loading' and ServingClaims' 'serving' alike, so a restart never resumes it and it never reads
// as serving.
func (s *Store) MarkClaimGaveUp(claimID int64) error {
	_, err := s.db.Exec(
		`UPDATE machine_claim SET serving_state = 'gave_up' WHERE id = ?`,
		claimID,
	)
	if err != nil {
		return fmt.Errorf("mark claim %d gave up: %w", claimID, err)
	}
	return nil
}

// ServingInfo is one claim's serving progress, read back by the running strip so a machine row
// can show "running, loading the model" or "serving <model>" instead of the plain "running"
// chip, and when that state was entered.
type ServingInfo struct {
	ModelName    string
	ServingState string // "loading", "serving", or "gave_up"
	Since        string
	// LastOutputAt is the flushed value of last_output_at: the moment the fixed address last
	// forwarded a byte back from this machine, "" when no proxied request has ever reached it.
	// The inactivity sweep and the strip's quiet clock both read this alongside Since.
	LastOutputAt string
	// Project is the project slug this machine is serving under (the running strip names it on
	// every serving machine's line).
	Project string
	// IdleMinutes is the claim's idle timeout, 0 meaning never.
	IdleMinutes int
	// ClaimID is the claim this machine was launched under; the strip's card carries it so the
	// page can tell its own launch's machine from any other.
	ClaimID int64
}

// ServingInfoByProviderID returns, for every machine on providerKey currently loading, serving,
// or given up on a model, its ServingInfo, keyed by provider_id. The running strip looks a
// machine's provider_id up in this map on every render; a machine absent from it draws the plain
// "running" chip, as for any machine without a claim. A caller that must treat only 'serving' as
// serving (the inactivity sweep, the fixed address's separate ServingClaims query) checks
// ServingState itself.
func (s *Store) ServingInfoByProviderID(providerKey string) (map[string]ServingInfo, error) {
	rows, err := s.db.Query(
		`SELECT provider_id, model_name, serving_state, loading_since, serving_since, last_output_at, project, idle_minutes, id
		 FROM machine_claim
		 WHERE provider_key = ? AND provider_id IS NOT NULL AND serving_state IN ('loading', 'serving', 'gave_up')`,
		providerKey,
	)
	if err != nil {
		return nil, fmt.Errorf("read %s's serving info: %w", providerKey, err)
	}
	defer rows.Close()
	out := map[string]ServingInfo{}
	for rows.Next() {
		var providerID, modelName, servingState, project string
		var idleMinutes int
		var claimID int64
		var loadingSince, servingSince, lastOutputAt sql.NullString
		if err := rows.Scan(&providerID, &modelName, &servingState, &loadingSince, &servingSince, &lastOutputAt, &project, &idleMinutes, &claimID); err != nil {
			return nil, fmt.Errorf("read a serving info row for %s: %w", providerKey, err)
		}
		since := loadingSince.String
		if servingState == "serving" {
			since = servingSince.String
		}
		out[providerID] = ServingInfo{ModelName: modelName, ServingState: servingState, Since: since, LastOutputAt: lastOutputAt.String, Project: project, IdleMinutes: idleMinutes, ClaimID: claimID}
	}
	return out, rows.Err()
}

// SetClaimIdleMinutes changes the idle timeout of the claim loading or serving providerID on
// providerKey, 0 meaning never, and answers false when no claim there is loading or serving. A
// loading machine's figure takes effect once it serves, because the sweep only judges serving
// claims; it reads the figure on its next pass.
func (s *Store) SetClaimIdleMinutes(providerKey, providerID string, minutes int) (bool, error) {
	res, err := s.db.Exec(
		`UPDATE machine_claim SET idle_minutes = ? WHERE provider_key = ? AND provider_id = ? AND serving_state IN ('loading', 'serving')`,
		minutes, providerKey, providerID,
	)
	if err != nil {
		return false, fmt.Errorf("set the idle timeout of %s/%s: %w", providerKey, providerID, err)
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// SetLastOutputAt writes the moment providerID's serving machine last forwarded a byte back
// through the fixed address. Called once per live-poll pass, from the console's flush of its
// in-memory activity clock and never from the proxy's hot path. Scoped to a currently-serving
// claim: a claim that has since stopped serving has nothing left to move.
func (s *Store) SetLastOutputAt(providerKey, providerID, at string) error {
	_, err := s.db.Exec(
		`UPDATE machine_claim SET last_output_at = ? WHERE provider_key = ? AND provider_id = ? AND serving_state = 'serving'`,
		at, providerKey, providerID,
	)
	if err != nil {
		return fmt.Errorf("record the last output time for %s/%s: %w", providerKey, providerID, err)
	}
	return nil
}

// AddTokenCounts adds proxied calls' token counts to providerID's running totals. Like
// SetLastOutputAt it is flushed once per live-poll pass and never called from the proxy's hot
// path, and it is scoped to a currently-serving claim: a claim that has since stopped serving has
// nothing left to add to.
func (s *Store) AddTokenCounts(providerKey, providerID string, in, out, cached int) error {
	_, err := s.db.Exec(
		`UPDATE machine_claim SET tokens_in = tokens_in + ?, tokens_out = tokens_out + ?, tokens_cached = tokens_cached + ?
		 WHERE provider_key = ? AND provider_id = ? AND serving_state = 'serving'`,
		in, out, cached, providerKey, providerID,
	)
	if err != nil {
		return fmt.Errorf("add token counts for %s/%s: %w", providerKey, providerID, err)
	}
	return nil
}

// ServingStateForProviderID answers the most recent claim's serving_state for providerID on
// providerKey ("" for never asked to serve, "loading", "serving" or "gave_up") and whether any
// claim at all is on file for it. Read by the destroy path to decide whether a machine ever
// reached serving before it is torn down, without asking any provider anything.
func (s *Store) ServingStateForProviderID(providerKey, providerID string) (state string, found bool, err error) {
	err = s.db.QueryRow(
		`SELECT serving_state FROM machine_claim WHERE provider_key = ? AND provider_id = ? ORDER BY id DESC LIMIT 1`,
		providerKey, providerID,
	).Scan(&state)
	if err == sql.ErrNoRows {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("read the serving state for %s/%s: %w", providerKey, providerID, err)
	}
	return state, true, nil
}

// MarkClaimConfirmed writes that a fresh provider answer has now listed this claim's machine.
// Once confirmed, the running strip reads the machine from the `machine` table like any other
// and stops drawing this claim's "starting up" line.
func (s *Store) MarkClaimConfirmed(claimID int64) error {
	_, err := s.db.Exec(
		`UPDATE machine_claim SET state = 'confirmed', settled_at = ? WHERE id = ?`,
		time.Now().UTC().Format(TimeLayout), claimID,
	)
	if err != nil {
		return fmt.Errorf("mark claim %d confirmed: %w", claimID, err)
	}
	return nil
}

// MarkClaimAbandoned writes that this claim's hour ran out with no machine ever confirmed for it,
// on either provider. The claim stops appearing on the strip; the log line that reaches this is
// where the fact is kept.
func (s *Store) MarkClaimAbandoned(claimID int64, detail string) error {
	_, err := s.db.Exec(
		`UPDATE machine_claim SET state = 'abandoned', detail = ?, settled_at = ? WHERE id = ?`,
		detail, time.Now().UTC().Format(TimeLayout), claimID,
	)
	if err != nil {
		return fmt.Errorf("mark claim %d abandoned: %w", claimID, err)
	}
	return nil
}
