package console

import (
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/DavidTimar1/gpugate/internal/destroyer"
	"github.com/DavidTimar1/gpugate/internal/fit"
	"github.com/DavidTimar1/gpugate/internal/podkey"
	"github.com/DavidTimar1/gpugate/internal/provider"
	"github.com/DavidTimar1/gpugate/internal/store"
)

// The auto-move: while a project's model serves, GPUGate looks for a cheaper machine that can
// serve the same launch, rents it, switches the project's fixed address to it once it serves, and
// destroys the old one. The user allows the program to rent and destroy by itself for this, per
// project. The new machine's claim row and the machine_move row are committed before the provider
// is asked. Only a machine named by a machine_move row is ever destroyed here.

const (
	// moverInterval is how often the mover looks for a cheaper machine, per project. A look also
	// runs at once when a claim turns serving, when auto-move is switched on, and at program start
	// (lookForCheaperNow).
	moverInterval = 3 * time.Minute
	// moveNewMachineCeiling is the one-hour launch clock: a new machine not serving this long
	// after its claim was asked for is destroyed and the old one keeps serving.
	moveNewMachineCeiling = time.Hour
	// moveDrainCeiling is the longest the old machine is kept after the switch while requests are
	// still in flight on it.
	moveDrainCeiling = time.Hour
	// moveNewMustServeCleanFor is how long the new machine must have answered its model list on
	// every look before the old machine may be destroyed. A failed answer restarts the count.
	moveNewMustServeCleanFor = time.Minute
	// movePollInterval is how often a move under way is looked at.
	movePollInterval = 15 * time.Second
	// moveOldGoneChecksNeeded is how many consecutive looks must miss the old machine on a fresh
	// provider answer, with no destroy recorded, before it counts as gone (one miss can be a
	// stale answer).
	moveOldGoneChecksNeeded = 3
	// moveReadFailuresGiveUp is how many looks in a row may fail to read the move's two claims
	// before the move is failed and its new machine destroyed: 20 looks of movePollInterval is five
	// minutes. A move that cannot read its own claims can decide nothing, and its new machine bills
	// the whole time.
	moveReadFailuresGiveUp = 20
)

// requestStarted and requestEnded count the requests the fixed address is forwarding to ref.
func (s *Server) requestStarted(ref machineRef) {
	s.inflightMu.Lock()
	s.inflight[ref]++
	s.inflightMu.Unlock()
}

func (s *Server) requestEnded(ref machineRef) {
	s.inflightMu.Lock()
	if s.inflight[ref] <= 1 {
		delete(s.inflight, ref)
	} else {
		s.inflight[ref]--
	}
	s.inflightMu.Unlock()
}

func (s *Server) requestsInFlight(ref machineRef) int {
	s.inflightMu.Lock()
	defer s.inflightMu.Unlock()
	return s.inflight[ref]
}

// moverForever looks for cheaper machines on moverInterval for the life of the process. Switching
// a project's auto-move off stops it looking at once: every pass reads the switches fresh.
func (s *Server) moverForever() {
	ticker := time.NewTicker(moverInterval)
	defer ticker.Stop()
	for range ticker.C {
		s.moverPassGuarded()
	}
}

// lookForCheaperNow runs one pass in the background without waiting for the next tick: used when
// a claim turns serving, when a project's auto-move is switched on, and at program start. The
// pass reads the switches fresh, so a project with auto-move off is never looked at.
func (s *Server) lookForCheaperNow() {
	go s.moverPassGuarded()
}

// moverPassGuarded runs one pass and survives a panic inside it: the pass ends, the next tick
// looks again, and nothing is rented or destroyed on the strength of a half-finished pass. Passes
// are serialized by moverPassMu: considerMove checks for a move under way and then starts one, so
// two passes at once could start two moves for one project. A pass asked for while another runs
// waits its turn and then looks with fresh answers.
func (s *Server) moverPassGuarded() {
	s.moverPassMu.Lock()
	defer s.moverPassMu.Unlock()
	defer func() {
		if r := recover(); r != nil {
			s.reportPanic("the auto-move pass", r)
		}
	}()
	s.moverPass()
}

// moverPass looks once, for every project with auto-move on.
func (s *Server) moverPass() {
	slugs, err := s.store.AutoMoveProjects()
	if err != nil {
		s.log.Add("move", "could not read which projects have auto-move on: "+err.Error())
		return
	}
	if len(slugs) == 0 {
		return
	}
	var shapes []provider.Shape
	asked := false
	for _, slug := range slugs {
		s.considerMove(slug, func() []provider.Shape {
			if !asked {
				asked = true
				shapes, _ = s.liveShapesForMover()
			}
			return shapes
		})
	}
}

// liveShapesForMover reads every provider's shapes live, never the page's cache. A provider that
// cannot be asked is logged as "could not ask", contributes nothing, and is named in the second
// answer: that is never read as "nothing is cheaper". The launch offer puts this read on the page
// and into the recalculation cache only when the second answer is empty (a partial read would
// drop that provider's rows).
func (s *Server) liveShapesForMover() ([]provider.Shape, []string) {
	var all []provider.Shape
	var problems []string
	for _, p := range s.providers {
		if !p.CredentialsConfigured() {
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), providerCallTimeout)
		shapes, err := p.ListShapes(ctx)
		cancel()
		if err != nil {
			s.log.Add("move", "could not ask "+p.DisplayName()+" what it offers, so it was not looked at for a cheaper machine: "+err.Error())
			problems = append(problems, p.DisplayName()+" could not be asked: "+err.Error())
			continue
		}
		all = append(all, shapes...)
	}
	return all, problems
}

