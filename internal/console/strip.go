package console

import (
	"fmt"
	"html"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/DavidTimar1/gpugate/internal/provider"
	"github.com/DavidTimar1/gpugate/internal/store"
)

// handleIdleMinutes answers POST /api/machines/{provider}/{id}/idle-minutes?minutes=N: the
// strip's control for one loading or serving machine's idle timeout, 0 meaning never. It changes
// only what the inactivity sweep reads on its next pass (a loading machine is judged once it
// serves) and asks no provider anything.
func (s *Server) handleIdleMinutes(w http.ResponseWriter, r *http.Request) {
	providerKey, providerID := r.PathValue("provider"), r.PathValue("id")
	minutes, err := strconv.Atoi(r.URL.Query().Get("minutes"))
	if err != nil || minutes < 0 {
		writeRefusal(w, http.StatusBadRequest, "minutes must be a whole number, zero or more.")
		return
	}
	found, err := s.store.SetClaimIdleMinutes(providerKey, providerID, minutes)
	if err != nil {
		s.log.Add("console", "could not write the idle timeout: "+err.Error())
		writeRefusal(w, http.StatusInternalServerError, "Could not set the idle timeout.")
		return
	}
	if !found {
		writeRefusal(w, http.StatusNotFound, "No loading or serving machine of that provider and id is on file.")
		return
	}
	s.log.Add("console", "idle timeout of "+providerKey+" "+providerID+" set to "+strconv.Itoa(minutes)+" minutes (0 means never).")
	writeJSON(w, http.StatusOK, map[string]any{"minutes": minutes})
}

// handleRunning answers GET /api/running: the HTML fragment htmx swaps into `#rlist`, followed by
// an out-of-band `#runstamp` chip saying when both providers last answered (hidden while any
// provider could not be asked). It asks no provider anything itself: it reads what the live poll
// last found, so the page's refresh (every 5 seconds) costs neither provider a call. Only a
// fresh answer counts in this handler: our notes say what we DID, only a fresh provider answer
// says what IS running, and "could not find out" is never drawn as "nothing running".
func (s *Server) handleRunning(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeRefusal(w, http.StatusMethodNotAllowed, "GET is the only method this call answers.")
		return
	}
	s.runMu.Lock()
	states := make(map[string]providerRunState, len(s.runState))
	for k, v := range s.runState {
		states[k] = *v
	}
	s.runMu.Unlock()

	// Which machines, on either provider, are loading or serving a model right now, keyed by
	// provider_id, so runningMachineLI below can draw the strip's second and third states
	// instead of the plain "running" chip.
	servingInfo := s.allServingInfo()

	var ok, failed []providerRunState
	for _, p := range s.providers {
		st, have := states[p.Key()]
		if !have {
			// The first poll has not landed yet (should not happen: New runs one before the
			// listener accepts), treated the same as "could not be asked".
			st = providerRunState{key: p.Key(), displayName: p.DisplayName(), errText: "not asked yet."}
		}
		if st.ok {
			ok = append(ok, st)
		} else {
			failed = append(failed, st)
		}
	}

	// A claim still open (Launch has been pressed, no fresh answer has confirmed a machine for it
	// yet) is shown honestly as starting up, on every branch below. It is our own note, never a
	// provider's proof, and it is never left off the strip just because a provider could not be
	// reached this pass. It is computed here, before the empty-state lines below, so an
	// empty-state line is never drawn while a claim's "starting up" line is also about to be
	// drawn.
	shown := map[string]bool{}
	for _, st := range ok {
		for _, m := range st.machines {
			if m.ProviderID != "" {
				shown[st.key+"/"+m.ProviderID] = true
			}
		}
	}
	claims, err := s.store.OpenClaims()
	if err != nil {
		s.log.Add("console", "could not read the open claims: "+err.Error())
	}
	visibleClaims := 0
	for _, c := range claims {
		if c.ProviderID != nil && shown[c.ProviderKey+"/"+*c.ProviderID] {
			continue
		}
		visibleClaims++
	}

	var b strings.Builder
	switch {
	case len(failed) == 0:
		// Every provider answered fresh.
		var machines []provider.RunningMachine
		for _, st := range ok {
			machines = append(machines, st.machines...)
		}
		if len(machines) == 0 && visibleClaims == 0 {
			b.WriteString(runningEmptyLI("Nothing is running, on either provider, as of " + latestAsOf(ok) + "."))
		} else {
			for _, st := range ok {
				for _, m := range st.machines {
					b.WriteString(s.runningMachineLI(m, true, "", servingInfo))
				}
			}
		}
	case len(ok) == 0:
		// Nobody answered. This is "could not find out", never "nothing running", and it is
		// never empty: it names every provider and shows our own last notes, unconfirmed.
		b.WriteString(runningNoticeLI("Could not find out what is running: " + failedReasons(failed)))
		wrote := false
		for _, st := range failed {
			known, lastSeen, err := s.store.LastKnownMachines(st.key)
			if err != nil {
				s.log.Add("console", "could not read "+st.displayName+"'s last known machines: "+err.Error())
				continue
			}
			for _, m := range known {
				b.WriteString(s.runningMachineLI(m, false, lastSeen, servingInfo))
				wrote = true
			}
		}
		if !wrote && visibleClaims == 0 {
			b.WriteString(runningEmptyLI("No machine has ever been seen on either provider."))
		}
	default:
		// Partly known: some providers answered, some did not.
		b.WriteString(runningNoticeLI("Could not ask: " + failedReasons(failed)))
		for _, st := range ok {
			for _, m := range st.machines {
				b.WriteString(s.runningMachineLI(m, true, "", servingInfo))
			}
		}
		for _, st := range failed {
			known, lastSeen, err := s.store.LastKnownMachines(st.key)
			if err != nil {
				s.log.Add("console", "could not read "+st.displayName+"'s last known machines: "+err.Error())
				continue
			}
			for _, m := range known {
				b.WriteString(s.runningMachineLI(m, false, lastSeen, servingInfo))
			}
		}
	}

	// The claims computed above (shown, claims, visibleClaims) are drawn here, on every branch
	// above: our own note, never a provider's proof, and never left off the strip just because a
	// provider could not be reached this pass.
	for _, c := range claims {
		if c.ProviderID != nil && shown[c.ProviderKey+"/"+*c.ProviderID] {
			continue
		}
		b.WriteString(startingUpLI(c))
	}

	if len(failed) == 0 && len(ok) > 0 {
		b.WriteString(`<span id="runstamp" class="chip plain" hx-swap-oob="true">checked ` + html.EscapeString(latestAsOf(ok)) + `</span>`)
	} else {
		b.WriteString(`<span id="runstamp" class="chip plain" hidden hx-swap-oob="true"></span>`)
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(b.String()))
}

