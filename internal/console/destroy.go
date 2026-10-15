package console

import (
	"net/http"
	"strconv"
	"time"

	"github.com/DavidTimar1/gpugate/internal/destroyer"
	"github.com/DavidTimar1/gpugate/internal/provider"
)

// resumeDestroys restarts runDestroy for every destroy_intent row still open (gone_at IS NULL)
// when this process starts. Nothing else resumes a destroy: without this, a process that died
// mid-retry would leave the row open forever, the machine billing, and nothing saying so. askedAt
// is the row's persisted time, not this restart, so the retry ceiling (destroyer.GiveUpAfter) is
// counted from when the destroy was first asked for.
func (s *Server) resumeDestroys() {
	intents, err := s.store.OpenDestroyIntents()
	if err != nil {
		s.log.Add("console", "could not read the open destroy intents: "+err.Error())
		return
	}
	for _, in := range intents {
		var target provider.Provider
		for _, p := range s.providers {
			if p.Key() == in.ProviderKey {
				target = p
				break
			}
		}
		if target == nil {
			s.log.Add("console", "an open destroy intent for "+in.DisplayName+" ("+in.ProviderID+") names an unknown provider '"+in.ProviderKey+"': not resumed.")
			continue
		}
		askedAt, ok := parseStoredTime(in.AskedAt)
		if !ok {
			askedAt = time.Now()
		}
		s.log.Add("instances", "resuming the destroy of "+in.DisplayName+" ("+in.ProviderID+") on "+target.DisplayName()+" after a restart.")
		s.setDestroyNotice(in.ProviderKey, in.ProviderID, "still billing, resuming its destroy after a restart")
		go s.runDestroy(target, in.ProviderID, in.DisplayName, false, askedAt)
	}
}

// destroyKey is the key destroyNotices and the in-flight goroutines are addressed by.
func destroyKey(providerKey, providerID string) string {
	return providerKey + "/" + providerID
}

func (s *Server) setDestroyNotice(providerKey, providerID, text string) {
	s.destroyMu.Lock()
	s.destroyNotices[destroyKey(providerKey, providerID)] = text
	s.destroyMu.Unlock()
}

// dropFromRunState removes one machine from the cached live answer the strip and the proxy read,
// once a fresh ListMachines pass inside runDestroy has confirmed it gone. It leaves ok and
// askedAt as they were: the cached answer is not made fresher than the poll that filled it. The
// slice is rebuilt rather than edited in place, because handleRunning copies the struct and
// shares the slice with the copy.
func (s *Server) dropFromRunState(providerKey, providerID string) {
	s.runMu.Lock()
	defer s.runMu.Unlock()
	st, have := s.runState[providerKey]
	if !have {
		return
	}
	kept := make([]provider.RunningMachine, 0, len(st.machines))
	for _, m := range st.machines {
		if m.ProviderID != providerID {
			kept = append(kept, m)
		}
	}
	keptLast := make([]provider.RunningMachine, 0, len(st.lastMachines))
	for _, m := range st.lastMachines {
		if m.ProviderID != providerID {
			keptLast = append(keptLast, m)
		}
	}
	next := *st
	next.machines = kept
	next.lastMachines = keptLast
	s.runState[providerKey] = &next
}

func (s *Server) clearDestroyNotice(providerKey, providerID string) {
	s.destroyMu.Lock()
	delete(s.destroyNotices, destroyKey(providerKey, providerID))
	s.destroyMu.Unlock()
}

func (s *Server) destroyNotice(providerKey, providerID string) (string, bool) {
	s.destroyMu.Lock()
	defer s.destroyMu.Unlock()
	text, ok := s.destroyNotices[destroyKey(providerKey, providerID)]
	return text, ok
}

// handleDestroy answers POST /api/machines/{provider}/{id}/destroy: the call the confirmation
// modal's "Destroy machine" press sends. It destroys ONE machine and nothing else. It never
// rents, starts, reconfigures or stops anything; the only thing it may do is tear the named
// machine down for good.
func (s *Server) handleDestroy(w http.ResponseWriter, r *http.Request) {
	providerKey := r.PathValue("provider")
	providerID := r.PathValue("id")
	if providerKey == "" || providerID == "" {
		writeRefusal(w, http.StatusBadRequest, "A provider and a machine id are both required.")
		return
	}
	var target provider.Provider
	for _, p := range s.providers {
		if p.Key() == providerKey {
			target = p
			break
		}
	}
	if target == nil {
		writeRefusal(w, http.StatusBadRequest, "'"+providerKey+"' is not a provider this console knows.")
		return
	}
	if !target.CredentialsConfigured() {
		writeRefusal(w, http.StatusBadRequest, target.DisplayName()+" has no credential configured, so it cannot be asked to destroy anything.")
		return
	}

	// Best-effort display name and price, read from the live poll's cache, for the log line and
	// the destroy_intent row alone; the destroy call itself only needs providerID.
	displayName := providerKey + " machine " + providerID
	var price *float64
	s.runMu.Lock()
	if st, ok := s.runState[providerKey]; ok {
		for _, m := range st.machines {
			if m.ProviderID == providerID {
				displayName = m.DisplayName
				price = m.PricePerHour
				break
			}
		}
	}
	s.runMu.Unlock()

	// Written down and committed before the provider is asked anything. If this cannot be
	// written, nothing is asked.
	if err := s.store.RecordDestroyIntent(providerKey, providerID, displayName, price); err != nil {
		s.log.Add("console", "could not record the destroy intent for "+displayName+": "+err.Error())
		writeRefusal(w, http.StatusInternalServerError, "Could not record the destroy. Nothing was asked of "+target.DisplayName()+".")
		return
	}
	s.log.Add("instances", "asked "+target.DisplayName()+" to destroy "+displayName+" ("+providerID+")")
	s.setDestroyNotice(providerKey, providerID, "destroy asked for, waiting on "+target.DisplayName())

	// A machine that has not reached serving has its log read first, whenever its log can be read
	// at all (RunPod's log endpoint, or a Verda machine reached through its SSH tunnel). "Not
	// reached serving" is read from the claim record, never guessed at: no claim on file for it,
	// or one whose serving_state is not yet 'serving', both count. A machine already serving is
	// destroyed with no log read.
	readLogFirst := false
	if s.canReadLog(machineRef{providerKey, providerID}) {
		state, found, err := s.store.ServingStateForProviderID(providerKey, providerID)
		if err != nil {
			s.log.Add("console", "could not read the serving state for "+providerID+" before destroying it: "+err.Error())
		}
		readLogFirst = err != nil || !found || state != "serving"
	}

	go s.runDestroy(target, providerID, displayName, readLogFirst, time.Now())

	writeJSON(w, http.StatusAccepted, map[string]any{
		"provider_key": providerKey,
		"provider_id":  providerID,
		"detail":       "Asked " + target.DisplayName() + " to destroy " + displayName + ". Confirming with a fresh answer next.",
	})
}