// logNoCheaperOnce writes why a look found no cheaper machine for a claim, once per claim per
// distinct reason: the same reason on the next look writes nothing, a changed one writes again.
func (s *Server) logNoCheaperOnce(claimID int64, reason string) {
	s.moverMu.Lock()
	if s.moveNoCheaperLogged == nil {
		s.moveNoCheaperLogged = map[int64]string{}
	}
	same := s.moveNoCheaperLogged[claimID] == reason
	s.moveNoCheaperLogged[claimID] = reason
	s.moverMu.Unlock()
	if !same {
		s.log.Add("move", reason)
	}
}

// logMoveSkipOnce writes text once per claim: why a claim is never moved.
func (s *Server) logMoveSkipOnce(claimID int64, text string) {
	s.moverMu.Lock()
	if s.moveSkipLogged == nil {
		s.moveSkipLogged = map[int64]bool{}
	}
	already := s.moveSkipLogged[claimID]
	s.moveSkipLogged[claimID] = true
	s.moverMu.Unlock()
	if !already {
		s.log.Add("move", text)
	}
}

// movablePods answers every machine of project the mover may replace, oldest claim first: the
// ones serving and the ones still loading, each carried by a fresh provider answer.
func (s *Server) movablePods(project string) ([]servingPod, error) {
	pods, err := s.servingPods(project, s.liveMachines())
	if err != nil {
		return nil, err
	}
	loading, err := s.store.ClaimsAwaitingServing()
	if err != nil {
		return nil, err
	}
	live := s.liveMachines()
	for _, c := range loading {
		if c.Project != project || c.ProviderID == nil || *c.ProviderID == "" || !live[c.ProviderKey][*c.ProviderID] {
			continue
		}
		pods = append(pods, servingPod{claim: c, providerKey: c.ProviderKey, providerID: *c.ProviderID, servedModel: s.byName[c.ModelName].HFRepo, format: s.byName[c.ModelName].Format})
	}
	sort.SliceStable(pods, func(i, j int) bool { return pods[i].claim.ID < pods[j].claim.ID })
	return pods, nil
}

// newMachineAnswers asks the new machine of a move for its model list, through the same request
// the readiness poll makes, and reports whether it named the served model. attempt says what the
// request saw, never the machine's address.
func (s *Server) newMachineAnswers(c store.Claim) (ok bool, attempt string) {
	id := derefString(c.ProviderID)
	m, have := s.machineByID(c.ProviderKey, id)
	if !have {
		return false, "a fresh provider answer does not list it"
	}
	baseURL, stage := s.servingBaseURL(m)
	if baseURL == "" || stage != "" {
		return false, "its serving address is not reachable yet"
	}
	key, err := podkey.Read(c.ProviderKey, id)
	if err != nil {
		return false, "no key on file for it"
	}
	hfRepo := s.byName[c.ModelName].HFRepo
	if hfRepo == "" {
		hfRepo = c.ModelName
	}
	return s.modelReady(baseURL+"/v1", key, hfRepo)
}

// considerMove looks at one project: when it has no move under way, it takes the first machine,
// serving or still loading, newest first, that has a cheaper candidate, and starts one move for
// it.
func (s *Server) considerMove(project string, liveShapes func() []provider.Shape) {
	open, err := s.store.ProjectHasOpenMove(project)
	if err != nil {
		s.log.Add("move", "could not check for a move under way on '"+project+"': "+err.Error())
		return
	}
	if open {
		return
	}
	pods, err := s.movablePods(project)
	if err != nil {
		s.log.Add("move", "could not read what '"+project+"' is serving or loading: "+err.Error())
		return
	}
	for i := len(pods) - 1; i >= 0; i-- {
		claim := pods[i].claim
		claimText := "claim " + strconv.FormatInt(claim.ID, 10)
		if hasIntent, err := s.store.HasOpenDestroyIntent(claim.ProviderKey, pods[i].providerID); err != nil || hasIntent {
			continue
		}
		raw, err := s.store.ClaimLaunchRequest(claim.ID)
		if err != nil {
			s.log.Add("move", "could not read the launch request of "+claimText+": "+err.Error())
			continue
		}
		if raw == "" {
			s.logMoveSkipOnce(claim.ID, "auto-move: "+claimText+" ("+claim.ShapeCode+") has no stored launch request, so it is never moved.")
			continue
		}
		var req launchRequest
		if err := json.Unmarshal([]byte(raw), &req); err != nil {
			s.logMoveSkipOnce(claim.ID, "auto-move: the launch request of "+claimText+" cannot be read, so it is never moved: "+err.Error())
			continue
		}
		// The project the pass is for, not the one the stored JSON names: the price cap that
		// bounds this move, and the cap startMove's launch is checked against, is this project's.
		req.Project = project
		if claim.PricePerHour == nil {
			s.logMoveSkipOnce(claim.ID, "auto-move: "+claimText+" ("+claim.ShapeCode+") has no recorded price, so nothing can be cheaper than it.")
			continue
		}
		candidate, noneBecause, ok := s.cheapestCandidate(req, claim, liveShapes())
		if !ok {
			if noneBecause != "" {
				s.logNoCheaperOnce(claim.ID, "auto-move: "+claimText+" ("+claim.ShapeCode+" at "+provider.PriceText(claim.PricePerHour)+") has no cheaper machine: "+noneBecause)
			}
			continue
		}
		// Switching the checkbox off between the pass starting and now still stops this at once.
		on, err := s.store.AutoMoveProjects()
		if err != nil || !slices.Contains(on, project) {
			return
		}
		s.startMove(project, claim, req, candidate)
		return
	}
}