// startingUpLI is one open claim's line: a launch that has been asked for but that no fresh
// provider answer has confirmed yet. Our note that a create was asked for is never drawn as the
// machine actually running, so this carries its own chip, never "running" or "unconfirmed", and
// no Delete button: there is nothing yet whose destroy this program could ask for.
func startingUpLI(c store.Claim) string {
	sub := providerLabel(c.ProviderKey)
	if c.Location != "" {
		sub += " \u00b7 " + c.Location
	}
	if c.PricePerHour != nil {
		sub += " \u00b7 " + fmt.Sprintf("$%.2f/hr", *c.PricePerHour)
	} else {
		sub += " \u00b7 price unknown"
	}
	switch c.State {
	case "created":
		sub += " \u00b7 waiting for a fresh " + providerLabel(c.ProviderKey) + " answer to list it"
	default:
		sub += " \u00b7 waiting for " + providerLabel(c.ProviderKey) + " to answer the create request"
	}
	return `<li class="card mach loading" data-run="starting">` +
		`<div class="mtop"><span class="chip wait"><span class="dot"></span>Starting up</span></div>` +
		`<div class="name mono">` + html.EscapeString(c.ShapeCode) + `</div>` +
		`<p class="sub">` + html.EscapeString(sub) + `</p>` +
		`</li>`
}

func failedReasons(failed []providerRunState) string {
	parts := make([]string, 0, len(failed))
	for _, st := range failed {
		parts = append(parts, st.displayName+": "+st.errText)
	}
	return strings.Join(parts, " ")
}

func latestAsOf(states []providerRunState) string {
	var latest time.Time
	for _, st := range states {
		if st.askedAt.After(latest) {
			latest = st.askedAt
		}
	}
	if latest.IsZero() {
		return "unknown"
	}
	return latest.UTC().Format("15:04:05") + " UTC"
}

