package console

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/DavidTimar1/gpugate/internal/fit"
	"github.com/DavidTimar1/gpugate/internal/podkey"
	"github.com/DavidTimar1/gpugate/internal/provider"
	"github.com/DavidTimar1/gpugate/internal/sshtunnel"
	"github.com/DavidTimar1/gpugate/internal/store"
)

// parseFitFigures reads the context and chats query parameters shared by the endpoints that judge
// fit, and refuses plainly when either is missing or not a positive whole number. This program
// never guesses a figure nobody supplied.
func parseFitFigures(r *http.Request) (contextThousands int, chats int, detail string) {
	ctxRaw := r.URL.Query().Get("context")
	chatsRaw := r.URL.Query().Get("chats")
	ctxVal, err := strconv.Atoi(ctxRaw)
	if err != nil || ctxVal <= 0 {
		return 0, 0, "context must be a whole number of thousands of tokens, greater than zero."
	}
	chatsVal, err := strconv.Atoi(chatsRaw)
	if err != nil || chatsVal <= 0 {
		return 0, 0, "chats must be a whole number, greater than zero."
	}
	return ctxVal, chatsVal, ""
}

// parseOptionalFloat reads the query parameter name, answering nil when it is absent or blank (an
// empty box sends no flag) or refusing by name when it cannot be read as a number.
func parseOptionalFloat(r *http.Request, name string) (*float64, string) {
	raw := r.URL.Query().Get(name)
	if raw == "" {
		return nil, ""
	}
	v, err := strconv.ParseFloat(raw, 64)
	if err != nil {
		return nil, name + " must be a number."
	}
	return &v, ""
}

// parseOptionalInt is parseOptionalFloat's twin for a whole-number query parameter.
func parseOptionalInt(r *http.Request, name string) (*int, string) {
	raw := r.URL.Query().Get(name)
	if raw == "" {
		return nil, ""
	}
	v, err := strconv.Atoi(raw)
	if err != nil {
		return nil, name + " must be a whole number."
	}
	return &v, ""
}

// parseOptionalBool reads the query parameter name as "1" or "0", answering nil when it is absent
// or blank (nil means on) or refusing by name when it carries anything else.
func parseOptionalBool(r *http.Request, name string) (*bool, string) {
	raw := r.URL.Query().Get(name)
	if raw == "" {
		return nil, ""
	}
	switch raw {
	case "1":
		v := true
		return &v, ""
	case "0":
		v := false
		return &v, ""
	default:
		return nil, name + " must be 1 or 0."
	}
}

// refusalReasonMax caps a provider's refusal sentence in the log: a provider's refusal can be a
// long body, and the claim row keeps all of it.
const refusalReasonMax = 300

// providerDetailRE picks the human sentence out of a provider's JSON refusal body
// ({"detail":"..."}), which is how a stored refusal reads.
var providerDetailRE = regexp.MustCompile(`"detail":\s*"([^"]*)"`)

// readableRefusal turns a provider's refusal text into one readable sentence for the log: the
// JSON detail when the body carries one, otherwise the first line, capped, with no closing full
// stop (the caller adds its own).
func readableRefusal(raw string) string {
	reason := strings.TrimSpace(raw)
	if m := providerDetailRE.FindStringSubmatch(reason); m != nil {
		reason = m[1]
	}
	if i := strings.IndexAny(reason, "\r\n"); i >= 0 {
		reason = strings.TrimSpace(reason[:i])
	}
	if len(reason) > refusalReasonMax {
		reason = strings.ToValidUTF8(reason[:refusalReasonMax], "") + "..."
	}
	reason = strings.TrimRight(reason, ".")
	if reason == "" {
		reason = "no reason given"
	}
	return reason
}

