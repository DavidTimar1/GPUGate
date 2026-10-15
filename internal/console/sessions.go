package console

import (
	"fmt"
	"html"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/DavidTimar1/gpugate/internal/store"
)

// sessionsListLimit is how many rows GET /api/sessions answers and the page shows, newest first.
const sessionsListLimit = 50

// handleSessions answers GET /api/sessions: an htmx fragment listing every rental that ever
// reached a real machine, newest first, since the sessions_since setting if one is on file, with
// an out-of-band swap of the totals tiles. Whether a claim with no destroy_intent row is still
// running, or its end was simply never confirmed, is judged like every other live-machine read:
// only a fresh live-poll answer that still lists the machine counts as still running, never a
// claim's stored state alone, and never a guessed end.
func (s *Server) handleSessions(w http.ResponseWriter, r *http.Request) {
	fragment, err := s.sessionsFragment()
	if err != nil {
		s.log.Add("console", "could not read the sessions: "+err.Error())
		writeRefusal(w, http.StatusInternalServerError, "Could not read the sessions.")
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write([]byte(fragment))
}

// handleSessionsSince answers POST /api/settings/sessions-since: the "Clear list" button. It
// writes sessions_since as this exact moment, UTC to the second (see sessionsSinceKey for why
// the format matters), then answers the same fragment GET /api/sessions would, so the htmx POST
// that pressed it can re-swap the list and its totals in one round trip. No claim row is
// touched: only where the list starts reading from moves.
func (s *Server) handleSessionsSince(w http.ResponseWriter, r *http.Request) {
	now := time.Now().UTC().Format(store.TimeLayout)
	if err := s.store.SetSessionsSince(now); err != nil {
		s.log.Add("console", "could not write the sessions-since setting: "+err.Error())
		writeRefusal(w, http.StatusInternalServerError, "Could not clear the session list.")
		return
	}
	s.log.Add("console", "cleared the Sessions list from "+now+" on.")
	fragment, err := s.sessionsFragment()
	if err != nil {
		s.log.Add("console", "could not read the sessions: "+err.Error())
		writeRefusal(w, http.StatusInternalServerError, "Could not read the sessions.")
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write([]byte(fragment))
}

// sessionsFragment builds what both GET /api/sessions and the Clear list button answer: the rows
// for #session-body's innerHTML swap, drawn twice (a card list for a phone, a table for a wide
// screen; the page's CSS shows one of them), followed by an out-of-band element
// (`hx-swap-oob="true"`) that updates the totals tiles at the top of the Sessions view. htmx
// applies an out-of-band element by its id, independent of the request's hx-target, so one
// response updates both in a single round trip.
func (s *Server) sessionsFragment() (string, error) {
	since, err := s.store.SessionsSince()
	if err != nil {
		return "", err
	}
	sessions, err := s.store.ListSessions(sessionsListLimit, since)
	if err != nil {
		return "", err
	}

	// live is keyed "<provider_key>/<provider_id>": the newest successful list, on whichever
	// provider a session's claim named, counts as still running; a provider that could not be
	// asked this pass keeps its last list.
	live := map[string]bool{}
	for key, machines := range s.lastKnownMachines() {
		for providerID := range machines {
			live[key+"/"+providerID] = true
		}
	}

	var b strings.Builder
	if len(sessions) == 0 {
		b.WriteString(`<p class="card empty">No session has ever reached a real machine.</p>`)
	} else {
		views := make([]sessionView, 0, len(sessions))
		for _, sess := range sessions {
			views = append(views, newSessionView(sess, live))
		}
		b.WriteString(`<ul class="slist">`)
		for _, v := range views {
			b.WriteString(v.cardLI())
		}
		b.WriteString(`</ul><div class="card stable"><table><thead><tr>`)
		for _, h := range []string{"Project", "Model", "Machine", "Where", "Price", "Ran", "Cost", "Tokens in", "Out", "Cached"} {
			b.WriteString(`<th scope="col" class="plainth">` + h + `</th>`)
		}
		b.WriteString(`</tr></thead><tbody>`)
		for _, v := range views {
			b.WriteString(v.rowTR())
		}
		b.WriteString(`</tbody></table></div>`)
	}
	b.WriteString(sessionTotalsHTML(sessions))
	return b.String(), nil
}

// dateFromStoredTime reads the calendar date, UTC, off one of the store's timestamps, the same
// shape clockFromStoredTime reads. It answers "" for an unparseable value, so a caller can fall
// back the way clockFromStoredTime's callers do.
func dateFromStoredTime(iso string) string {
	t, err := time.Parse(store.TimeLayout, iso)
	if err != nil {
		return ""
	}
	return t.UTC().Format("2006-01-02")
}

// sameUTCDate reports whether a and b fall on the same calendar day, UTC.
func sameUTCDate(a, b time.Time) bool {
	ay, am, ad := a.UTC().Date()
	by, bm, bd := b.UTC().Date()
	return ay == by && am == bm && ad == bd
}

// sessionSpan answers one session's start, end (now if still open) and whether that end is a
// real confirmed end (destroy_intent.gone_at) rather than "now" standing in for it. endText is
// bare ("ended HH:MM:SS UTC") unless the end falls on a later calendar day than the start: the
// row prints the start's date once, so a bare end clock on a different day would read as the
// same day it is not. Then endText carries its own date too, "ended YYYY-MM-DD HH:MM:SS UTC".
func sessionSpan(sess store.Session, live map[string]bool) (start, end time.Time, haveStart, confirmedEnd bool, endText string) {
	start, haveStart = parseStoredTime(sess.AskedAt)
	if sess.GoneAt != nil {
		if goneAt, ok := parseStoredTime(*sess.GoneAt); ok {
			endClock := "ended " + clockFromStoredTime(*sess.GoneAt)
			if haveStart && !sameUTCDate(start, goneAt) {
				endClock = "ended " + dateFromStoredTime(*sess.GoneAt) + " " + clockFromStoredTime(*sess.GoneAt)
			}
			return start, goneAt, haveStart, true, endClock
		}
	}
	if sess.ProviderID != nil && live[sess.ProviderKey+"/"+*sess.ProviderID] {
		return start, time.Now(), haveStart, false, "running"
	}
	// Our own notes never guess an end. No destroy_intent row exists, and a fresh live-poll
	// answer no longer lists this machine (or never has, for a claim still 'created'): the end is
	// not known, and is never shown as still running.
	return start, time.Now(), haveStart, false, "end not confirmed"
}

// formatSessionDuration prints a duration to whole minutes, "Hh Mm" once it reaches an hour.
func formatSessionDuration(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	minutes := int(d.Minutes())
	hours := minutes / 60
	minutes %= 60
	if hours > 0 {
		return fmt.Sprintf("%dh %dm", hours, minutes)
	}
	return fmt.Sprintf("%dm", minutes)
}

// sessionCost is duration times price_per_hour, in dollars to two decimals, with "so far"
// appended while the session's end is not a confirmed one. "price unknown" stands in for a claim
// that was never priced (the provider's answer named no price).
func sessionCost(duration time.Duration, pricePerHour *float64, confirmedEnd bool) string {
	if pricePerHour == nil {
		return "price unknown"
	}
	cost := duration.Hours() * *pricePerHour
	text := fmt.Sprintf("$%.2f", cost)
	if !confirmedEnd {
		text += " so far"
	}
	return text
}

// sessionView is one session's figures as the page prints them, built once and drawn twice: as a
// card (phone) and as a table row (wide screen).
type sessionView struct {
	project, model, machine, where, price, ran, cost string
	tokensIn, tokensOut, tokensCached                int
}

// newSessionView reads one session: project, model, provider and shape, location, price, the
// date once, started, ended (or running, or end not confirmed), duration, cost, and the three
// token counts. The date is the start's calendar day, printed once ahead of every clock on the
// row, because clockFromStoredTime alone prints a bare "HH:MM:SS UTC" that reads identically for
// a session from a week ago and one from a minute ago. sessionSpan's endText carries a second
// date only when the session ended on a later day than it started.
func newSessionView(sess store.Session, live map[string]bool) sessionView {
	v := sessionView{
		project:      sess.Project,
		model:        sess.ModelName,
		machine:      sess.ShapeCode,
		where:        providerLabel(sess.ProviderKey),
		price:        "price unknown",
		tokensIn:     sess.TokensIn,
		tokensOut:    sess.TokensOut,
		tokensCached: sess.TokensCached,
	}
	if v.model == "" {
		v.model = "no model"
	}
	if sess.Location != "" {
		v.where += " " + sess.Location
	}
	if sess.PricePerHour != nil {
		v.price = fmt.Sprintf("$%.2f/hr", *sess.PricePerHour)
	}
	start, end, haveStart, confirmedEnd, endText := sessionSpan(sess, live)
	ran := ""
	if date := dateFromStoredTime(sess.AskedAt); date != "" {
		ran = date + " \u00b7 "
	}
	ran += "started " + clockFromStoredTime(sess.AskedAt)
	if sess.ServingSince != nil {
		ran += " \u00b7 serving since " + clockFromStoredTime(*sess.ServingSince)
	}
	ran += " \u00b7 " + endText
	v.cost = "time unknown"
	if haveStart {
		ran += " \u00b7 " + formatSessionDuration(end.Sub(start))
		v.cost = sessionCost(end.Sub(start), sess.PricePerHour, confirmedEnd)
	}
	v.ran = ran
	return v
}

func (v sessionView) cardLI() string {
	return `<li class="card sess"><div class="l1"><span>` + html.EscapeString(v.project+" \u00b7 "+v.machine) +
		`</span><span class="mono">` + html.EscapeString(v.cost) + `</span></div>` +
		`<div class="l2">` + html.EscapeString(v.model+" \u00b7 "+v.where+" \u00b7 "+v.price) +
		`<br>` + html.EscapeString(v.ran) + `</div>` +
		`<div class="nums"><span>in<b>` + groupInt(v.tokensIn) + `</b></span><span>out<b>` + groupInt(v.tokensOut) +
		`</b></span><span>cached<b>` + groupInt(v.tokensCached) + `</b></span></div></li>`
}

func (v sessionView) rowTR() string {
	cell := func(text, class string) string {
		if class != "" {
			class = ` class="` + class + `"`
		}
		return `<td` + class + `>` + html.EscapeString(text) + `</td>`
	}
	return `<tr>` + cell(v.project, "") + cell(v.model, "") + cell(v.machine, "") + cell(v.where, "") +
		cell(v.price, "mono") + cell(v.ran, "") + cell(v.cost, "mono") +
		cell(groupInt(v.tokensIn), "mono num") + cell(groupInt(v.tokensOut), "mono num") +
		cell(groupInt(v.tokensCached), "mono num") + `</tr>`
}

// groupInt prints n with a comma between every three digits.
func groupInt(n int) string {
	digits := strconv.Itoa(n)
	negative := strings.HasPrefix(digits, "-")
	digits = strings.TrimPrefix(digits, "-")
	var b strings.Builder
	for i, c := range digits {
		if i > 0 && (len(digits)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(c)
	}
	if negative {
		return "-" + b.String()
	}
	return b.String()
}

// sessionTotalsHTML is the totals tiles across every row the fragment just wrote: summed cost
// (each session's duration times its price, folded in the way a single row's is, silently, since
// the tiles name no one session's end state), summed time rented, and summed tokens. It is the
// out-of-band element that replaces #session-totals in the page.
func sessionTotalsHTML(sessions []store.Session) string {
	var totalCost float64
	var totalTime time.Duration
	haveCost := false
	var tokensIn, tokensOut, tokensCached int
	for _, sess := range sessions {
		tokensIn += sess.TokensIn
		tokensOut += sess.TokensOut
		tokensCached += sess.TokensCached
		start, haveStart := parseStoredTime(sess.AskedAt)
		if !haveStart {
			continue
		}
		end := time.Now()
		if sess.GoneAt != nil {
			if goneAt, ok := parseStoredTime(*sess.GoneAt); ok {
				end = goneAt
			}
		}
		totalTime += end.Sub(start)
		if sess.PricePerHour == nil {
			continue
		}
		totalCost += end.Sub(start).Hours() * *sess.PricePerHour
		haveCost = true
	}
	costText := "price unknown"
	if haveCost {
		costText = fmt.Sprintf("$%.2f", totalCost)
	}
	tile := func(k, v string) string {
		return `<div class="card"><div class="k">` + html.EscapeString(k) + `</div><div class="v">` + html.EscapeString(v) + `</div></div>`
	}
	return `<div id="session-totals" class="totals" hx-swap-oob="true">` +
		tile("Rentals", strconv.Itoa(len(sessions))) + tile("Spent", costText) +
		tile("Time rented", formatSessionDuration(totalTime)) + tile("Tokens in", groupInt(tokensIn)) +
		tile("Tokens out", groupInt(tokensOut)) + tile("Tokens cached", groupInt(tokensCached)) + `</div>`
}