// handleLaunchOffer answers GET /api/launch-offer: before the page's Launch button rents the row
// the user chose, it asks for two kinds of better machine and rents nothing (the launch stays the
// user's click). "cheaper" is the mover's rule (cheapestCandidate), asked only when the project
// has auto-move on. "fewer" is the cheapest ticked machine of the same card with fewer cards that
// holds the same launch (fewerCardsCandidate), asked only when the chosen machine has more than
// one card, whether or not auto-move is on. Any failure to find out answers "no offer" with the
// reason in the log, so the launch that was asked for is never held up. Every answer, offer or
// not, also carries "shapes": the table's rows from the same live read (see liveShapesForMover),
// which the page puts on screen so the table never disagrees with what the offer just saw. A
// complete read also becomes the recalculation cache (cachedFrom), as GET /api/machines does.
func (s *Server) handleLaunchOffer(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	providerKey, shapeCode, project, modelName := q.Get("provider"), q.Get("shape_code"), q.Get("project"), q.Get("model")
	if providerKey == "" || shapeCode == "" || project == "" {
		writeRefusal(w, http.StatusBadRequest, "provider, shape_code and project are all required.")
		return
	}
	ctxThousands, chats, detail := parseFitFigures(r)
	if detail != "" {
		writeRefusal(w, http.StatusBadRequest, detail)
		return
	}
	where := q.Get("where")
	if where == "" {
		where = "any"
	}
	on, err := s.store.AutoMoveProjects()
	if err != nil {
		s.log.Add("console", "could not read which projects have auto-move on, so no cheaper machine is offered for this launch: "+err.Error())
		on = nil
	}
	autoMoveOn := slices.Contains(on, project)
	shapes, problems := s.liveShapesForMover()
	// The answer carries the table's rows from this same read, so the page can show what the offer
	// saw. They are left out when a provider could not be asked: a partial read would drop that
	// provider's rows from the table.
	answer := map[string]any{"availability_problems": emptyIfNil(problems)}
	if len(problems) == 0 {
		model, _ := s.modelForFit(modelName)
		plan := fit.Plan{ContextTokens: fit.ContextTokensFromThousands(ctxThousands), ConcurrentChats: chats}
		byPrice := slices.Clone(shapes)
		sortByPrice(byPrice)
		if rows, err := s.tableRows(byPrice, model, plan); err != nil {
			s.log.Add("console", "could not read the shape ticks for the launch offer's table rows: "+err.Error())
		} else {
			answer["shapes"] = rows
			// The page puts these rows on screen, so the recalculation cache takes the same read: a
			// later GET /api/models must judge the rows the table shows.
			s.cacheMu.Lock()
			s.cachedFrom = byPrice
			s.haveCache = true
			s.cacheMu.Unlock()
		}
	}
	noOffer := func() {
		answer["offer"] = false
		writeJSON(w, http.StatusOK, answer)
	}
	var chosen *provider.Shape
	for i := range shapes {
		if shapes[i].ProviderKey == providerKey && shapes[i].Code == shapeCode {
			chosen = &shapes[i]
			break
		}
	}
	if chosen == nil || chosen.OnDemandPricePerHour == nil {
		noOffer()
		return
	}
	req := launchRequest{
		ProviderKey: providerKey, ShapeCode: shapeCode, Project: project, Model: modelName,
		ContextThousands: ctxThousands, Chats: chats, Where: where,
	}
	describe := func(sh provider.Shape) map[string]any {
		providerName := sh.ProviderKey
		if p := provider.ByKey(s.providers, sh.ProviderKey); p != nil {
			providerName = p.DisplayName()
		}
		return map[string]any{
			"provider_key": sh.ProviderKey, "provider_name": providerName,
			"shape_code": sh.Code, "display_name": sh.DisplayName,
			"gpu_count": sh.GPUCount, "price_per_hour": *sh.OnDemandPricePerHour,
		}
	}
	answer["chosen"] = describe(*chosen)
	answer["auto_move_on"] = autoMoveOn
	if autoMoveOn {
		// ID 0: this is not a running claim; the price and shape code are all the rule reads of it.
		asClaim := store.Claim{ShapeCode: shapeCode, PricePerHour: chosen.OnDemandPricePerHour}
		if candidate, _, ok := s.cheapestCandidate(req, asClaim, shapes); ok {
			answer["cheaper"] = describe(candidate)
		}
	}
	if candidate, ok := s.fewerCardsCandidate(req, *chosen, shapes); ok {
		answer["fewer"] = describe(candidate)
	}
	if answer["cheaper"] == nil && answer["fewer"] == nil {
		noOffer()
		return
	}
	answer["offer"] = true
	writeJSON(w, http.StatusOK, answer)
}