// ResolveLocations turns the user's "where" choice on the launch panel into the list of locations
// CreateMachine is asked to use, and the word this claim's location column records. where ""
// means "any". orderable is the shape's live orderable list, already confirmed non-empty by the
// caller (startLaunch refuses an empty one).
//
// A refusal names exactly what went wrong, in place of an empty answer walked past silently: a
// name not in orderable, or "any-eu" or "any-us" when nothing in orderable is in that set.
// providerKey only matters for "any": RunPod's create takes an empty `dataCenterIds` to mean
// "anywhere" ([DOC] docs.runpod.io/api-reference-v2/pods/create-a-pod), but Verda's create
// requires one `location_code` always (openapi.json, `required`), so "any" on a Verda row still
// names the first orderable one rather than an empty list Verda would refuse. "any-eu" and
// "any-us" match provider.IsEULocation and provider.IsUSLocation.
func ResolveLocations(where, providerKey string, orderable []string) (locations []string, claimLocation string, refusal string) {
	if where == "" {
		where = "any"
	}
	switch where {
	case "any":
		if providerKey == "verda" {
			return orderable[:1], "any", ""
		}
		return nil, "any", ""
	case "any-eu":
		eu := filterLocations(orderable, provider.IsEULocation)
		if len(eu) == 0 {
			return nil, "", "no orderable location for this shape is in the EU, Iceland or Norway."
		}
		return eu, "any-eu", ""
	case "any-us":
		us := filterLocations(orderable, provider.IsUSLocation)
		if len(us) == 0 {
			return nil, "", "no orderable location for this shape is in the US."
		}
		return us, "any-us", ""
	default:
		for _, loc := range orderable {
			if loc == where {
				return []string{where}, where, ""
			}
		}
		return nil, "", "'" + where + "' is not an orderable location for this shape."
	}
}

// filterLocations answers the subset of locations matching predicate, in the order they appear.
func filterLocations(locations []string, predicate func(string) bool) []string {
	var out []string
	for _, loc := range locations {
		if predicate(loc) {
			out = append(out, loc)
		}
	}
	return out
}

// whereClause turns a resolved claimLocation into the phrase the launch log line and the accepted
// response's detail sentence name the place with: "" for "any" (nothing is named yet; the
// provider answers with what it chose, see SettleClaimCreated).
func whereClause(claimLocation string) string {
	switch claimLocation {
	case "any":
		return ""
	case "any-eu":
		return " in the EU, Iceland or Norway"
	case "any-us":
		return " in the US"
	default:
		return " in " + claimLocation
	}
}

// handleLaunch answers POST /api/launch: the call the row's Launch button sends after the
// confirmation. It rents ONE machine and nothing else. It parses the query, then startLaunch
// re-derives every figure live before anything is asked of a provider; nothing the page sent is
// trusted.
func (s *Server) handleLaunch(w http.ResponseWriter, r *http.Request) {
	providerKey := r.URL.Query().Get("provider")
	shapeCode := r.URL.Query().Get("shape_code")
	if providerKey == "" || shapeCode == "" {
		writeRefusal(w, http.StatusBadRequest, "provider and shape_code are both required.")
		return
	}
	// Required, and refused by name before any claim is written, like every other missing or
	// invalid launch input.
	project := r.URL.Query().Get("project")
	if project == "" {
		writeRefusal(w, http.StatusBadRequest, "project is required.")
		return
	}
	if exists, err := s.store.ProjectExists(project); err != nil {
		s.log.Add("console", "could not check whether project "+project+" exists: "+err.Error())
		writeRefusal(w, http.StatusInternalServerError, "Could not check that project.")
		return
	} else if !exists {
		writeRefusal(w, http.StatusBadRequest, "'"+project+"' is not a project this console knows.")
		return
	}
	ctxThousands, chats, detail := parseFitFigures(r)
	if detail != "" {
		writeRefusal(w, http.StatusBadRequest, detail)
		return
	}
	// The five sampling values, gpu_memory_utilization and max_num_batched_tokens ride alongside
	// the other query parameters; an empty or absent one is nil and sends no flag.
	temperature, detail := parseOptionalFloat(r, "temperature")
	if detail != "" {
		writeRefusal(w, http.StatusBadRequest, detail)
		return
	}
	topK, detail := parseOptionalFloat(r, "top_k")
	if detail != "" {
		writeRefusal(w, http.StatusBadRequest, detail)
		return
	}
	topP, detail := parseOptionalFloat(r, "top_p")
	if detail != "" {
		writeRefusal(w, http.StatusBadRequest, detail)
		return
	}
	minP, detail := parseOptionalFloat(r, "min_p")
	if detail != "" {
		writeRefusal(w, http.StatusBadRequest, detail)
		return
	}
	repeatPenalty, detail := parseOptionalFloat(r, "repeat_penalty")
	if detail != "" {
		writeRefusal(w, http.StatusBadRequest, detail)
		return
	}
	gpuMemUtil, detail := parseOptionalFloat(r, "gpu_memory_utilization")
	if detail != "" {
		writeRefusal(w, http.StatusBadRequest, detail)
		return
	}
	maxBatchedTokens, detail := parseOptionalInt(r, "max_num_batched_tokens")
	if detail != "" {
		writeRefusal(w, http.StatusBadRequest, detail)
		return
	}
	// The launch panel's -b, -ub and --cache-reuse boxes for a GGUF row, per launch and never
	// saved. provider.VLLMServeArgs ignores them, as buildLlamaCppArgs ignores maxBatchedTokens
	// above.
	batchTokens, detail := parseOptionalInt(r, "batch_tokens")
	if detail != "" {
		writeRefusal(w, http.StatusBadRequest, detail)
		return
	}
	microBatchTokens, detail := parseOptionalInt(r, "micro_batch_tokens")
	if detail != "" {
		writeRefusal(w, http.StatusBadRequest, detail)
		return
	}
	cacheReuseTokens, detail := parseOptionalInt(r, "cache_reuse_tokens")
	if detail != "" {
		writeRefusal(w, http.StatusBadRequest, detail)
		return
	}
	// Nil means on. The page's checkbox always sends 1 or 0.
	thinking, detail := parseOptionalBool(r, "thinking")
	if detail != "" {
		writeRefusal(w, http.StatusBadRequest, detail)
		return
	}
	idleMinutes, detail := parseOptionalInt(r, "idle_minutes")
	if detail != "" {
		writeRefusal(w, http.StatusBadRequest, detail)
		return
	}
	modelName := r.URL.Query().Get("model")
	where := r.URL.Query().Get("where")
	if where == "" {
		where = "any"
	}
	out, refusal := s.startLaunch(r.Context(), launchRequest{
		ProviderKey: providerKey, ShapeCode: shapeCode, Project: project, Model: modelName,
		ContextThousands: ctxThousands, Chats: chats, Where: where,
		Temperature: temperature, TopK: topK, TopP: topP, MinP: minP, RepeatPenalty: repeatPenalty,
		GPUMemoryUtilization: gpuMemUtil, MaxNumBatchedTokens: maxBatchedTokens,
		BatchTokens: batchTokens, MicroBatchTokens: microBatchTokens, CacheReuseTokens: cacheReuseTokens,
		Thinking: thinking, IdleMinutes: idleMinutes,
	}, launchOptions{})
	if refusal != nil {
		writeRefusal(w, refusal.status, refusal.detail)
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{
		"claim_id": out.claimID,
		"detail":   out.detail,
	})
}