// clockFromStoredTime renders one of the store's timestamps (store.TimeLayout) as a plain clock
// time, the shape latestAsOf prints. An unparseable value is shown as stored rather than hidden,
// so a formatting mismatch is visible instead of silent.
func clockFromStoredTime(iso string) string {
	t, err := time.Parse(store.TimeLayout, iso)
	if err != nil {
		return iso
	}
	return t.UTC().Format("15:04:05") + " UTC"
}

// quietClockClause is the inactivity clock's words on a serving machine's strip line: "quiet
// since HH:MM:SS UTC, destroy at HH:MM:SS UTC" when the claim's idle timeout is above zero, "no
// inactivity timeout" when it is zero, each led by a middle dot. The caller does not call this
// once destruction has started: the destroy notice takes its place on that row instead.
func (s *Server) quietClockClause(info store.ServingInfo) string {
	minutes := info.IdleMinutes
	if minutes <= 0 {
		return " \u00b7 no inactivity timeout"
	}
	since := s.quietSince(info)
	destroyAt := since.Add(time.Duration(minutes) * time.Minute)
	return " \u00b7 quiet since " + since.UTC().Format("15:04:05") + " UTC, destroy at " + destroyAt.UTC().Format("15:04:05") + " UTC"
}

// runningEmptyLI is one plain notice line: no machine to show, but a stated reason, never a
// silently blank list.
func runningEmptyLI(text string) string {
	return `<li class="card empty">` + html.EscapeString(text) + `</li>`
}

// runningNoticeLI is the line naming which provider could not be asked and why, in its own words.
// It is never mistaken for a machine row: it carries no state chip and no Delete button. The page
// moves its text into the message overlay (ui/src/app.js, afterRunningSwap) and removes the line.
func runningNoticeLI(text string) string {
	return `<li class="notice">` + html.EscapeString(text) + `</li>`
}

