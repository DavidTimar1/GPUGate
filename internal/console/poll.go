package console

import (
	"context"
	"strconv"
	"strings"
	"time"

	"github.com/DavidTimar1/gpugate/internal/provider"
	"github.com/DavidTimar1/gpugate/internal/store"
)

// livePollInterval is how often this program asks both providers what is actually running. The
// refresh is meant to be 30 seconds or less; 20 seconds meets it. The page polls this cache every
// 5 seconds (the hx-trigger in ui/index.html), which costs neither provider anything because it
// reads what this ticker last found.
const livePollInterval = 20 * time.Second

// pollRunningForever asks both providers what is running, on livePollInterval, for the life of
// the process. There is no shutdown signal wired to it: the process exiting stops it, which is
// enough for a poll that spends nothing.
func (s *Server) pollRunningForever() {
	ticker := time.NewTicker(livePollInterval)
	defer ticker.Stop()
	for range ticker.C {
		s.pollRunningOnce(context.Background())
	}
}

// pollRunningOnce asks every provider, once each, what it is running right now, and records the
// outcome, success or the reason it could not be asked, in s.runState. A successful answer is
// also mirrored into the store, so "could not find out" later has our last notes to show, marked
// unconfirmed, rather than nothing.
func (s *Server) pollRunningOnce(ctx context.Context) {
	// A panic in the bookkeeping before the providers are asked ends this pass only; the next
	// tick tries again. Every provider's own call is guarded separately below.
	defer func() {
		if r := recover(); r != nil {
			s.reportPanic("the live poll pass", r)
		}
	}()
	s.flushLastOutputAt()
	s.flushTokenDeltas()
	openClaims, err := s.store.OpenClaims()
	if err != nil {
		s.log.Add("console", "could not read the open claims: "+err.Error())
	}
	claimsByProvider := map[string][]store.Claim{}
	for _, c := range openClaims {
		claimsByProvider[c.ProviderKey] = append(claimsByProvider[c.ProviderKey], c)
	}

	for _, p := range s.providers {
		state := s.pollProviderOnce(ctx, p, claimsByProvider[p.Key()])
		s.runMu.Lock()
		if state.ok {
			state.lastMachines = state.machines
		} else if prev, have := s.runState[p.Key()]; have {
			state.lastMachines = prev.lastMachines
		}
		s.runState[p.Key()] = state
		s.runMu.Unlock()
		if state.newlyConfirmed {
			s.lookForCheaperNow()
		}
	}
}

// pollProviderOnce asks one provider what it is running and works through the answer. A panic
// anywhere inside it is recovered and answered as "could not be asked" for this pass, never as
// an empty list, so the strip and the watcher never read it as "nothing is running".
func (s *Server) pollProviderOnce(ctx context.Context, p provider.Provider, claims []store.Claim) (state *providerRunState) {
	state = &providerRunState{key: p.Key(), displayName: p.DisplayName(), askedAt: time.Now()}
	defer func() {
		if r := recover(); r != nil {
			s.reportPanic("the live poll of "+p.DisplayName(), r)
			state = &providerRunState{key: p.Key(), displayName: p.DisplayName(), askedAt: state.askedAt,
				errText: "its answer made GPUGate's own code fail (see the log); it was not read this pass."}
		}
	}()
	if !p.CredentialsConfigured() {
		state.errText = "no credential configured, so it was not asked."
		return state
	}
	callCtx, cancel := context.WithTimeout(ctx, providerCallTimeout)
	machines, err := p.ListMachines(callCtx)
	cancel()
	if err != nil {
		state.errText = err.Error()
		s.log.Add("providers", p.DisplayName()+" could not be asked what is running: "+err.Error())
		return state
	}
	state.ok = true
	state.machines = machines
	if err := s.store.ReplaceProviderMachines(p.Key(), machines); err != nil {
		s.log.Add("console", "could not record "+p.DisplayName()+"'s running machines: "+err.Error())
	}
	state.newlyConfirmed = s.reconcileClaims(p.Key(), claims, machines)
	s.sweepInactivity(p, machines)
	s.ensureTunnels(p.Key(), machines)
	return state
}

// claimAbandonAfter is how long an open claim (the record of a launch) is left waiting for a
// machine to show up on a fresh provider answer before the watcher gives up on it. Every launch
// clock is one hour, and the claim is settled then.
const claimAbandonAfter = time.Hour