// launchRequest is everything a launch is asked for, as structured data: what the page sends, and
// what is stored on the claim (machine_claim.launch_request) so the auto-move can launch the same
// thing on a cheaper shape. It holds no credential: the machine's key and the Hugging Face token
// are minted or read fresh for every launch.
type launchRequest struct {
	ProviderKey          string   `json:"provider_key"`
	ShapeCode            string   `json:"shape_code"`
	Project              string   `json:"project"`
	Model                string   `json:"model"`
	ContextThousands     int      `json:"context_thousands"`
	Chats                int      `json:"chats"`
	Where                string   `json:"where"`
	Temperature          *float64 `json:"temperature,omitempty"`
	TopK                 *float64 `json:"top_k,omitempty"`
	TopP                 *float64 `json:"top_p,omitempty"`
	MinP                 *float64 `json:"min_p,omitempty"`
	RepeatPenalty        *float64 `json:"repeat_penalty,omitempty"`
	GPUMemoryUtilization *float64 `json:"gpu_memory_utilization,omitempty"`
	MaxNumBatchedTokens  *int     `json:"max_num_batched_tokens,omitempty"`
	BatchTokens          *int     `json:"batch_tokens,omitempty"`
	MicroBatchTokens     *int     `json:"micro_batch_tokens,omitempty"`
	CacheReuseTokens     *int     `json:"cache_reuse_tokens,omitempty"`
	Thinking             *bool    `json:"thinking,omitempty"`
	// IdleMinutes is the machine's idle timeout, 0 meaning never; nil means the default.
	IdleMinutes *int `json:"idle_minutes,omitempty"`
}

// launchRefusal is why startLaunch asked nothing of a provider, with the HTTP status handleLaunch
// answers it with.
type launchRefusal struct {
	status int
	detail string
}

// launchOutcome is an accepted launch: the claim written before the provider is asked.
type launchOutcome struct {
	claimID int64
	detail  string
}