// runningMachineLI is one running machine's line. confirmed is false when this row comes from the
// store rather than from a fresh answer this poll, and it carries the "unconfirmed" chip rather
// than "running". Its Delete button carries the provider and id a click needs to open the
// confirmation modal (ui/src/app.js). It is a method because the row's notice line reads
// s.destroyNotices.
//
// servingInfo is the strip's own: when this machine's provider_id is in it AND confirmed is true
// (never on our own say-so), the plain "running" chip is replaced with the strip's second or
// third state, "running, loading the model" or "serving <model>", carrying the time that state
// was entered.
func (s *Server) runningMachineLI(m provider.RunningMachine, confirmed bool, lastSeenAt string, servingInfo map[string]store.ServingInfo) string {
	chipClass, chipText, cardClass := "ok", strings.ToLower(strings.TrimSpace(m.State)), ""
	if chipText == "" {
		chipText = "running"
	}
	runKind := "running"
	detail := m.DisplayName + " (" + m.GPUName + ")"
	sub := providerLabel(m.ProviderKey)
	if m.Location != "" {
		sub += " \u00b7 " + m.Location
	}
	if m.PricePerHour != nil {
		sub += " \u00b7 " + fmt.Sprintf("$%.2f/hr", *m.PricePerHour)
	} else {
		sub += " \u00b7 price unknown"
	}
	// sub2 is the card's second line: the idle clock, the idle limit while loading, a destroy
	// notice.
	sub2 := ""
	info, haveInfo := servingInfo[m.ProviderID]
	if !confirmed {
		chipClass, chipText, cardClass, runKind = "wait", "unconfirmed", "loading", "unconfirmed"
		if lastSeenAt != "" {
			sub += " \u00b7 last confirmed " + lastSeenAt
		}
	} else if haveInfo {
		switch info.ServingState {
		case "loading":
			chipClass, chipText, cardClass, runKind = "wait", "running, loading the model", "loading", "loading"
		case "serving":
			chipClass, chipText, cardClass, runKind = "ok", "serving "+info.ModelName, "serving", "serving"
			// The running strip names the project on every serving machine's line, so a machine
			// serving another project is never mistaken for the one a client could reach.
			sub += " \u00b7 project " + info.Project
		case "gave_up":
			chipClass, chipText = "bad", "running, gave up loading "+info.ModelName
		}
		if info.Since != "" {
			sub += " \u00b7 since " + clockFromStoredTime(info.Since)
		}
	}
	if m.SSHAddr != "" {
		// A Verda machine's tunnel note. A connected tunnel adds nothing here: it is already
		// implied by the state chip above.
		s.tunnelMu.Lock()
		t, haveTunnel := s.tunnels[machineRef{m.ProviderKey, m.ProviderID}]
		s.tunnelMu.Unlock()
		switch {
		case haveTunnel && !t.Connected():
			sub += " - SSH tunnel down, reconnecting (" + t.LastAttempt() + ")"
		case !haveTunnel:
			if haveInfo && info.ServingState == "loading" {
				sub += " - waiting for SSH"
			}
		}
	}
	notice, destroying := s.destroyNotice(m.ProviderKey, m.ProviderID)
	idleControl := ""
	if haveInfo && (info.ServingState == "serving" || info.ServingState == "loading") && !destroying {
		// This machine's idle timeout, changed in place (ui/src/app.js, .idleinp). A loading
		// machine's figure counts once it serves, and the card says so.
		idleControl = `<label class="idle">Destroy after idle (min) <input type="number" min="0" class="idleinp"` +
			` id="idle-` + html.EscapeString(m.ProviderKey+"-"+m.ProviderID) + `" name="idle_minutes_` + html.EscapeString(m.ProviderKey+"_"+m.ProviderID) + `" autocomplete="off"` +
			` value="` + strconv.Itoa(info.IdleMinutes) + `" inputmode="numeric"` +
			` data-provider-key="` + html.EscapeString(m.ProviderKey) + `"` +
			` data-provider-id="` + html.EscapeString(m.ProviderID) + `"></label>`
		if info.ServingState == "serving" {
			// The clock, replaced by the destroy notice below once destruction starts.
			sub2 = strings.TrimPrefix(s.quietClockClause(info), " \u00b7 ")
		} else if info.IdleMinutes > 0 {
			sub2 = "Idle limit " + strconv.Itoa(info.IdleMinutes) + " min, counted once it serves."
		} else {
			sub2 = "No inactivity timeout."
		}
	}
	if destroying {
		sub2 = notice
	}
	subHTML := html.EscapeString(sub)
	if sub2 != "" {
		subHTML += `<br>` + html.EscapeString(sub2)
	}
	if idleControl == "" {
		idleControl = `<span></span>`
	}
	return `<li class="card mach ` + cardClass + `" data-run="` + runKind + `">` +
		`<div class="mtop"><span class="chip ` + chipClass + `"><span class="dot"></span>` + html.EscapeString(capitalFirst(chipText)) + `</span></div>` +
		`<div class="name mono">` + html.EscapeString(detail) + `</div>` +
		`<p class="sub">` + subHTML + `</p>` +
		`<div class="acts">` + idleControl +
		`<button type="button" class="btn danger rdelete"` +
		` data-provider-key="` + html.EscapeString(m.ProviderKey) + `"` +
		` data-provider-id="` + html.EscapeString(m.ProviderID) + `"` +
		` data-machine-name="` + html.EscapeString(detail) + `"` +
		` data-machine-sub="` + html.EscapeString(providerLabel(m.ProviderKey)+withLocationAndPrice(m)) + `"` +
		`>Delete</button>` +
		`</div></li>`
}

// capitalFirst upper-cases the first letter of a chip's text, since the state words are lower
// case and the chip reads as a label.
func capitalFirst(text string) string {
	if text == "" {
		return text
	}
	return strings.ToUpper(text[:1]) + text[1:]
}

// withLocationAndPrice is the plain-language "where and how much" the confirmation modal names
// alongside the machine, built the way runningMachineLI's sub-line is but without the destroy
// notice or the unconfirmed marker: those belong on the strip row, never inside a modal asking
// "are you sure".
func withLocationAndPrice(m provider.RunningMachine) string {
	sub := ""
	if m.Location != "" {
		sub += " \u00b7 " + m.Location
	}
	if m.PricePerHour != nil {
		sub += " \u00b7 " + fmt.Sprintf("$%.2f/hr", *m.PricePerHour)
	} else {
		sub += " \u00b7 price unknown"
	}
	return sub
}