// claimAbandonMissesNeeded is how many consecutive periodic passes past claimAbandonAfter must
// each find no machine before the watcher gives up on a claim. One miss past the ceiling is
// never enough: the live-poll race pollServingReadiness guards against, a request that began
// before the machine appeared landing after it, can strike here too.
const claimAbandonMissesNeeded = 2

// reconcileClaims is the watcher that finishes what a dead process left. It runs inside the live
// poll on every pass rather than as a separate program: it is what this pass does with what it
// just heard. A launch's job is only to rent one machine, so there is nothing left to finish
// once fresh carries the machine. A claim is matched by its provider id, once the provider has
// answered the create call, or by its hostname when the process that asked for it died before it
// got that id back (the machine still gets found and the record still comes up to date). It
// answers whether it confirmed any claim's machine on this pass.
func (s *Server) reconcileClaims(providerKey string, claims []store.Claim, fresh []provider.RunningMachine) (confirmedAny bool) {
	if len(claims) == 0 {
		return false
	}
	byID := map[string]bool{}
	byHostname := map[string]bool{}
	for _, m := range fresh {
		if m.ProviderID != "" {
			byID[m.ProviderID] = true
		}
		if m.Hostname != "" {
			byHostname[m.Hostname] = true
		}
	}
	for _, c := range claims {
		matched := (c.ProviderID != nil && *c.ProviderID != "" && byID[*c.ProviderID]) ||
			(c.Hostname != "" && byHostname[c.Hostname])
		if matched {
			if err := s.store.MarkClaimConfirmed(c.ID); err != nil {
				s.log.Add("console", "could not confirm claim "+strconv.FormatInt(c.ID, 10)+": "+err.Error())
			} else {
				confirmedAny = true
				confirmedAt := time.Now()
				s.log.Add("launch", s.rentalPrefix(c.ID)+providerLabel(providerKey)+" now lists "+c.ShapeCode+" ("+c.Hostname+"); it is starting up and is not serving yet.")
				// A claim that asked to serve a model (ServeArgs non-empty) starts its readiness
				// poll the moment a fresh answer confirms the machine, on either provider: the
				// strip's second state, "running, loading the model". confirmedAt is this exact
				// moment, so the readiness poll can tell a stale answer from a fresh one. baseURL
				// is the matched machine's servingBaseURL: always known on RunPod, "" on Verda
				// until its tunnel connects, which pollServingReadiness waits for itself.
				if c.ServeArgs != "" && c.ProviderID != nil {
					if err := s.store.MarkClaimLoading(c.ID); err != nil {
						s.log.Add("console", "could not mark claim "+strconv.FormatInt(c.ID, 10)+" loading: "+err.Error())
					}
					baseURL := ""
					for _, m := range fresh {
						if m.ProviderID == *c.ProviderID {
							baseURL, _ = s.servingBaseURL(m)
							break
						}
					}
					s.startReadinessPoll(c.ID, providerKey, *c.ProviderID, baseURL, c.ModelName, s.byName[c.ModelName].HFRepo, confirmedAt)
				}
			}
			s.clearAbandonMiss(c.ID)
			continue
		}
		asked, err := time.Parse(store.TimeLayout, c.AskedAt)
		if err == nil && time.Since(asked) > claimAbandonAfter {
			misses := s.recordAbandonMiss(c.ID)
			if misses < claimAbandonMissesNeeded {
				s.log.Add("launch", s.rentalPrefix(c.ID)+providerLabel(providerKey)+" still shows no machine for "+c.ShapeCode+" ("+c.Hostname+") past the "+claimAbandonAfter.String()+" ceiling ("+strconv.Itoa(misses)+" of "+strconv.Itoa(claimAbandonMissesNeeded)+" fresh misses needed); waiting for one more before giving up.")
				continue
			}
			detail := "no machine ever appeared on " + providerLabel(providerKey) + " for this claim, after " + claimAbandonAfter.String() + "."
			if err := s.store.MarkClaimAbandoned(c.ID, detail); err != nil {
				s.log.Add("console", "could not abandon claim "+strconv.FormatInt(c.ID, 10)+": "+err.Error())
			} else {
				s.log.Add("launch", s.rentalPrefix(c.ID)+providerLabel(providerKey)+" failed to produce a machine for "+c.ShapeCode+" ("+c.Hostname+") within "+claimAbandonAfter.String()+"; giving up.")
			}
			// An abandoned claim never got as far as 'loading', so nothing else will delete its
			// Verda startup script (a no-op on RunPod, which never carries one).
			s.deleteStartupScriptForClaim(providerKey, c.ID)
			s.clearAbandonMiss(c.ID)
		}
	}
	return confirmedAny
}