// launchOptions are the two things only the auto-move adds to a launch. belowPrice, when set,
// refuses the launch unless the shape's live price is strictly below it. beforeCreate runs after
// the claim row is committed and before the provider is asked to create anything, with the new
// claim id and the live price; when it fails nothing is asked and the claim is settled refused.
type launchOptions struct {
	belowPrice   *float64
	beforeCreate func(claimID int64, price *float64) error
}

// startLaunch is the whole launch, shared by the page's Launch button (handleLaunch) and the
// auto-move: it re-derives every figure live, runs every refusal, commits the claim and starts
// runLaunch. Nothing the caller sent is trusted: the shape, its price and its locations are read
// from the provider again, here.
func (s *Server) startLaunch(ctx context.Context, req launchRequest, opts launchOptions) (launchOutcome, *launchRefusal) {
	providerKey, shapeCode, project := req.ProviderKey, req.ShapeCode, req.Project
	ctxThousands, chats := req.ContextThousands, req.Chats
	temperature, topK, topP, minP, repeatPenalty := req.Temperature, req.TopK, req.TopP, req.MinP, req.RepeatPenalty
	gpuMemUtil, maxBatchedTokens := req.GPUMemoryUtilization, req.MaxNumBatchedTokens
	batchTokens, microBatchTokens, cacheReuseTokens := req.BatchTokens, req.MicroBatchTokens, req.CacheReuseTokens
	thinking := req.Thinking
	idleMinutes := store.DefaultIdleMinutes
	if req.IdleMinutes != nil {
		idleMinutes = *req.IdleMinutes
	}
	refuse := func(status int, detail string) (launchOutcome, *launchRefusal) {
		return launchOutcome{}, &launchRefusal{status: status, detail: detail}
	}
	if idleMinutes < 0 {
		return refuse(http.StatusBadRequest, "idle_minutes must be zero (never) or more.")
	}
	// vLLM refuses to start when max_num_batched_tokens < max_num_seqs
	// (docs/providers.md#model-servers), so a typed figure below the chat count is refused here,
	// before any claim is written, rather than reaching the provider and failing in the machine's
	// log.
	if maxBatchedTokens != nil && *maxBatchedTokens < chats {
		return refuse(http.StatusBadRequest,
			"max_num_batched_tokens ("+strconv.Itoa(*maxBatchedTokens)+
				") must be at least the chat count ("+strconv.Itoa(chats)+").")
	}
	modelName := req.Model
	if modelName == "" {
		return refuse(http.StatusBadRequest, "A model must be chosen above the table before anything can be launched.")
	}
	model, detail := s.modelForFit(modelName)
	if detail != "" {
		return refuse(http.StatusBadRequest, detail)
	}
	catRow, haveCatRow := s.byName[modelName]
	if !haveCatRow {
		// modelForFit above already validated modelName against the same map, so this does not
		// happen; it is handled defensively.
		return refuse(http.StatusBadRequest, "'"+modelName+"' is not a model this console's catalog carries.")
	}
	target := provider.ByKey(s.providers, providerKey)
	if target == nil {
		return refuse(http.StatusBadRequest, "'"+providerKey+"' is not a provider this console knows.")
	}
	if !target.CredentialsConfigured() {
		return refuse(http.StatusBadRequest, target.DisplayName()+" has no credential configured, so it cannot be asked to create anything.")
	}
	// Refused by name, never a silent fallback, and by the same call the table's fit column
	// uses (judge), so the page and this refusal cannot disagree.
	if serves, reason := target.ServesFormat(catRow.Format); !serves {
		return refuse(http.StatusBadRequest, "'"+modelName+"' cannot be served on "+target.DisplayName()+": "+reason)
	}
	var hfFile string
	if catRow.HFFile != nil {
		hfFile = *catRow.HFFile
	}
	if catRow.Format == "gguf" && hfFile == "" {
		// Refused by name before any claim is written, never a silent fallback to a guessed
		// quant tag. A GGUF row without one is caught here rather than failing inside the
		// machine.
		return refuse(http.StatusBadRequest, "'"+modelName+"' has no hf_file in the catalog, so it cannot be served.")
	}
	// A model's published context is its serving ceiling, and a launch above it would only fail
	// after the rental began, inside vLLM's startup check. It is checked here, before any
	// provider is asked, and only when the catalog carries a context_length at all. Nothing here
	// picks a smaller context on the user's behalf; the refusal says what to change.
	if catRow.ContextLength != nil {
		requestedTokens := fit.ContextTokensFromThousands(ctxThousands)
		if requestedTokens > *catRow.ContextLength {
			return refuse(http.StatusBadRequest,
				"'"+modelName+"' publishes a maximum context of "+strconv.Itoa(*catRow.ContextLength)+
					" tokens; "+strconv.Itoa(requestedTokens)+" tokens was requested. Choose a smaller context.")
		}
	}

	ticked, err := s.store.IsShapeTicked(shapeCode)
	if err != nil {
		s.log.Add("console", "could not read the shape tick for "+shapeCode+": "+err.Error())
		return refuse(http.StatusInternalServerError, "Could not check whether this shape may be rented.")
	}
	if !ticked {
		return refuse(http.StatusBadRequest, "'"+shapeCode+"' is not ticked, so it may not be rented.")
	}

	// Re-derive the shape from a fresh, live answer (it costs nothing) rather than trusting the
	// price or the availability the page sent.
	callCtx, cancel := context.WithTimeout(ctx, providerCallTimeout)
	shapes, err := target.ListShapes(callCtx)
	cancel()
	if err != nil {
		return refuse(http.StatusBadGateway, target.DisplayName()+" could not be asked what it offers right now: "+err.Error())
	}
	var shape *provider.Shape
	for i := range shapes {
		if shapes[i].Code == shapeCode {
			shape = &shapes[i]
			break
		}
	}
	if shape == nil {
		return refuse(http.StatusBadRequest, target.DisplayName()+" does not currently offer '"+shapeCode+"'.")
	}
	if len(shape.OrderableLocations) == 0 {
		return refuse(http.StatusBadRequest, target.DisplayName()+" has nowhere to order '"+shapeCode+"' right now.")
	}
	// The mover only: a machine is rented for a move only while its live price is still strictly
	// below the machine it replaces, read from this fresh answer and not from the earlier look.
	if opts.belowPrice != nil && (shape.OnDemandPricePerHour == nil || *shape.OnDemandPricePerHour >= *opts.belowPrice) {
		return refuse(http.StatusBadRequest, "'"+shapeCode+"' is no longer cheaper than the machine it would replace.")
	}
	// The price cap of this launch's own project, set under Projects. A shape whose price the
	// provider did not name cannot be shown to be under it, so it is refused too. A project that
	// cannot be read refuses; it is never read as "no cap".
	priceCap, err := s.store.ProjectPriceCap(project)
	if err != nil {
		s.log.Add("console", "could not read the price cap of project '"+project+"': "+err.Error())
		return refuse(http.StatusInternalServerError, "Could not check the price cap of project '"+project+"'. Nothing was asked of "+target.DisplayName()+".")
	}
	if priceCap != nil {
		if shape.OnDemandPricePerHour == nil {
			return refuse(http.StatusBadRequest, "'"+shapeCode+"' has no price from "+target.DisplayName()+", so it cannot be shown to be under the price cap of "+priceCapText(*priceCap)+" for project '"+project+"'.")
		}
		if *shape.OnDemandPricePerHour > *priceCap {
			return refuse(http.StatusBadRequest, "'"+shapeCode+"' costs "+priceCapText(*shape.OnDemandPricePerHour)+", over the price cap of "+priceCapText(*priceCap)+" for project '"+project+"'.")
		}
	}
	plan := fit.Plan{ContextTokens: fit.ContextTokensFromThousands(ctxThousands), ConcurrentChats: chats}
	verdict := s.judge(model, plan, *shape)
	if verdict == nil || verdict.Verdict != string(fit.Launchable) {
		reason := "it does not fit at these figures."
		if verdict != nil {
			reason = verdict.Reason
		}
		return refuse(http.StatusBadRequest, "'"+shapeCode+"' cannot serve '"+model.Name+"': "+reason)
	}

	// Where is the user's choice on the launch panel, never this program's guess. Missing means
	// "any". A name not in the shape's live orderable list, or an "any-eu" or "any-us" this shape
	// has nothing in, is refused by name before anything is asked of the provider.
	locations, claimLocation, whereRefusal := ResolveLocations(req.Where, providerKey, shape.OrderableLocations)
	if whereRefusal != "" {
		return refuse(http.StatusBadRequest, whereRefusal)
	}
	hostname := "gpugate-" + strconv.FormatInt(time.Now().UnixNano(), 36)

	// Every launch names a model (checked above), so every provider is asked to come up already
	// serving it: RunPod through its verified vLLM template or the pinned llama.cpp image, Verda
	// through vLLM's official image. One random key per launch is minted here before the claim is
	// written, never a provider's template default derived from the machine's id. It travels to
	// CreateMachine on serve.APIKey and is never put in the args string or the claim record
	// below; it is written to its own file once the create answer names the machine id
	// (runLaunch).
	podKey, err := podkey.Generate()
	if err != nil {
		s.log.Add("console", "could not mint a key for "+shapeCode+": "+err.Error())
		return refuse(http.StatusInternalServerError, "Could not mint a key for this launch. Nothing was asked of "+target.DisplayName()+".")
	}
	serve := &provider.ServeOptions{
		ModelName:            catRow.Name,
		HFRepo:               catRow.HFRepo,
		Format:               catRow.Format,
		ShapeCode:            shapeCode,
		HFFile:               hfFile,
		ReasoningParser:      catRow.ReasoningParser,
		ToolCallParser:       catRow.ToolCallParser,
		ContextTokens:        fit.ContextTokensFromThousands(ctxThousands),
		ConcurrentChats:      chats,
		DiskBytes:            catRow.DiskBytes,
		GPUCount:             shape.GPUCount,
		APIKey:               podKey,
		HFToken:              s.hfToken,
		Temperature:          temperature,
		TopK:                 topK,
		TopP:                 topP,
		MinP:                 minP,
		RepeatPenalty:        repeatPenalty,
		GPUMemoryUtilization: gpuMemUtil,
		MaxNumBatchedTokens:  maxBatchedTokens,
		BatchTokens:          batchTokens,
		MicroBatchTokens:     microBatchTokens,
		CacheReuseTokens:     cacheReuseTokens,
		Thinking:             thinking,
		TrustRemoteCode:      catRow.TrustRemoteCode,
	}
	if serve.HFToken == "" {
		// An empty token never refuses a launch: the download proceeds unauthenticated. It earns
		// one line naming no value, since it is a cause of a slow download
		// (docs/providers.md#hugging-face).
		s.log.Add("console", "no Hugging Face token configured; this machine will download unauthenticated")
	}
	claimModelName := catRow.Name
	// What the create will be asked to do is read from the provider itself (for Verda this
	// includes its GET /v1/images choice) and written into the claim before anything is asked
	// for.
	desc, err := target.DescribeServe(ctx, *serve)
	if err != nil {
		return refuse(http.StatusBadGateway, target.DisplayName()+" could not be asked what it will serve: "+err.Error())
	}
	claimImageOrTemplate := desc.ImageOrTemplate
	claimServeArgs := desc.Args
	// The NAMES of the environment variables the create will send, never their values: one of
	// them is this machine's key.
	claimServeEnvKeys := strings.Join(desc.EnvKeys, ",")
	// Verda only: the host image DescribeServe just chose, copied onto serve so CreateMachine
	// sends exactly what the claim recorded. "" on RunPod, unread.
	serve.HostImage = desc.HostImage

	// The launch-time key match, Verda only (target implements sshKeyLister): a machine created
	// with ssh_key_ids naming a key nobody here can sign with is a machine nobody can log into,
	// billing for the readiness hour. Refused by name before any claim is written.
	if lister, ok := target.(sshKeyLister); ok {
		if s.sshKey == nil {
			return refuse(http.StatusBadRequest, "no usable SSH key at "+sshtunnel.PrivateKeyPath)
		}
		lines, err := lister.ListSSHPublicKeys(ctx)
		if err != nil {
			return refuse(http.StatusBadGateway, target.DisplayName()+" could not be asked which SSH keys the account holds: "+err.Error())
		}
		if !s.sshKey.IsRegistered(lines) {
			return refuse(http.StatusBadRequest, "the key at "+sshtunnel.PrivateKeyPath+" is not one of the account's registered SSH keys")
		}
	}

	// Written down and committed before the provider is asked to create anything. If this cannot
	// be written, nothing is asked. The record names the model, the image or template, the full
	// args, and the names of the environment variables the create will send, so it says what was
	// asked for, not only what machine.
	requestJSON, err := json.Marshal(req)
	if err != nil {
		s.log.Add("console", "could not encode the launch request for "+shapeCode+": "+err.Error())
		return refuse(http.StatusInternalServerError, "Could not record the rental. Nothing was asked of "+target.DisplayName()+".")
	}
	claimID, err := s.store.RecordClaim(providerKey, shapeCode, claimLocation, hostname, project, shape.OnDemandPricePerHour, claimModelName, claimImageOrTemplate, claimServeArgs, claimServeEnvKeys, string(requestJSON), idleMinutes)
	if err != nil {
		s.log.Add("console", "could not record the claim for "+shapeCode+": "+err.Error())
		return refuse(http.StatusInternalServerError, "Could not record the rental. Nothing was asked of "+target.DisplayName()+".")
	}
	if opts.beforeCreate != nil {
		if err := opts.beforeCreate(claimID, shape.OnDemandPricePerHour); err != nil {
			s.log.Add("console", "could not record the move for claim "+strconv.FormatInt(claimID, 10)+"; nothing was asked of "+target.DisplayName()+": "+err.Error())
			if werr := s.store.SettleClaimRefused(claimID, "the move could not be recorded"); werr != nil {
				s.log.Add("console", "could not settle claim "+strconv.FormatInt(claimID, 10)+" as refused: "+werr.Error())
			}
			return refuse(http.StatusInternalServerError, "Could not record the move. Nothing was asked of "+target.DisplayName()+".")
		}
	}

	priceText := "price unknown"
	if shape.OnDemandPricePerHour != nil {
		priceText = fmt.Sprintf("$%.2f/hr", *shape.OnDemandPricePerHour)
	}
	detailMsg := "Asked " + target.DisplayName() + " to create " + shapeCode + whereClause(claimLocation) + " at " + priceText + " for project '" + project + "'."
	detailMsg += " It will be asked to serve " + claimModelName + "."
	detailMsg += " Still waiting for a fresh answer to confirm it."
	s.log.Add("launch", s.rentalPrefix(claimID)+detailMsg+" ("+hostname+")")

	// The create call itself is never cancelled once asked, because an unanswered create may
	// already have produced a billing machine. It runs on its own context, detached from this
	// request, like handleDestroy's goroutine.
	go s.runLaunch(target, claimID, provider.CreateOptions{ShapeCode: shapeCode, Locations: locations, Hostname: hostname, Serve: serve})

	return launchOutcome{claimID: claimID, detail: detailMsg}, nil
}