// cheapestCandidate answers the cheapest shape a move could use for the launch req made on claim,
// or false when there is none, with the reason there is none (empty when the reason was already
// logged by the caller it came from). A candidate is ticked, judged launchable for the same
// model, context and chat count (so unservable formats and shapes are out), orderable where the
// original launch's location choice allows, a different shape code from the running machine's,
// and priced strictly below the running claim's recorded price. A missing price is never a
// candidate.
func (s *Server) cheapestCandidate(req launchRequest, claim store.Claim, shapes []provider.Shape) (provider.Shape, string, bool) {
	model, detail := s.modelForFit(req.Model)
	if model == nil {
		if detail != "" {
			s.logMoveSkipOnce(claim.ID, "auto-move: claim "+strconv.FormatInt(claim.ID, 10)+": "+detail)
		}
		return provider.Shape{}, "", false
	}
	ticked, err := s.store.TickedShapeCodes()
	if err != nil {
		s.log.Add("move", "could not read the shape ticks: "+err.Error())
		return provider.Shape{}, "", false
	}
	priceCap, err := s.store.ProjectPriceCap(req.Project)
	if err != nil {
		s.log.Add("move", "could not read the price cap of project '"+req.Project+"', so no cheaper machine is chosen: "+err.Error())
		return provider.Shape{}, "", false
	}
	plan := fit.Plan{ContextTokens: fit.ContextTokensFromThousands(req.ContextThousands), ConcurrentChats: req.Chats}
	var candidates []provider.Shape
	// The tallies say why the shapes priced below the running machine were turned away; the
	// cheapest one turned away is named with its reason.
	cheaper, notTicked, noLocation, notLaunchable, locationRefused, overCap := 0, 0, 0, 0, 0, 0
	// cheapestOut is the cheapest shape turned away that CAN hold this launch, falling back to the
	// cheapest of any, so the sentence never names a machine too small for the model while one
	// that fits was turned away. unticked collects the capable shapes only ticking would allow.
	var cheapestOut, cheapestOutAny *provider.Shape
	cheapestOutWhy, cheapestOutAnyWhy := "", ""
	var unticked []provider.Shape
	cheaperThan := func(a provider.Shape, b *provider.Shape) bool {
		return b == nil || *a.OnDemandPricePerHour < *b.OnDemandPricePerHour ||
			(*a.OnDemandPricePerHour == *b.OnDemandPricePerHour && a.Code < b.Code)
	}
	reject := func(shape provider.Shape, why string, canHold bool) {
		sc := shape
		if cheaperThan(shape, cheapestOutAny) {
			cheapestOutAny, cheapestOutAnyWhy = &sc, why
		}
		if canHold && cheaperThan(shape, cheapestOut) {
			cheapestOut, cheapestOutWhy = &sc, why
		}
	}
	// canHold answers whether shape could serve this launch if it were allowed: orderable,
	// launchable for this model, context and chat count, and on the original location choice.
	canHold := func(shape provider.Shape) bool { return s.shapeHoldsLaunch(model, plan, req.Where, shape) }
	for _, shape := range shapes {
		if shape.Code == claim.ShapeCode || shape.OnDemandPricePerHour == nil || *shape.OnDemandPricePerHour >= *claim.PricePerHour {
			continue
		}
		// Over the price cap: never a candidate, so the mover does not rent it only to be refused
		// by startLaunch every look. Not counted in the reasons below either.
		if priceCap != nil && *shape.OnDemandPricePerHour > *priceCap {
			overCap++
			continue
		}
		cheaper++
		if !ticked[shape.Code] {
			notTicked++
			fits := canHold(shape)
			if fits {
				unticked = append(unticked, shape)
			}
			reject(shape, "it is not ticked", fits)
			continue
		}
		if len(shape.OrderableLocations) == 0 {
			noLocation++
			reject(shape, "it is not orderable anywhere right now", false)
			continue
		}
		if verdict := s.judge(model, plan, shape); verdict == nil || verdict.Verdict != string(fit.Launchable) {
			notLaunchable++
			why := "it cannot serve this model at this context and chat count"
			if verdict != nil && verdict.Reason != "" {
				why = verdict.Reason
			}
			reject(shape, why, false)
			continue
		}
		_, _, refusal := ResolveLocations(req.Where, shape.ProviderKey, shape.OrderableLocations)
		if refusal != "" {
			locationRefused++
			reject(shape, "no orderable location matches the original launch's location choice", false)
			continue
		}
		candidates = append(candidates, shape)
	}
	if len(candidates) == 0 {
		if cheaper == 0 {
			if overCap > 0 {
				return provider.Shape{}, strconv.Itoa(overCap) + " shapes are priced below it, all over the price cap of project '" + req.Project + "'.", false
			}
			return provider.Shape{}, "no shape is priced below it.", false
		}
		if cheapestOut == nil {
			cheapestOut, cheapestOutWhy = cheapestOutAny, cheapestOutAnyWhy
		}
		sort.SliceStable(unticked, func(i, j int) bool { return cheaperThan(unticked[i], &unticked[j]) })
		tickable := ""
		if len(unticked) > 0 {
			names := []string{}
			for i, sh := range unticked {
				if i == 3 {
					break
				}
				names = append(names, sh.Code+" ("+provider.PriceText(sh.OnDemandPricePerHour)+")")
			}
			tickable = " Cheaper shapes that could hold this launch but are not ticked: " + strings.Join(names, ", ") + "."
		}
		return provider.Shape{}, strconv.Itoa(cheaper) + " shapes are priced below it: " +
			strconv.Itoa(notTicked) + " not ticked, " + strconv.Itoa(noLocation) + " not orderable, " +
			strconv.Itoa(notLaunchable) + " cannot serve this launch, " + strconv.Itoa(locationRefused) +
			" off the location choice. Cheapest turned away: " +
			cheapestOut.Code + " (" + cheapestOutWhy + ")." + tickable, false
	}
	sort.SliceStable(candidates, func(i, j int) bool {
		pi, pj := *candidates[i].OnDemandPricePerHour, *candidates[j].OnDemandPricePerHour
		if pi != pj {
			return pi < pj
		}
		return candidates[i].Code < candidates[j].Code
	})
	return candidates[0], "", true
}

// shapeHoldsLaunch answers whether shape could serve the launch if it were allowed: orderable,
// launchable for this model, context and chat count, and on the original location choice. It
// leaves out the shape tick, which each caller judges itself.
func (s *Server) shapeHoldsLaunch(model *fit.Model, plan fit.Plan, where string, shape provider.Shape) bool {
	if len(shape.OrderableLocations) == 0 {
		return false
	}
	if verdict := s.judge(model, plan, shape); verdict == nil || verdict.Verdict != string(fit.Launchable) {
		return false
	}
	_, _, refusal := ResolveLocations(where, shape.ProviderKey, shape.OrderableLocations)
	return refusal == ""
}