// recordAbandonMiss counts one more periodic pass, past an open claim's abandon ceiling, that
// found no machine for it, and returns the new count. A pass that still finds nothing never
// resets it, only a match (clearAbandonMiss) or the claim finally being abandoned.
func (s *Server) recordAbandonMiss(claimID int64) int {
	s.abandonMu.Lock()
	defer s.abandonMu.Unlock()
	s.abandonMisses[claimID]++
	return s.abandonMisses[claimID]
}

// clearAbandonMiss drops claimID's miss count: it matched, or it was just abandoned and the
// count is no longer needed.
func (s *Server) clearAbandonMiss(claimID int64) {
	s.abandonMu.Lock()
	delete(s.abandonMisses, claimID)
	s.abandonMu.Unlock()
}

// recordOutput moves ref's in-memory "last output" clock to now. Called by the fixed address on
// the response headers arriving and on every non-empty chunk read from the upstream body (that
// is what "output" means for the inactivity timeout). It never touches the database: the flush
// to the store happens once per live-poll pass (flushLastOutputAt), never on this hot streaming
// path.
func (s *Server) recordOutput(ref machineRef) {
	if ref.providerID == "" {
		return
	}
	s.lastOutputMu.Lock()
	s.lastOutputAt[ref] = time.Now()
	s.lastOutputMu.Unlock()
}

// flushLastOutputAt writes every machine whose in-memory "last output" clock has moved since the
// last flush to machine_claim.last_output_at, once per live-poll pass. A machine whose clock has
// not moved since the last flush is left alone.
func (s *Server) flushLastOutputAt() {
	s.lastOutputMu.Lock()
	changed := map[machineRef]time.Time{}
	for ref, at := range s.lastOutputAt {
		if prev, have := s.flushedOutputAt[ref]; !have || !prev.Equal(at) {
			changed[ref] = at
			s.flushedOutputAt[ref] = at
		}
	}
	s.lastOutputMu.Unlock()
	for ref, at := range changed {
		if err := s.store.SetLastOutputAt(ref.providerKey, ref.providerID, at.UTC().Format(store.TimeLayout)); err != nil {
			s.log.Add("console", "could not record the last output time for "+ref.providerID+": "+err.Error())
		}
	}
}

// recordUsage adds one proxied call's token counts to ref's in-memory delta since the last
// live-poll flush. It follows the pattern recordOutput uses for the inactivity clock, except
// this accumulates a delta to add rather than overwriting an absolute clock. It never touches
// the database: flushTokenDeltas does that, once per live-poll pass.
func (s *Server) recordUsage(ref machineRef, in, out, cached int) {
	if ref.providerID == "" || (in == 0 && out == 0 && cached == 0) {
		return
	}
	s.tokenMu.Lock()
	d := s.tokenDeltas[ref]
	d.in += in
	d.out += out
	d.cached += cached
	s.tokenDeltas[ref] = d
	s.tokenMu.Unlock()
}

// flushTokenDeltas adds every machine's accumulated token delta to machine_claim.tokens_in,
// tokens_out and tokens_cached, once per live-poll pass, and clears what it flushed. The delta is
// ADDED to the store's running total (store.AddTokenCounts), never replaces it, unlike
// flushLastOutputAt's absolute clock.
func (s *Server) flushTokenDeltas() {
	s.tokenMu.Lock()
	deltas := s.tokenDeltas
	s.tokenDeltas = map[machineRef]tokenDelta{}
	s.tokenMu.Unlock()
	for ref, d := range deltas {
		if d.in == 0 && d.out == 0 && d.cached == 0 {
			continue
		}
		if err := s.store.AddTokenCounts(ref.providerKey, ref.providerID, d.in, d.out, d.cached); err != nil {
			s.log.Add("console", "could not record the token counts for "+ref.providerID+": "+err.Error())
		}
	}
}

// parseStoredTime parses one of the store's timestamps (store.TimeLayout), answering false for an
// empty or unparseable value rather than a zero time that could be mistaken for a real one.
func parseStoredTime(iso string) (time.Time, bool) {
	if iso == "" {
		return time.Time{}, false
	}
	t, err := time.Parse(store.TimeLayout, iso)
	if err != nil {
		return time.Time{}, false
	}
	return t, true
}

