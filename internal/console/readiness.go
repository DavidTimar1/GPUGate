package console

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"syscall"
	"time"

	"github.com/DavidTimar1/gpugate/internal/podkey"
	"github.com/DavidTimar1/gpugate/internal/provider"
	"github.com/DavidTimar1/gpugate/internal/store"
)

// servingReadinessPollInterval is how often the readiness poll asks a confirmed machine's model
// list whether it has finished loading.
const servingReadinessPollInterval = 15 * time.Second

// servingReadinessCeiling bounds how long the readiness poll waits for a machine's model list to
// answer before giving up. Every launch clock is one hour.
const servingReadinessCeiling = time.Hour

// resumeServingPolls restarts the readiness poll for every claim, on either provider, still
// 'loading' when this process starts. It runs once, after New's first fresh pollRunningOnce, so
// a claim whose machine has vanished since the last run is not resumed pointlessly;
// pollServingReadiness checks a fresh answer on every pass regardless.
func (s *Server) resumeServingPolls() {
	claims, err := s.store.ClaimsAwaitingServing()
	if err != nil {
		s.log.Add("console", "could not read the claims awaiting serving: "+err.Error())
		return
	}
	for _, c := range claims {
		if c.ProviderID == nil || *c.ProviderID == "" {
			continue
		}
		// confirmedAt is when a fresh answer confirmed this claim, before the restart (settled_at,
		// see store.MarkClaimConfirmed). When it cannot be read back, the current time is used
		// instead: it is at least as late as the true confirm time, so it never trusts an answer
		// any staler than the true one would allow.
		confirmedAt := time.Now()
		if c.ConfirmedAt != nil {
			if parsed, err := time.Parse(store.TimeLayout, *c.ConfirmedAt); err == nil {
				confirmedAt = parsed
			}
		}
		// baseURL: the first fresh answer's machine, "" when it is not there yet. That is
		// Verda's case, where pollServingReadiness waits for the tunnel to connect.
		// ensureTunnels (pollRunningOnce) rebuilds this restart's tunnels from the same fresh
		// answer, so a claim resumed here finds its tunnel already being dialed.
		baseURL := ""
		if m, ok := s.machineByID(c.ProviderKey, *c.ProviderID); ok {
			baseURL, _ = s.servingBaseURL(m)
		}
		s.log.Add("launch", "resuming the readiness poll for "+c.ModelName+" on "+*c.ProviderID+" after a restart.")
		s.startReadinessPoll(c.ID, c.ProviderKey, *c.ProviderID, baseURL, c.ModelName, s.byName[c.ModelName].HFRepo, confirmedAt)
	}
}

// startReadinessPoll begins polling providerID's serving address for hfRepo in the background,
// settling claimID's serving_state as it goes (store.MarkClaimServing). Called once a claim is
// first confirmed running, and again for any claim still 'loading' after a restart. baseURL is
// the matched machine's ServingBaseURL: always known on RunPod, "" on Verda until a fresh answer
// names one, which pollServingReadiness waits for. confirmedAt is the moment a fresh answer
// confirmed this claim's machine; only a live-poll answer asked for after it is trusted to say
// the machine has vanished (see pollServingReadiness).
//
// A no-op when claimID already has an active poller: pollRunningOnce carries no lock against
// running concurrently with itself, so reconcileClaims's call (from the periodic ticker) can
// race runLaunch's explicit "ask for one now" call (both landing on the same claim still
// 'created'), and each would otherwise start its own goroutine: two pollers for one claim, each
// logging its own status line forever. readinessPollers is cleared by pollServingReadiness on
// every return path, so a later restart (resumeServingPolls) still starts a fresh one.
func (s *Server) startReadinessPoll(claimID int64, providerKey, providerID, baseURL, modelName, hfRepo string, confirmedAt time.Time) {
	s.readinessPollersMu.Lock()
	if s.readinessPollers[claimID] {
		s.readinessPollersMu.Unlock()
		return
	}
	s.readinessPollers[claimID] = true
	s.readinessPollersMu.Unlock()
	go s.pollServingReadiness(claimID, providerKey, providerID, baseURL, modelName, hfRepo, confirmedAt)
}