// fewerCardsCandidate answers the cheapest ticked shape of the same card as chosen (the same
// GPUName the shape table shows) with fewer cards that holds the launch req: ticked, orderable,
// launchable for the same model, context and chat count, and on the launch's location choice
// (shapeHoldsLaunch). It is not required to be cheaper than chosen; the offer names both prices.
func (s *Server) fewerCardsCandidate(req launchRequest, chosen provider.Shape, shapes []provider.Shape) (provider.Shape, bool) {
	if chosen.GPUCount <= 1 {
		return provider.Shape{}, false
	}
	model, _ := s.modelForFit(req.Model)
	if model == nil {
		return provider.Shape{}, false
	}
	ticked, err := s.store.TickedShapeCodes()
	if err != nil {
		s.log.Add("console", "could not read the shape ticks, so no machine with fewer cards is offered: "+err.Error())
		return provider.Shape{}, false
	}
	priceCap, err := s.store.ProjectPriceCap(req.Project)
	if err != nil {
		s.log.Add("console", "could not read the price cap of project '"+req.Project+"', so no machine with fewer cards is offered: "+err.Error())
		return provider.Shape{}, false
	}
	plan := fit.Plan{ContextTokens: fit.ContextTokensFromThousands(req.ContextThousands), ConcurrentChats: req.Chats}
	var best *provider.Shape
	for i := range shapes {
		sh := shapes[i]
		if sh.GPUName != chosen.GPUName || sh.GPUCount >= chosen.GPUCount || sh.OnDemandPricePerHour == nil || (priceCap != nil && *sh.OnDemandPricePerHour > *priceCap) || !ticked[sh.Code] || !s.shapeHoldsLaunch(model, plan, req.Where, sh) {
			continue
		}
		if best == nil || *sh.OnDemandPricePerHour < *best.OnDemandPricePerHour ||
			(*sh.OnDemandPricePerHour == *best.OnDemandPricePerHour && sh.Code < best.Code) {
			best = &shapes[i]
		}
	}
	if best == nil {
		return provider.Shape{}, false
	}
	return *best, true
}

// startMove launches req on candidate through the same startLaunch the page's Launch button uses,
// with the serve options rebuilt there for the new shape (tensor-parallel size and the multi-card
// environment follow its card count). The move record is committed in beforeCreate, after the new
// claim row and before the provider is asked, and the one log line naming project, model, old
// shape and price and new shape and price is written there too.
func (s *Server) startMove(project string, old store.Claim, req launchRequest, candidate provider.Shape) {
	newReq := req
	newReq.ProviderKey = candidate.ProviderKey
	newReq.ShapeCode = candidate.Code
	// The new machine keeps the old machine's idle timeout as it stands now, which the user may
	// have changed on the strip since the launch request was stored.
	if current, found, err := s.store.GetClaim(old.ID); err != nil {
		s.log.Add("move", "could not read claim "+strconv.FormatInt(old.ID, 10)+" for its idle timeout, so the new machine takes the launch request's: "+err.Error())
	} else if found {
		idle := current.IdleMinutes
		newReq.IdleMinutes = &idle
	}
	var moveID, newID int64
	_, refusal := s.startLaunch(context.Background(), newReq, launchOptions{
		belowPrice: old.PricePerHour,
		beforeCreate: func(newClaimID int64, price *float64) error {
			id, err := s.store.RecordMove(project, old.ID, newClaimID)
			if err != nil {
				return err
			}
			moveID, newID = id, newClaimID
			s.log.Add("move", "auto-move: project '"+project+"', model "+req.Model+": "+
				old.ShapeCode+" at "+provider.PriceText(old.PricePerHour)+" -> "+candidate.Code+" at "+provider.PriceText(price)+
				" (move "+strconv.FormatInt(id, 10)+", new claim "+strconv.FormatInt(newClaimID, 10)+").")
			return nil
		},
	})
	if refusal != nil {
		s.log.Add("move", "auto-move for '"+project+"' to "+candidate.Code+" was not started: "+refusal.detail)
		return
	}
	go s.runMoveGuarded(store.Move{ID: moveID, Project: project, OldClaimID: old.ID, NewClaimID: newID, State: "starting"})
}

// resumeMoves finishes every move still under way when this process starts: a new machine still
// loading keeps its hour from the new claim's creation, a serving one proceeds to drain and
// destroy the old.
func (s *Server) resumeMoves() {
	moves, err := s.store.OpenMoves()
	if err != nil {
		s.log.Add("move", "could not read the moves under way: "+err.Error())
		return
	}
	for _, m := range moves {
		s.log.Add("move", "resuming move "+strconv.FormatInt(m.ID, 10)+" of project '"+m.Project+"' after a restart.")
		go s.runMoveGuarded(m)
	}
}