// quietSince answers one serving claim's quiet-since moment: last_output_at when the store holds
// one, whatever it says, because a persisted output time is real evidence a restart must not
// override; serving_since when it does not; this process's start time only when neither is on
// file yet (which should not happen for a claim already serving_state = 'serving', since
// serving_since is written the moment MarkClaimServing fires). A restart does not floor the
// clock at the process start, because that would push a known-quiet machine's destroy time out
// by a full window on every restart.
func (s *Server) quietSince(info store.ServingInfo) time.Time {
	if t, ok := parseStoredTime(info.LastOutputAt); ok {
		return t
	}
	if t, ok := parseStoredTime(info.Since); ok {
		return t
	}
	return s.startedAt
}

// sweepInactivity is the inactivity sweep, run from pollRunningOnce right after reconcileClaims,
// on the fresh answer this pass just got from target. Every claim serving_state = 'serving'
// whose provider_id fresh lists is judged; a claim fresh does not list is reconcileClaims's
// business, never this sweep's. A machine whose quiet-since has reached its claim's idle_minutes
// (0 means never) is destroyed through the same intent-first path every other destroy uses, and
// never twice for the same machine: the in-memory destroy notice and an open destroy_intent row
// both guard it, so a second pass mid-retry, or one after a restart, does nothing new.
func (s *Server) sweepInactivity(target provider.Provider, fresh []provider.RunningMachine) {
	servingInfo, err := s.store.ServingInfoByProviderID(target.Key())
	if err != nil {
		s.log.Add("console", "could not read "+target.DisplayName()+"'s serving info for the inactivity sweep: "+err.Error())
		return
	}
	byID := map[string]provider.RunningMachine{}
	for _, m := range fresh {
		byID[m.ProviderID] = m
	}
	now := time.Now()
	for providerID, info := range servingInfo {
		if info.ServingState != "serving" || info.IdleMinutes <= 0 {
			continue
		}
		ceiling := time.Duration(info.IdleMinutes) * time.Minute
		machine, present := byID[providerID]
		if !present {
			continue
		}
		since := s.quietSince(info)
		if now.Sub(since) < ceiling {
			continue
		}
		if _, destroying := s.destroyNotice(target.Key(), providerID); destroying {
			continue
		}
		hasIntent, err := s.store.HasOpenDestroyIntent(target.Key(), providerID)
		if err != nil {
			s.log.Add("console", "could not check for an open destroy intent on "+providerID+": "+err.Error())
			continue
		}
		if hasIntent {
			continue
		}
		s.destroyForInactivity(target, machine, int(now.Sub(since).Minutes()))
	}
}

// destroyForInactivity starts destroying a machine nobody has talked to in quietMinutes, through
// the same intent-first path destroyUnreachablePod uses (RecordDestroyIntent, then runDestroy),
// with a log line and a destroy notice.
func (s *Server) destroyForInactivity(target provider.Provider, machine provider.RunningMachine, quietMinutes int) {
	displayName := machine.DisplayName
	if err := s.store.RecordDestroyIntent(target.Key(), machine.ProviderID, displayName, machine.PricePerHour); err != nil {
		s.log.Add("console", "could not record the destroy intent for "+displayName+": "+err.Error())
	}
	minutesText := strconv.Itoa(quietMinutes)
	s.log.Add("instances", "destroying "+displayName+" ("+machine.ProviderID+"): no output for "+minutesText+" minutes")
	s.setDestroyNotice(target.Key(), machine.ProviderID, "destroying: no output for "+minutesText+" minutes")
	// A machine destroyed here is already serving (the inactivity sweep only fires on
	// serving_state = 'serving'), so its log is not read first.
	go s.runDestroy(target, machine.ProviderID, displayName, false, time.Now())
}

// machineByID answers the live poll's fresh row for providerID on providerKey, and whether it
// has one; ServingBaseURL is "" when the machine has none yet. Used wherever one specific
// machine's current fields, not just its presence, are needed (resumeServingPolls and the
// mover).
func (s *Server) machineByID(providerKey, providerID string) (provider.RunningMachine, bool) {
	s.runMu.Lock()
	st, have := s.runState[providerKey]
	s.runMu.Unlock()
	if !have || !st.ok {
		return provider.RunningMachine{}, false
	}
	for _, m := range st.machines {
		if m.ProviderID == providerID {
			return m, true
		}
	}
	return provider.RunningMachine{}, false
}