// servingVanishMissesNeeded is how many consecutive FRESH live-poll answers, each asked for
// after confirmedAt and so at least one live-poll interval apart, must miss a machine before the
// readiness poll even considers it vanished, and only then if the direct read still cannot find
// it. One fresh miss is never enough: a live poll whose request began before the machine was
// confirmed can still land after and correctly carry no such machine. That is a stale answer,
// not proof.
const servingVanishMissesNeeded = 2

// readinessStatusLogInterval is how often, while a machine is being polled for readiness, the
// wait writes a status line: at most one this often, never one per poll
// (servingReadinessPollInterval is far tighter, and a line every 15 seconds would drown the log
// in "still waiting").
const readinessStatusLogInterval = 2 * time.Minute

// podLogCheckpoint answers the elapsed-wait duration at which the n-th (0-based) machine-log read
// happens: five minutes, fifteen minutes, then every five minutes after that.
func podLogCheckpoint(n int) time.Duration {
	switch {
	case n == 0:
		return 5 * time.Minute
	case n == 1:
		return 15 * time.Minute
	default:
		return 15*time.Minute + time.Duration(n-1)*5*time.Minute
	}
}

// pollServingReadiness asks providerID's model list, on either provider, every
// servingReadinessPollInterval until it lists hfRepo, up to servingReadinessCeiling, counted from
// confirmedAt so a program restarted past the ceiling destroys the machine at once instead of
// waiting another hour. While baseURL is "" (Verda has not yet named an address), each pass
// instead re-reads the latest fresh answer for it, logging at the status cadence below without
// naming the address itself; the first non-empty answer starts the model-list attempts. It
// stops early, settling the claim 'gave_up' (giveUpOnClaim, which also deletes a Verda claim's
// startup script) so a restart never resumes it, if no key is on file for the machine. It
// destroys the machine as well (destroyUndeployedClaim) if a FRESH live-poll answer, asked for
// after confirmedAt, shows it exited; or if such answers miss it servingVanishMissesNeeded times
// running and a direct read (podGoneByDirectRead) cannot find it either; or if the ceiling
// passes without it serving. A stale answer (asked for before confirmedAt) is skipped, never
// counted as a miss. A machine still loading before the ceiling is never destroyed.
//
// While it waits, the status line (logReadinessStatus) fires at most every
// readinessStatusLogInterval, and the machine-log read (logPodLogTail; over the SSH tunnel on
// Verda, the provider's log call on RunPod) fires on podLogCheckpoint's schedule. Both name the
// machine by its provider id only, never its URL or key: a failed model-list request is
// classified, never printed as an error string that could carry the machine's address.
func (s *Server) pollServingReadiness(claimID int64, providerKey, providerID, baseURL, modelName, hfRepo string, confirmedAt time.Time) {
	defer func() {
		s.readinessPollersMu.Lock()
		delete(s.readinessPollers, claimID)
		s.readinessPollersMu.Unlock()
	}()
	who := s.rentalPrefix(claimID)
	deadline := confirmedAt.Add(servingReadinessCeiling)
	apiKey, err := podkey.Read(providerKey, providerID)
	if err != nil {
		s.log.Add("launch", who+"no key on file for "+providerID+", so it could not be reached; giving up on "+modelName+" ever loading.")
		s.giveUpOnClaim(claimID, providerKey, providerID)
		return
	}
	ref := machineRef{providerKey, providerID}
	if !s.canReadLog(ref) {
		s.log.Add("launch", who+providerLabel(providerKey)+" "+providerID+": no log endpoint on "+providerLabel(providerKey)+"; the box keeps its own at /var/log/gpugate-serve.log.")
	}
	format := s.byName[modelName].Format
	freshMisses := 0
	var lastMissAskedAt time.Time
	waitStart := time.Now()
	nextStatusLogAt := waitStart.Add(readinessStatusLogInterval)
	podLogCalls := 0
	stage := ""
	var hang hangWatch
	hangAnnounced := false

	for {
		// A machine the auto-move replaced while it loaded is destroyed on purpose: the wait ends
		// cleanly and says so, instead of reading the machine's going away as a failure to load.
		if moved, err := s.store.ClaimIsOldOfSwitchedMove(claimID); err == nil && moved {
			if exists, _, err := s.store.DestroyIntentState(providerKey, providerID); err == nil && exists {
				s.log.Add("launch", who+providerLabel(providerKey)+" "+providerID+" was moved to a cheaper machine while it loaded "+modelName+"; no longer waiting for it.")
				s.deleteStartupScriptForClaim(providerKey, claimID)
				s.closeTunnel(machineRef{providerKey, providerID})
				return
			}
		}
		if st, fresh := s.freshAnswer(providerKey, confirmedAt); fresh {
			present, exited := podState(st.machines, providerID)
			switch {
			case exited:
				s.log.Add("launch", who+providerLabel(providerKey)+" "+providerID+" exited before "+modelName+" ever answered its own model list; it failed to load, so it is destroyed.")
				s.destroyUndeployedClaim(claimID, providerKey, providerID, "it exited before "+modelName+" ever answered its own model list")
				return
			case present:
				freshMisses = 0
				for _, m := range st.machines {
					if m.ProviderID == providerID {
						baseURL, stage = s.servingBaseURL(m)
						break
					}
				}
			case st.askedAt.After(lastMissAskedAt):
				// A new fresh answer, distinct from the last one already counted, still misses
				// the machine; servingVanishMissesNeeded of these in a row are required.
				freshMisses++
				lastMissAskedAt = st.askedAt
				s.log.Add("launch", who+providerLabel(providerKey)+" "+providerID+" is missing from a fresh answer asked for after it was confirmed ("+strconv.Itoa(freshMisses)+" of "+strconv.Itoa(servingVanishMissesNeeded)+" needed); still waiting for "+modelName+" to load.")
				if freshMisses >= servingVanishMissesNeeded {
					if s.podGoneByDirectRead(providerKey, providerID) {
						// A destroy record (open or closed) means someone asked for this destroy,
						// the command line included, so the vanish is not a failure to load and
						// the provider is not asked again.
						if exists, _, err := s.store.DestroyIntentState(providerKey, providerID); err == nil && exists {
							s.log.Add("launch", who+providerLabel(providerKey)+" "+providerID+" was destroyed by request while it loaded "+modelName+"; no longer waiting for it.")
							s.giveUpOnClaim(claimID, providerKey, providerID)
							return
						}
						s.log.Add("launch", who+providerLabel(providerKey)+" "+providerID+" could not be found on a direct read; giving up on "+modelName+" ever loading; destroying it.")
						s.destroyUndeployedClaim(claimID, providerKey, providerID, "it vanished from fresh answers before "+modelName+" ever answered its own model list")
						return
					}
					s.log.Add("launch", who+providerLabel(providerKey)+" "+providerID+" missed "+strconv.Itoa(freshMisses)+" fresh answers in a row, but a direct read still finds it; still waiting for "+modelName+" to load.")
					freshMisses = 0
				}
			}
		}

		now := time.Now()
		if baseURL == "" || stage != "" {
			// Two stages read the same way: no address named yet (baseURL == "", stage == ""), or
			// an address named but the SSH tunnel to it not yet connected (stage names why, via
			// Tunnel.LastAttempt). Neither puts the machine's address in the log line.
			if !now.Before(nextStatusLogAt) {
				if baseURL == "" {
					s.log.Add("launch", who+providerLabel(providerKey)+" has not yet named an address for "+providerID+"; still waiting for "+modelName+" to load.")
				} else {
					s.log.Add("launch", who+providerLabel(providerKey)+" "+providerID+": "+stage+"; still waiting for "+modelName+" to load.")
				}
				nextStatusLogAt = now.Add(readinessStatusLogInterval)
			}
			if now.After(deadline) {
				if baseURL == "" {
					s.log.Add("launch", who+providerLabel(providerKey)+" never named an address for "+providerID+" within "+servingReadinessCeiling.String()+", so loading "+modelName+" failed; destroying it.")
				} else {
					s.log.Add("launch", who+providerLabel(providerKey)+" "+providerID+" never finished "+stage+" within "+servingReadinessCeiling.String()+", so loading "+modelName+" failed; destroying it.")
				}
				s.destroyUndeployedClaim(claimID, providerKey, providerID, "it did not get past starting up within "+servingReadinessCeiling.String())
				return
			}
			time.Sleep(servingReadinessPollInterval)
			continue
		}

		ready, attempt := s.modelReady(baseURL+"/v1", apiKey, hfRepo)
		if ready {
			engineVersion, versionErr := s.engineVersion(baseURL, apiKey, format)
			if err := s.store.MarkClaimServing(claimID, engineVersion); err != nil {
				s.log.Add("console", "could not record that claim "+strconv.FormatInt(claimID, 10)+" is serving: "+err.Error())
			}
			versionText := engineVersion
			if versionText == "" {
				versionText = "an unknown engine version (" + versionErr + ")"
			}
			s.log.Add("launch", who+providerLabel(providerKey)+" "+providerID+" is now serving "+modelName+" on "+versionText+".")
			s.deleteStartupScriptForClaim(providerKey, claimID)
			s.lookForCheaperNow()
			return
		}

		if !now.Before(nextStatusLogAt) {
			s.logReadinessStatus(who, providerKey, providerID, modelName, now.Sub(waitStart), attempt)
			nextStatusLogAt = now.Add(readinessStatusLogInterval)
		}
		for now.Sub(waitStart) >= podLogCheckpoint(podLogCalls) {
			if lines, ok := s.logPodLogTail(who, providerKey, providerID, modelName); ok {
				hang.observe(lines, now)
			}
			podLogCalls++
		}
		// Hang give-up: once the log has shown vLLM's hang message it is read every minute; ten
		// minutes of nothing else, at least hangMinLines of them, destroys the machine.
		if hang.dueForRead(now) {
			if lines, err := s.readRedactedPodLog(providerKey, providerID, hangLogTail); err == nil {
				hang.observe(lines, now)
			} else {
				hang.lastRead = now // a failed read is tried again next minute, never counted as quiet
			}
		}
		if hang.active() && !hangAnnounced {
			hangAnnounced = true
			s.log.Add("launch", who+providerLabel(providerKey)+" "+providerID+"'s log shows vLLM's hang message; reading it every minute, and the machine is destroyed if nothing else is printed for "+hangGiveUpAfter.String()+".")
		}
		if hang.shouldGiveUp(now) {
			s.log.Add("launch", who+providerLabel(providerKey)+" "+providerID+" has printed nothing but vLLM's hang message ("+strconv.Itoa(hang.hangLines)+" times) for "+hangGiveUpAfter.String()+", so loading "+modelName+" will not finish; destroying it.")
			s.destroyUndeployedClaim(claimID, providerKey, providerID, "its log showed only vLLM's hang message for "+hangGiveUpAfter.String())
			return
		}

		if now.After(deadline) {
			s.log.Add("launch", who+providerLabel(providerKey)+" "+providerID+" never answered its own model list for "+modelName+" within "+servingReadinessCeiling.String()+", so loading it failed; destroying it.")
			s.destroyUndeployedClaim(claimID, providerKey, providerID, "it did not serve "+modelName+" within "+servingReadinessCeiling.String())
			return
		}
		time.Sleep(servingReadinessPollInterval)
	}
}