// runMoveGuarded runs runMove for m unless one is already running for it.
func (s *Server) runMoveGuarded(m store.Move) {
	if m.NewClaimID == 0 {
		return
	}
	s.moverMu.Lock()
	if s.moverRunning[m.ID] {
		s.moverMu.Unlock()
		return
	}
	s.moverRunning[m.ID] = true
	s.moverMu.Unlock()
	defer func() {
		s.moverMu.Lock()
		delete(s.moverRunning, m.ID)
		s.moverMu.Unlock()
	}()
	// A panic inside runMove must not leave a move open with nobody running it: its new machine
	// bills until the move closes. The move is re-read from the store, which holds its true state,
	// and run again a few times; after that a restart resumes it (resumeMoves).
	for attempt := 1; attempt <= moveRunAttempts; attempt++ {
		if !s.runMoveRecovered(m) {
			return
		}
		time.Sleep(movePollInterval)
		open, err := s.store.OpenMoves()
		if err != nil {
			s.log.Add("move", "could not read the moves under way after a failure: "+err.Error())
			return
		}
		found := false
		for _, o := range open {
			if o.ID == m.ID {
				m, found = o, true
			}
		}
		if !found {
			return
		}
	}
	s.log.Add("move", "move "+strconv.FormatInt(m.ID, 10)+" of project '"+m.Project+"' failed "+strconv.Itoa(moveRunAttempts)+" times in GPUGate's own code and is left open; a restart resumes it.")
}

// moveRunAttempts is how many times a move that panicked is run again before it is left to a
// restart.
const moveRunAttempts = 5

// runMoveRecovered runs runMove once and reports whether it panicked.
func (s *Server) runMoveRecovered(m store.Move) (panicked bool) {
	defer func() {
		if r := recover(); r != nil {
			panicked = true
			s.reportPanic("move "+strconv.FormatInt(m.ID, 10)+" of project '"+m.Project+"'", r)
		}
	}()
	s.runMove(m)
	return false
}

// machineLiveNow answers whether a fresh answer from providerKey lists providerID, and whether
// that provider's last answer was usable at all.
func (s *Server) machineLiveNow(providerKey, providerID string) (live, answered bool) {
	set, ok := s.liveMachines()[providerKey]
	if !ok {
		return false, false
	}
	return set[providerID], true
}

// runMove carries one move from 'starting' to 'done' or 'failed', looking every movePollInterval.
// It touches only the two machines the move record names.
func (s *Server) runMove(m store.Move) {
	moveText := "move " + strconv.FormatInt(m.ID, 10) + " of project '" + m.Project + "'"
	oldMisses := 0
	newMisses := 0
	zeroReads := 0
	// cleanSince is when the new machine began answering its own model list on every look; zero
	// while it has not. A failed answer resets it.
	var cleanSince time.Time
	failingLogged := false
	readFailures := 0
	readGiveUpLogged := false
	first := true
	for {
		if !first {
			time.Sleep(movePollInterval)
		}
		first = false

		oldClaim, haveOld, err := s.store.GetClaim(m.OldClaimID)
		newClaim, haveNew, err2 := s.store.GetClaim(m.NewClaimID)
		if err != nil || err2 != nil || !haveOld || !haveNew {
			readFailures++
			cause := "a claim is missing"
			if err != nil {
				cause = err.Error()
			} else if err2 != nil {
				cause = err2.Error()
			}
			s.log.Add("move", moveText+": could not read its claims ("+strconv.Itoa(readFailures)+" looks in a row): "+cause)
			if readFailures >= moveReadFailuresGiveUp {
				// The wide read is what is failing, so the new machine is named by a short read of
				// its own. Only a move that can name it is failed: closing a move whose new machine
				// cannot be named would leave it billing with nobody looking.
				newMachine, found, mErr := s.store.GetClaimMachine(m.NewClaimID)
				if mErr == nil && found {
					s.log.Add("move", moveText+": its claims could not be read for "+strconv.Itoa(readFailures)+" looks in a row, so the move is failed and the new machine destroyed.")
					s.failMove(m, newMachine, "its claims could not be read for "+strconv.Itoa(readFailures)+" looks in a row")
					return
				}
				if !readGiveUpLogged {
					s.log.Add("move", moveText+": the new machine's claim cannot be read either, so the move cannot be failed and is kept; trying again every look.")
					readGiveUpLogged = true
				}
			}
			continue
		}
		readFailures = 0

		// The old machine going away during the move (Delete, the inactivity timeout, the provider)
		// is tolerated: before the switch the move is abandoned and the new machine destroyed,
		// because nobody is left to serve, unless the new machine already serves and is listed, in
		// which case it is kept and the move is finished; after the switch, the move is finished.
		oldGone := s.moveOldIsGone(oldClaim, &oldMisses)

		switch m.State {
		case "starting":
			if oldGone {
				// The new machine is kept when it already serves and a fresh answer lists it: it is
				// then the project's only machine, and destroying it would leave nothing serving.
				if newClaim.ServingState == "serving" {
					if live, _ := s.machineLiveNow(newClaim.ProviderKey, derefString(newClaim.ProviderID)); live {
						s.carryQuietClock(oldClaim, newClaim)
						s.log.Add("move", moveText+": the old machine went away while the new "+newClaim.ShapeCode+" serves, so the new machine is kept and the move is done.")
						s.finishDrained(m, moveText, "the old machine went away and the new one serves")
						return
					}
				}
				s.log.Add("move", moveText+": the old machine is gone before the new one served, so the move is dropped and the new machine destroyed.")
				s.failMove(m, newClaim, "the old machine went away before the new one served")
				return
			}
			switch {
			case newClaim.State == "refused" || newClaim.State == "abandoned":
				d := ""
				if newClaim.Detail != nil {
					d = *newClaim.Detail
				}
				s.log.Add("move", "Auto-move for '"+m.Project+"': "+providerLabel(newClaim.ProviderKey)+" did not give a machine for "+newClaim.ShapeCode+" ("+readableRefusal(d)+"); the next look picks the cheapest candidate afresh, and the old machine keeps serving.")
				s.failMove(m, newClaim, "the new machine was refused: "+d)
				return
			case newClaim.ServingState == "serving":
				if live, _ := s.machineLiveNow(newClaim.ProviderKey, derefString(newClaim.ProviderID)); !live {
					continue // serving by our notes; wait for a fresh answer that lists it
				}
				s.switchMove(&m, oldClaim, newClaim)
				zeroReads = 0
				if ok, _ := s.newMachineAnswers(newClaim); ok {
					cleanSince = time.Now()
				}
			case newClaim.ServingState == "gave_up":
				s.log.Add("move", moveText+": the new machine "+newClaim.ShapeCode+" never served; destroying it, and the old machine keeps serving.")
				s.failMove(m, newClaim, "the new machine stopped before it served")
				return
			default:
				asked, ok := parseStoredTime(newClaim.AskedAt)
				if ok && time.Since(asked) > moveNewMachineCeiling {
					s.log.Add("move", moveText+": the new machine "+newClaim.ShapeCode+" has not served an hour after it was asked for; destroying it, and the old machine keeps serving.")
					s.failMove(m, newClaim, "the new machine did not serve within an hour")
					return
				}
			}
		case "draining":
			// The old machine is destroyed only while the new one still serves: a new machine that
			// died or vanished since the switch leaves the old one as the only thing serving.
			if s.moveNewIsGone(newClaim, &newMisses) {
				s.log.Add("move", moveText+": the new machine "+newClaim.ShapeCode+" of the move is gone, so the old machine keeps serving and the move is closed as failed; the next look picks the cheapest candidate afresh.")
				s.failMove(m, newClaim, "the new machine was gone after the switch; the old machine was kept")
				return
			}
			if oldGone {
				s.finishDrained(m, moveText, "the old machine was already gone")
				return
			}
			// The new machine must answer its own model list on every look, not only be listed by the
			// provider: the old machine is the fallback until it has done so for a minute.
			if ok, attempt := s.newMachineAnswers(newClaim); ok {
				if cleanSince.IsZero() {
					cleanSince = time.Now()
				}
				failingLogged = false
			} else {
				if !cleanSince.IsZero() || !failingLogged {
					s.log.Add("move", moveText+": the new machine "+newClaim.ShapeCode+" did not answer its model list ("+attempt+"); the old machine is kept and the minute of clean answers starts again.")
					failingLogged = true
				}
				cleanSince = time.Time{}
			}
			cleanFor := time.Duration(0)
			if !cleanSince.IsZero() {
				cleanFor = time.Since(cleanSince)
			}
			oldRef := machineRef{oldClaim.ProviderKey, derefString(oldClaim.ProviderID)}
			if s.requestsInFlight(oldRef) == 0 {
				zeroReads++
			} else {
				zeroReads = 0
			}
			switchedAt, ok := parseStoredTime(m.SwitchedAt)
			overdue := ok && time.Since(switchedAt) > moveDrainCeiling
			if cleanFor >= moveNewMustServeCleanFor && (zeroReads >= 2 || overdue) {
				s.log.Add("move", moveText+": the new machine "+newClaim.ShapeCode+" has answered its model list on every look for "+cleanFor.Round(time.Second).String()+", so the old machine is destroyed now.")
				s.destroyOldOfMove(m, moveText, oldClaim, overdue && zeroReads < 2)
				return
			}
			if overdue {
				s.log.Add("move", moveText+": an hour after the switch the new machine "+newClaim.ShapeCode+" is still not answering its model list cleanly, so the move is closed as failed, the new machine destroyed, and the old machine keeps serving; the next look picks the cheapest candidate afresh.")
				s.failMove(m, newClaim, "the new machine did not answer cleanly within an hour of the switch; the old machine was kept")
				return
			}
		default:
			return
		}
	}
}