// priceCapText writes a dollars-per-hour figure the way the cap is shown to the user.
func priceCapText(usd float64) string {
	return "$" + strconv.FormatFloat(usd, 'f', -1, 64) + "/hr"
}

// runLaunch is the goroutine startLaunch starts: it asks target to create the machine opts
// describes and settles claimID with whatever the provider said. The whole job is this one create
// call, because the machine comes up already serving; if this process dies instead,
// reconcileClaims, run from the live poll, finishes it.
func (s *Server) runLaunch(target provider.Provider, claimID int64, opts provider.CreateOptions) {
	who := s.rentalPrefix(claimID)
	// Verda only (target implements startupScripter): the startup script is created and its id
	// recorded on the claim before the instance is asked for. A script that cannot be created,
	// or cannot be recorded, refuses the claim here and stops; nothing was asked of Verda's
	// instance create yet.
	if scripter, ok := target.(startupScripter); ok {
		scriptCtx, scriptCancel := context.WithTimeout(context.Background(), providerCallTimeout)
		scriptID, scriptErr := scripter.CreateStartupScript(scriptCtx, opts)
		scriptCancel()
		if scriptErr != nil {
			s.log.Add("launch", who+target.DisplayName()+" refused to create the startup script for "+opts.ShapeCode+" ("+opts.Hostname+"): "+scriptErr.Error())
			if werr := s.store.SettleClaimRefused(claimID, "could not create the startup script: "+scriptErr.Error()); werr != nil {
				s.log.Add("console", "could not settle claim "+strconv.FormatInt(claimID, 10)+" as refused: "+werr.Error())
			}
			return
		}
		if werr := s.store.SetClaimStartupScript(claimID, scriptID); werr != nil {
			s.log.Add("console", "could not record the startup script "+scriptID+" for claim "+strconv.FormatInt(claimID, 10)+": "+werr.Error())
			// An unrecorded script is deleted and the claim refused, rather than letting a script
			// nobody can find a record of be sent to a real create.
			delCtx, delCancel := context.WithTimeout(context.Background(), providerCallTimeout)
			if derr := scripter.DeleteStartupScript(delCtx, scriptID); derr != nil {
				s.log.Add("launch", who+target.DisplayName()+" could not delete the unrecorded startup script "+scriptID+": "+derr.Error())
			}
			delCancel()
			if werr := s.store.SettleClaimRefused(claimID, "could not record the startup script"); werr != nil {
				s.log.Add("console", "could not settle claim "+strconv.FormatInt(claimID, 10)+" as refused: "+werr.Error())
			}
			return
		}
		opts.Serve.StartupScriptID = scriptID
	}

	ctx, cancel := context.WithTimeout(context.Background(), providerCallTimeout)
	defer cancel()
	machine, err := target.CreateMachine(ctx, opts)
	if machine.ProviderID != "" {
		// The provider made a machine, whether or not reading it back afterward also succeeded
		// (what was actually rented is what gets recorded). machine.Location is the data centre
		// the provider's answer named, never opts.Locations, which is only what was asked for
		// and may name several or none.
		if werr := s.store.SettleClaimCreated(claimID, machine.ProviderID, machine.Location); werr != nil {
			s.log.Add("console", "could not settle claim "+strconv.FormatInt(claimID, 10)+" as created: "+werr.Error())
		}
		if err != nil {
			s.log.Add("launch", who+target.DisplayName()+" created "+opts.ShapeCode+" ("+machine.ProviderID+") but answered oddly afterward: "+err.Error())
		} else {
			locationText := machine.Location
			if locationText == "" {
				locationText = "a location it did not name"
			}
			s.log.Add("launch", who+target.DisplayName()+" created "+opts.ShapeCode+" as "+machine.ProviderID+" ("+opts.Hostname+") in "+locationText+"; it is starting up and is not serving yet.")
		}

		// A machine asked to serve a model gets its key written to disk the instant the provider
		// names its id, before the claim is marked confirmed, so every later reader (the
		// readiness poll, the fixed address) can find it. A machine whose key cannot be written
		// is one nobody can ever reach, so it is destroyed at once rather than left billing,
		// without asking the user first.
		if werr := podkey.Write(target.Key(), machine.ProviderID, opts.Serve.APIKey); werr != nil {
			s.log.Add("console", "could not write the pod key for "+machine.ProviderID+": "+werr.Error())
			s.destroyUnreachablePod(target, machine.ProviderID, opts.ShapeCode+" ("+opts.Hostname+")", "has no key on file, so nothing could ever reach it")
		}

		// A fresh poll, not this call's own success, is what confirms it; ask for one now rather
		// than waiting for the next tick, so the strip catches up promptly.
		s.pollRunningOnce(context.Background())
		return
	}
	if errors.Is(err, provider.ErrCreateOutcomeUnknown) {
		// Never settle this as a refusal: the machine may exist and may be billing. The claim
		// stays in 'asked', so reconcileClaims matches it by hostname the moment a fresh answer
		// lists it, and abandons it after claimAbandonAfter if none ever does.
		s.log.Add("launch", who+target.DisplayName()+" was asked to create "+opts.ShapeCode+" ("+opts.Hostname+"); it gave no clear success and no clear refusal, so nothing is settled yet. Still waiting for a fresh answer to say what really happened: "+err.Error())
		s.pollRunningOnce(context.Background())
		return
	}
	if err != nil {
		s.log.Add("launch", who+target.DisplayName()+" refused to create "+opts.ShapeCode+" ("+opts.Hostname+"): "+readableRefusal(err.Error())+".")
		if werr := s.store.SettleClaimRefused(claimID, err.Error()); werr != nil {
			s.log.Add("console", "could not settle claim "+strconv.FormatInt(claimID, 10)+" as refused: "+werr.Error())
		}
		// A create refusal means nothing will read the script, so it is deleted at once.
		s.deleteStartupScriptForClaim(target.Key(), claimID)
		return
	}
	// No id and no error should not happen; it is treated as a refusal so the claim never stays
	// open forever waiting on a machine that was never created.
	s.log.Add("launch", who+target.DisplayName()+" answered the create for "+opts.ShapeCode+" with neither a machine id nor an error, so it is treated as refused.")
	if werr := s.store.SettleClaimRefused(claimID, "the provider's answer carried neither a machine id nor an error"); werr != nil {
		s.log.Add("console", "could not settle claim "+strconv.FormatInt(claimID, 10)+" as refused: "+werr.Error())
	}
	s.deleteStartupScriptForClaim(target.Key(), claimID)
}