// logReadinessStatus writes the status line: how long the wait has run, what the last model-list
// attempt answered, and the machine's runtime.uptime from a fresh direct read (RunPod only;
// Verda names no uptime). The machine is named by its provider id only, never its address or
// key.
func (s *Server) logReadinessStatus(who, providerKey, providerID, modelName string, waited time.Duration, lastAttempt string) {
	uptimeText := "no direct read is available for its uptime"
	if getter := s.machineGetter(providerKey); getter != nil {
		ctx, cancel := context.WithTimeout(context.Background(), providerCallTimeout)
		machine, err := getter.GetMachine(ctx, providerID)
		cancel()
		switch {
		case err != nil:
			uptimeText = "its uptime could not be read: " + err.Error()
		case machine.UptimeSeconds != nil:
			uptimeText = "uptime " + (time.Duration(*machine.UptimeSeconds) * time.Second).String()
		default:
			uptimeText = "no uptime named yet"
		}
	}
	s.log.Add("launch", who+providerLabel(providerKey)+" "+providerID+" still waiting for "+modelName+" after "+
		waited.Round(time.Second).String()+"; last model-list attempt: "+lastAttempt+"; "+uptimeText+".")
}

// logPodLogTail reads providerID's container log (redacted, capped; see readRedactedPodLog for
// how each provider's is reached) and writes the last ten lines into the program's log under the
// machine's id. A read that fails is said plainly once; the loop's next checkpoint tries again
// rather than retrying inside this call. It answers the lines it read, for the hang watch
// (hangwatch.go), and false when the read failed.
func (s *Server) logPodLogTail(who, providerKey, providerID, modelName string) ([]string, bool) {
	lines, err := s.readRedactedPodLog(providerKey, providerID, hangLogTail)
	if err != nil {
		s.log.Add("launch", who+providerLabel(providerKey)+" "+providerID+": could not read its own log while waiting for "+modelName+": "+err.Error())
		return nil, false
	}
	last := lines
	if len(last) > 10 {
		last = last[len(last)-10:]
	}
	s.log.Add("launch", who+providerLabel(providerKey)+" "+providerID+"'s own log, last "+strconv.Itoa(len(last))+" line(s), while waiting for "+modelName+":")
	for _, l := range last {
		s.log.Add("launch", who+providerLabel(providerKey)+" "+providerID+": "+l)
	}
	return lines, true
}