// liveMachines answers, for every provider whose live poll last succeeded, the set of provider
// ids it holds right now. It is the fresh-live-poll gate every serving-machine read applies.
func (s *Server) liveMachines() map[string]map[string]bool {
	s.runMu.Lock()
	defer s.runMu.Unlock()
	out := map[string]map[string]bool{}
	for key, st := range s.runState {
		if !st.ok {
			continue
		}
		set := map[string]bool{}
		for _, m := range st.machines {
			set[m.ProviderID] = true
		}
		out[key] = set
	}
	return out
}

// lastKnownMachines answers, for every provider, the provider ids of its newest successful list,
// including a provider whose latest pass failed. Only the fixed address and the project list read
// it, so a provider blip does not cut off a coding tool; a successful list that omits a machine
// removes it here at once. It never decides a destroy, a sweep, a confirmation or a move: those
// read liveMachines and machineByID, which answer fresh lists only.
func (s *Server) lastKnownMachines() map[string]map[string]bool {
	s.runMu.Lock()
	defer s.runMu.Unlock()
	out := map[string]map[string]bool{}
	for key, st := range s.runState {
		set := map[string]bool{}
		for _, m := range st.lastMachines {
			set[m.ProviderID] = true
		}
		out[key] = set
	}
	return out
}

// lastKnownMachineByID is machineByID over lastKnownMachines: the row from the newest successful
// list, even when the latest pass failed. Used by the fixed address alone.
func (s *Server) lastKnownMachineByID(providerKey, providerID string) (provider.RunningMachine, bool) {
	s.runMu.Lock()
	st, have := s.runState[providerKey]
	s.runMu.Unlock()
	if !have {
		return provider.RunningMachine{}, false
	}
	for _, m := range st.lastMachines {
		if m.ProviderID == providerID {
			return m, true
		}
	}
	return provider.RunningMachine{}, false
}

// allServingInfo merges every provider's ServingInfoByProviderID answer into one map, keyed by
// provider_id, for the running strip. Provider ids do not collide across providers (RunPod's are
// short alphanumerics, Verda's are UUIDs). A read error on one provider is logged and leaves that
// provider's machines without serving detail; the strip still draws.
func (s *Server) allServingInfo() map[string]store.ServingInfo {
	out := map[string]store.ServingInfo{}
	for _, p := range s.providers {
		info, err := s.store.ServingInfoByProviderID(p.Key())
		if err != nil {
			s.log.Add("console", "could not read "+p.DisplayName()+"'s serving info: "+err.Error())
			continue
		}
		for k, v := range info {
			out[k] = v
		}
	}
	return out
}

// exitedStateWords are the provider state words that mean a machine has exited: RunPod's and
// Verda's terminal status words share one set, matched case-insensitively, since neither
// provider emits the other's spelling.
var exitedStateWords = map[string]bool{
	"EXITED": true, "TERMINATED": true, "ERROR": true, "FAILED": true,
	"INSTALLATION_FAILED": true, "NO_CAPACITY": true, "DISCONTINUED": true,
	"NOTFOUND": true, "DELETING": true, "OFFLINE": true,
}

// podState reports whether providerID appears in machines, and whether it has exited, per the
// provider's state word and exitedStateWords.
func podState(machines []provider.RunningMachine, providerID string) (present, exited bool) {
	for _, m := range machines {
		if m.ProviderID == providerID {
			present = true
			if exitedStateWords[strings.ToUpper(strings.TrimSpace(m.State))] {
				exited = true
			}
			return
		}
	}
	return
}

// freshAnswer answers the live poll's last answer for providerKey, and whether it is FRESH: asked
// for after confirmedAt. A stale answer, one whose request began before confirmedAt, can still
// land after it and correctly carry no particular machine, so only a fresh one is handed back;
// the caller reads nothing at all from a stale one.
func (s *Server) freshAnswer(providerKey string, confirmedAt time.Time) (providerRunState, bool) {
	s.runMu.Lock()
	defer s.runMu.Unlock()
	st, have := s.runState[providerKey]
	if !have || !st.ok || !st.askedAt.After(confirmedAt) {
		return providerRunState{}, false
	}
	return *st, true
}