func derefString(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

// moveOldIsGone reports whether the old machine of a move has gone away on its own: a destroy
// was recorded for it and confirmed, or fresh answers have missed it moveOldGoneChecksNeeded times
// running with no destroy recorded. A destroy still in flight is not gone.
func (s *Server) moveOldIsGone(old store.Claim, misses *int) bool {
	id := derefString(old.ProviderID)
	if id == "" {
		return true
	}
	exists, gone, err := s.store.DestroyIntentState(old.ProviderKey, id)
	if err == nil && exists {
		return gone
	}
	live, answered := s.machineLiveNow(old.ProviderKey, id)
	if !answered || live {
		*misses = 0
		return false
	}
	*misses++
	return *misses >= moveOldGoneChecksNeeded
}

// moveNewIsGone reports whether the new machine of a draining move has stopped serving: a destroy
// was recorded for it, or fresh answers have missed it, or its claim no longer reads serving,
// moveOldGoneChecksNeeded times running. A provider that could not be asked counts for nothing
// either way (one "could not ask" is not "gone").
func (s *Server) moveNewIsGone(newClaim store.Claim, misses *int) bool {
	id := derefString(newClaim.ProviderID)
	if id == "" {
		return true
	}
	if exists, _, err := s.store.DestroyIntentState(newClaim.ProviderKey, id); err == nil && exists {
		return true
	}
	live, answered := s.machineLiveNow(newClaim.ProviderKey, id)
	if !answered {
		return false
	}
	if live && newClaim.ServingState == "serving" {
		*misses = 0
		return false
	}
	*misses++
	return *misses >= moveOldGoneChecksNeeded
}

// switchMove marks the move draining the moment the new machine serves and a fresh answer lists
// it: new requests already go to the newest matching machine (proxyTarget), so this only has to
// carry the old machine's quiet clock onto the new claim, so the inactivity timeout measures the
// project's own quiet time across a move, and write the state down.
func (s *Server) switchMove(m *store.Move, oldClaim, newClaim store.Claim) {
	s.carryQuietClock(oldClaim, newClaim)
	if err := s.store.MarkMoveDraining(m.ID); err != nil {
		s.log.Add("move", "could not record that move "+strconv.FormatInt(m.ID, 10)+" switched: "+err.Error())
	}
	m.State = "draining"
	m.SwitchedAt = time.Now().UTC().Format(store.TimeLayout)
	s.log.Add("move", "move "+strconv.FormatInt(m.ID, 10)+" of project '"+m.Project+"': "+newClaim.ShapeCode+" serves and now takes the project's requests; the old "+oldClaim.ShapeCode+" is destroyed once the new one has answered its model list on every look for a minute and nothing is in flight on the old one.")
}

// carryQuietClock writes the old machine's quiet-since moment onto the new claim as its
// last_output_at. An old machine whose claim is no longer listed as loading or serving has no
// quiet clock to carry (quietSince would fall back to this process's start, which is later than
// the truth), so the new claim keeps its own serving_since.
func (s *Server) carryQuietClock(oldClaim, newClaim store.Claim) {
	oldID := derefString(oldClaim.ProviderID)
	info, listed := s.allServingInfo()[oldID]
	var quiet time.Time
	if listed {
		quiet = s.quietSince(info)
	}
	s.lastOutputMu.Lock()
	if at, ok := s.lastOutputAt[machineRef{oldClaim.ProviderKey, oldID}]; ok && at.After(quiet) {
		quiet = at
	}
	s.lastOutputMu.Unlock()
	if quiet.IsZero() {
		return
	}
	if err := s.store.SetClaimLastOutputAt(newClaim.ID, quiet.UTC().Format(store.TimeLayout)); err != nil {
		s.log.Add("move", "could not carry the quiet clock onto claim "+strconv.FormatInt(newClaim.ID, 10)+": "+err.Error())
	}
}

// destroyOldOfMove destroys the old machine through the normal destroy path (intent first;
// confirmed gone only by a fresh provider answer) and settles the move once it is confirmed.
func (s *Server) destroyOldOfMove(m store.Move, moveText string, old store.Claim, overdue bool) {
	id := derefString(old.ProviderID)
	target := provider.ByKey(s.providers, old.ProviderKey)
	if target == nil {
		s.log.Add("move", moveText+": the old machine's provider '"+old.ProviderKey+"' is unknown; it is not destroyed.")
		_ = s.store.FinishMove(m.ID, "failed", "the old machine's provider is unknown")
		return
	}
	reason := "moved to a cheaper machine"
	if overdue {
		reason += " (requests were still in flight after an hour)"
	}
	s.destroyMachineForMove(target, id, old.ShapeCode+" ("+old.Hostname+")", old.PricePerHour, reason, false)
	exists, gone, err := s.store.DestroyIntentState(old.ProviderKey, id)
	if err == nil && exists && gone {
		s.finishDrained(m, moveText, "the old machine is destroyed")
		return
	}
	s.log.Add("move", moveText+": the old machine "+id+" is not confirmed destroyed; it is shown on the strip as still billing.")
	_ = s.store.FinishMove(m.ID, "failed", "the old machine could not be confirmed destroyed")
}

func (s *Server) finishDrained(m store.Move, moveText, detail string) {
	if err := s.store.FinishMove(m.ID, "done", detail); err != nil {
		s.log.Add("move", moveText+": could not record that it is done: "+err.Error())
		return
	}
	s.log.Add("move", moveText+" is done: "+detail+".")
}

// failMove ends a move whose new machine did not come up: the new machine is destroyed if it
// exists, and the old machine is left alone. Nothing is remembered about the shape: the next look
// picks the cheapest candidate afresh.
func (s *Server) failMove(m store.Move, newClaim store.Claim, detail string) {
	if id := derefString(newClaim.ProviderID); id != "" {
		if target := provider.ByKey(s.providers, newClaim.ProviderKey); target != nil {
			s.destroyMachineForMove(target, id, newClaim.ShapeCode+" ("+newClaim.Hostname+")", newClaim.PricePerHour, "the new machine of a move did not come up", newClaim.ServingState != "serving")
		}
	}
	if err := s.store.FinishMove(m.ID, "failed", detail); err != nil {
		s.log.Add("move", "could not record that move "+strconv.FormatInt(m.ID, 10)+" failed: "+err.Error())
	}
}

// destroyMachineForMove writes the destroy intent unless one is already open, then runs the
// normal destroy and waits for it to be confirmed gone by a fresh answer.
func (s *Server) destroyMachineForMove(target provider.Provider, providerID, displayName string, price *float64, reason string, readLogFirst bool) {
	open, err := s.store.HasOpenDestroyIntent(target.Key(), providerID)
	if err != nil {
		s.log.Add("move", "could not check for an open destroy of "+providerID+": "+err.Error())
		return
	}
	if open {
		// Something else is already destroying it (the inactivity timeout, Delete): wait for that,
		// for as long as a destroy itself is retried.
		for deadline := time.Now().Add(destroyer.GiveUpAfter); time.Now().Before(deadline); {
			time.Sleep(movePollInterval)
			still, err := s.store.HasOpenDestroyIntent(target.Key(), providerID)
			if err == nil && !still {
				return
			}
		}
		return
	}
	if err := s.store.RecordDestroyIntent(target.Key(), providerID, displayName, price); err != nil {
		s.log.Add("move", "could not record the destroy intent for "+displayName+"; nothing was asked of "+target.DisplayName()+": "+err.Error())
		return
	}
	s.log.Add("instances", "destroying "+displayName+" ("+providerID+") on "+target.DisplayName()+": "+reason+".")
	s.setDestroyNotice(target.Key(), providerID, "destroying: "+reason)
	s.runDestroy(target, providerID, displayName, readLogFirst, time.Now())
}