// rentalPrefix is what a log line about claimID's rental starts with so a reader can tell who
// asked for it: "" for a launch made from the console, "Auto-move for '<project>': " for the new
// machine of an auto-move (store.ClaimIsNewOfMove is true from the moment its create is sent).
func (s *Server) rentalPrefix(claimID int64) string {
	fromMove, err := s.store.ClaimIsNewOfMove(claimID)
	if err != nil {
		s.log.Add("console", "could not tell whether claim "+strconv.FormatInt(claimID, 10)+" belongs to a move: "+err.Error())
		return ""
	}
	if !fromMove {
		return ""
	}
	project := "?"
	if c, found, err := s.store.GetClaim(claimID); err == nil && found {
		project = c.Project
	}
	return "Auto-move for '" + project + "': "
}

// destroyUndeployedClaim ends a rental that cannot be deployed: it settles the claim as given up
// (giveUpOnClaim) and destroys the machine through the same intent-first path Delete uses
// (RecordDestroyIntent, then runDestroy), the reason written into the log. The new machine of an
// auto-move is only settled here: the mover destroys its own new machine itself (mover.go,
// failMove). An open destroy intent already on the machine means something is destroying it, so
// nothing more is asked.
func (s *Server) destroyUndeployedClaim(claimID int64, providerKey, providerID, reason string) {
	s.giveUpOnClaim(claimID, providerKey, providerID)
	fromMove, err := s.store.ClaimIsNewOfMove(claimID)
	if err != nil {
		s.log.Add("console", "could not tell whether claim "+strconv.FormatInt(claimID, 10)+" belongs to a move: "+err.Error())
	}
	if fromMove {
		return
	}
	target := provider.ByKey(s.providers, providerKey)
	if target == nil {
		s.log.Add("console", "claim "+strconv.FormatInt(claimID, 10)+" names an unknown provider '"+providerKey+"'; "+providerID+" is not destroyed.")
		return
	}
	open, err := s.store.HasOpenDestroyIntent(providerKey, providerID)
	if err != nil {
		s.log.Add("console", "could not check for an open destroy of "+providerID+": "+err.Error())
		return
	}
	if open {
		return
	}
	displayName := providerID
	var price *float64
	if c, found, err := s.store.GetClaim(claimID); err == nil && found {
		displayName = c.ShapeCode + " (" + c.Hostname + ")"
		price = c.PricePerHour
	}
	if err := s.store.RecordDestroyIntent(providerKey, providerID, displayName, price); err != nil {
		s.log.Add("console", "could not record the destroy intent for "+displayName+"; nothing was asked of "+target.DisplayName()+": "+err.Error())
		return
	}
	s.log.Add("instances", "destroying "+displayName+" ("+providerID+") on "+target.DisplayName()+": "+reason+".")
	s.setDestroyNotice(providerKey, providerID, "destroying: "+reason)
	go s.runDestroy(target, providerID, displayName, true, time.Now())
}