// runDestroy is the goroutine handleDestroy starts: the log of a machine that never served is
// read first when readLogFirst is set and the log can be read at all (a slow or failed read does
// not hold the destroy up), then destroyer.Run keeps asking until the provider accepts and a
// fresh answer no longer carries the machine. What the console adds on the way is the strip
// notice, and on confirmed gone the closed tunnel and the dropped cache entry.
//
// askedAt is the destroy_intent row's asked_at, so resumeDestroys does not give a destroy
// already partway through its ceiling a fresh one.
func (s *Server) runDestroy(target provider.Provider, providerID, displayName string, readLogFirst bool, askedAt time.Time) {
	if readLogFirst && s.canReadLog(machineRef{target.Key(), providerID}) {
		s.readAndLogPodLogBeforeDestroy(target.Key(), providerID)
	}
	destroyer.Run(target, s.store, providerID, displayName, askedAt, destroyer.Hooks{
		Log:    s.log.Add,
		Notice: func(text string) { s.setDestroyNotice(target.Key(), providerID, text) },
		Gone: func() {
			// A confirmed-gone destroy closes this machine's own SSH tunnel; a no-op on RunPod.
			s.closeTunnel(machineRef{target.Key(), providerID})
			s.dropFromRunState(target.Key(), providerID)
			s.clearDestroyNotice(target.Key(), providerID)
		},
	})
}

// podLogBeforeDestroyLines is how many lines of a machine's log the read before a destroy asks
// for: enough to place a failure that the tail alone would not show. RunPod returns at most 5000
// ([DOC] openapi.json, GET /v2/pods/{id}/logs); 1000 is what the console log can take without
// pushing out every other line, so a machine with a longer log is written from its last 1000
// lines on.
const podLogBeforeDestroyLines = 1000

// readAndLogPodLogBeforeDestroy reads providerID's whole available log (up to
// podLogBeforeDestroyLines), redacted and capped the way logPodLogTail's checkpoints are, and
// writes it into the program's log before the destroy that called it asks the provider to tear
// the machine down. A failed read is said plainly and never delays the destroy that follows it.
func (s *Server) readAndLogPodLogBeforeDestroy(providerKey, providerID string) {
	lines, err := s.readRedactedPodLog(providerKey, providerID, podLogBeforeDestroyLines)
	if err != nil {
		s.log.Add("instances", providerLabel(providerKey)+" "+providerID+": could not read its own log before destroying it: "+err.Error())
		return
	}
	s.log.Add("instances", providerLabel(providerKey)+" "+providerID+"'s own log, "+strconv.Itoa(len(lines))+" line(s) (the most it returns, up to "+strconv.Itoa(podLogBeforeDestroyLines)+"), before it is destroyed:")
	for _, l := range lines {
		s.log.Add("instances", providerLabel(providerKey)+" "+providerID+": "+l)
	}
}

// destroyUnreachablePod starts destroying providerID at once, from runLaunch, by the path
// handleDestroy uses (RecordDestroyIntent, then runDestroy). A machine nobody could ever reach
// is destroyed without asking the user first, because it only bills.
func (s *Server) destroyUnreachablePod(target provider.Provider, providerID, displayName, reason string) {
	if werr := s.store.RecordDestroyIntent(target.Key(), providerID, displayName, nil); werr != nil {
		s.log.Add("console", "could not record the destroy intent for "+displayName+": "+werr.Error())
	}
	s.log.Add("instances", "destroying "+displayName+" ("+providerID+") on "+target.DisplayName()+": "+reason+".")
	s.setDestroyNotice(target.Key(), providerID, "destroying: "+reason)
	// A machine destroyed here has never reached serving (it is unreachable, missing its key
	// file the instant it was created), so its log is read first.
	go s.runDestroy(target, providerID, displayName, true, time.Now())
}