// giveUpOnClaim settles claimID's serving_state as 'gave_up' (store.MarkClaimGaveUp) once the
// readiness poll has decided providerID's machine will never serve, so a restart never resumes
// waiting on it. It also deletes the claim's Verda startup script and closes its SSH tunnel
// (both no-ops on RunPod).
func (s *Server) giveUpOnClaim(claimID int64, providerKey, providerID string) {
	if err := s.store.MarkClaimGaveUp(claimID); err != nil {
		s.log.Add("console", "could not settle claim "+strconv.FormatInt(claimID, 10)+" for "+providerID+" as gave up: "+err.Error())
	}
	s.deleteStartupScriptForClaim(providerKey, claimID)
	s.closeTunnel(machineRef{providerKey, providerID})
}

// engineVersion reads the model server's version at readiness: GET <baseURL>/version on vLLM
// (safetensors, fp8), GET <baseURL>/props on llama.cpp (gguf, RunPod only). "" with a reason when
// it could not be read (a bad status, an unreachable machine, or an answer that does not parse),
// never guessed at.
func (s *Server) engineVersion(baseURL, apiKey, format string) (version, failReason string) {
	path := "/version"
	if format == "gguf" {
		path = "/props"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+path, nil)
	if err != nil {
		return "", "could not build the request"
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)
	resp, err := s.proxyClient.Do(req)
	if err != nil {
		return "", "could not be reached"
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", "answered HTTP " + strconv.Itoa(resp.StatusCode)
	}
	if format == "gguf" {
		var payload struct {
			BuildInfo string `json:"build_info"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil || payload.BuildInfo == "" {
			return "", "a 200 that did not name a build_info"
		}
		return "llama.cpp " + payload.BuildInfo, ""
	}
	var payload struct {
		Version string `json:"version"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil || payload.Version == "" {
		return "", "a 200 that did not name a version"
	}
	return "vllm " + payload.Version, ""
}

// modelReady asks baseURL's model list, with apiKey, whether hfRepo is being served, and answers
// what that attempt saw, in a fixed vocabulary (timed out, connection refused, an HTTP status,
// or a 200 without the model). A request that fails is classified by its error's kind, never
// printed as text: an http.Client error embeds the request's URL, which must never reach the log.
func (s *Server) modelReady(baseURL, apiKey, hfRepo string) (ready bool, attempt string) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+"/models", nil)
	if err != nil {
		return false, "could not be built"
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)
	resp, err := s.proxyClient.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return false, "timed out"
		}
		if errors.Is(err, syscall.ECONNREFUSED) {
			return false, "connection refused"
		}
		return false, "could not connect"
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return false, "HTTP " + strconv.Itoa(resp.StatusCode)
	}
	var payload struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return false, "a 200 that could not be read as a model list"
	}
	for _, m := range payload.Data {
		if m.ID == hfRepo {
			return true, "200 naming the model"
		}
	}
	return false, "a 200 without the model"
}
