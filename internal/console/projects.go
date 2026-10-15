package console

import (
	"database/sql"
	"encoding/json"
	"html"
	"math"
	"net/http"
	"strconv"
	"strings"

	"github.com/DavidTimar1/gpugate/internal/store"
)

// The project list an outside tool reads before it points a client at GPUGate.
//
// The shape is fixed because outside tools already parse it. They fetch `GET /api/projects`, keep
// every project with `enabled === true`, a `slug`, a `base_url` and at least one entry in
// `models`, append their own suffix ("" or "/v1") to `base_url`, and refuse a `base_url` that is
// not under the address they asked. So `base_url` is built from the request's own host, never a
// fixed one, and carries no version segment. `context_tokens` is read per model from `backends`
// by `served_model_name`. Every project this console knows is listed, 'coding' first, then the
// rest by `created_at`.

type projectBackendAnswer struct {
	ServedModelName string `json:"served_model_name"`
	Model           string `json:"model"`
	// BackendID equals InstanceID. It stays because an outside client may read backend_id.
	BackendID       string `json:"backend_id"`
	InstanceID      string `json:"instance_id"`
	ContextTokens   int    `json:"context_tokens"`
	ConcurrentChats int    `json:"concurrent_chats"`
	// EngineVersion is what the model server reported at readiness: "vllm <version>" or
	// "llama.cpp <build_info>", "" when it could not be read. Outside readers read named fields
	// only (see the header comment) and ignore any field they do not know.
	EngineVersion string `json:"engine_version"`
}

type projectAnswer struct {
	Slug string `json:"slug"`
	Name string `json:"name"`
	// Enabled is always true. It stays because outside tools keep only projects with
	// `enabled === true`.
	Enabled  bool                   `json:"enabled"`
	AutoMove bool                   `json:"auto_move"`
	BaseURL  string                 `json:"base_url"`
	Models   []string               `json:"models"`
	Backends []projectBackendAnswer `json:"backends"`
}

// handleProjects answers GET /api/projects. A project serving nothing is still listed, with no
// models: a visible absence, never a project that has vanished. What is "serving" comes from the
// claims the readiness poll marked serving, each gated on a fresh provider answer that still
// carries its machine (servingPods).
func (s *Server) handleProjects(w http.ResponseWriter, r *http.Request) {
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	projects, err := s.store.ListProjects()
	if err != nil {
		s.log.Add("console", "could not read the projects for /api/projects: "+err.Error())
		writeRefusal(w, http.StatusInternalServerError, "Could not read the projects.")
		return
	}
	out := make([]projectAnswer, 0, len(projects))
	for _, p := range projects {
		answer, err := s.projectAnswerFor(p, scheme+"://"+r.Host)
		if err != nil {
			s.log.Add("console", "could not read what '"+p.Slug+"' is serving for /api/projects: "+err.Error())
			writeRefusal(w, http.StatusInternalServerError, "Could not read what is serving.")
			return
		}
		out = append(out, answer)
	}
	priceCap, err := s.store.PriceCap()
	if err != nil {
		s.log.Add("console", "could not read the price cap for /api/projects: "+err.Error())
		writeRefusal(w, http.StatusInternalServerError, "Could not read the price cap.")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"projects": out, "price_cap_per_hour": priceCap})
}

// projectAnswerFor builds one project's /api/projects row: its base address under /p/<slug>, and
// only its own serving machines, never another project's. Enabled is always true and each
// backend's BackendID equals its InstanceID; both are kept because outside tools read them.
func (s *Server) projectAnswerFor(p store.Project, origin string) (projectAnswer, error) {
	answer := projectAnswer{
		Slug:     p.Slug,
		Name:     p.Name,
		Enabled:  true,
		AutoMove: p.AutoMove,
		BaseURL:  origin + "/p/" + p.Slug,
		Models:   []string{},
		Backends: []projectBackendAnswer{},
	}
	// Every machine serving right now, not just the newest: two can serve at once, and the tool
	// reading this list picks between them by naming the model (handleProjectProxy).
	pods, err := s.servingPods(p.Slug, s.lastKnownMachines())
	if err != nil {
		return answer, err
	}
	listed := map[string]bool{}
	for _, pod := range pods {
		catRow := s.byName[pod.claim.ModelName]
		// llama.cpp's -np is the chat count directly, like vLLM's --max-num-seqs; its -c is the
		// whole pool shared by every slot (buildLlamaCppArgs), so the per-chat figure this endpoint
		// reports is -c divided by -np, the meaning vLLM's --max-model-len already has.
		chats := intFlag(pod.claim.ServeArgs, "--max-num-seqs", "-np")
		contextTokens := intFlag(pod.claim.ServeArgs, "--max-model-len", "-c")
		if catRow.Format == "gguf" && chats > 0 {
			contextTokens = contextTokens / chats
		}
		// `models` carries a name once, because it is the menu a tool offers; `backends` carries a
		// row per machine, so two machines serving the same name are both visible there.
		if !listed[pod.servedModel] {
			listed[pod.servedModel] = true
			answer.Models = append(answer.Models, pod.servedModel)
		}
		answer.Backends = append(answer.Backends, projectBackendAnswer{
			ServedModelName: pod.servedModel,
			Model:           pod.claim.ModelName,
			BackendID:       pod.providerID,
			InstanceID:      pod.providerID,
			ContextTokens:   contextTokens,
			ConcurrentChats: chats,
			EngineVersion:   pod.claim.EngineVersion,
		})
	}
	return answer, nil
}

// intFlag reads the whole number after the first of names that appears in a serving args string,
// or 0. names carries the alternate spellings the two model servers use for the same setting:
// vLLM's "--max-model-len" and "--max-num-seqs", llama.cpp's "-c" and "-np".
func intFlag(args string, names ...string) int {
	parts := strings.Fields(args)
	for i, p := range parts {
		for _, name := range names {
			if p == name && i+1 < len(parts) {
				if n, err := strconv.Atoi(parts[i+1]); err == nil {
					return n
				}
			}
		}
	}
	return 0
}

// writeProjectRefusalFragment answers an htmx-swappable HTML fragment naming what went wrong, for
// the "New project" form's hx-target. It is not the JSON shape writeRefusal answers elsewhere,
// because htmx swaps this call's body straight into the page.
func writeProjectRefusalFragment(w http.ResponseWriter, status int, detail string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(`<p class="refusal-text">` + html.EscapeString(detail) + `</p>`))
}

// handleCreateProject answers POST /api/projects: the "New project" form's htmx submit, form
// fields `slug` and `name`. A slug outside the shape, or one already taken, is refused by name
// rather than silently ignored or coerced.
func (s *Server) handleCreateProject(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		writeProjectRefusalFragment(w, http.StatusBadRequest, "Could not read the form.")
		return
	}
	slug := strings.TrimSpace(r.FormValue("slug"))
	name := strings.TrimSpace(r.FormValue("name"))
	if slug == "" || name == "" {
		writeProjectRefusalFragment(w, http.StatusBadRequest, "A slug and a name are both required.")
		return
	}
	if !store.ValidProjectSlug(slug) {
		writeProjectRefusalFragment(w, http.StatusBadRequest,
			"'"+slug+"' is not a valid slug: 1 to 32 lowercase letters, digits and dashes, starting with a letter.")
		return
	}
	if err := s.store.CreateProject(slug, name); err != nil {
		if err == store.ErrProjectExists {
			writeProjectRefusalFragment(w, http.StatusBadRequest, "A project named '"+slug+"' already exists.")
			return
		}
		s.log.Add("console", "could not create project "+slug+": "+err.Error())
		writeProjectRefusalFragment(w, http.StatusInternalServerError, "Could not create that project.")
		return
	}
	s.log.Add("console", "created project '"+slug+"' ('"+name+"').")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(`<p>Created '` + html.EscapeString(slug) + `'.</p>`))
}

// handleDeleteProject answers DELETE /api/projects/{slug}. 'coding' can never be deleted; any
// other project can be deleted only while none of its machines is actually loading or serving
// right now, and a refusal names the machine rather than silently orphaning a running rental.
// "Actually" matters: projectActivePod judges this by a fresh live-poll answer like every other
// live-machine check, never by a claim's serving_state alone, which stays 'serving' or 'loading'
// forever once its machine is destroyed. A deleted project's past claims are untouched:
// DeleteProject only removes the project row.
func (s *Server) handleDeleteProject(w http.ResponseWriter, r *http.Request) {
	slug := r.PathValue("slug")
	if slug == store.CodingProjectSlug {
		writeRefusal(w, http.StatusBadRequest, "'coding' can never be deleted.")
		return
	}
	exists, err := s.store.ProjectExists(slug)
	if err != nil {
		s.log.Add("console", "could not check whether project "+slug+" exists: "+err.Error())
		writeRefusal(w, http.StatusInternalServerError, "Could not check that project.")
		return
	}
	if !exists {
		writeRefusal(w, http.StatusNotFound, "'"+slug+"' is not a project this console knows.")
		return
	}
	hostname, modelName, active, err := s.projectActivePod(slug)
	if err != nil {
		s.log.Add("console", "could not check project "+slug+" for a live pod: "+err.Error())
		writeRefusal(w, http.StatusInternalServerError, "Could not check whether that project is still serving something.")
		return
	}
	if active {
		writeRefusal(w, http.StatusBadRequest, "'"+slug+"' still has "+hostname+" loading or serving "+modelName+"; destroy it first.")
		return
	}
	if err := s.store.DeleteProject(slug); err != nil {
		s.log.Add("console", "could not delete project "+slug+": "+err.Error())
		writeRefusal(w, http.StatusInternalServerError, "Could not delete that project.")
		return
	}
	s.log.Add("console", "deleted project '"+slug+"'.")
	writeJSON(w, http.StatusOK, map[string]any{"slug": slug, "detail": "Deleted '" + slug + "'."})
}

// handleProjectAutoMove answers POST /api/projects/{slug}/auto-move?on=true|false: the checkbox in
// the Projects view. It only moves the switch and, when switching on, asks for a look at once;
// whether anything is rented because of it is the mover's decision (mover.go), and a move already
// under way finishes either way.
func (s *Server) handleProjectAutoMove(w http.ResponseWriter, r *http.Request) {
	slug := r.PathValue("slug")
	var on bool
	switch r.URL.Query().Get("on") {
	case "true":
		on = true
	case "false":
		on = false
	default:
		writeRefusal(w, http.StatusBadRequest, "on must be the string true or false.")
		return
	}
	if err := s.store.SetProjectAutoMove(slug, on); err != nil {
		if err == sql.ErrNoRows {
			writeRefusal(w, http.StatusNotFound, "'"+slug+"' is not a project this console knows.")
			return
		}
		s.log.Add("console", "could not change auto-move for project "+slug+": "+err.Error())
		writeRefusal(w, http.StatusInternalServerError, "Could not change that setting.")
		return
	}
	state := "off"
	if on {
		state = "on"
	}
	s.log.Add("console", "auto-move to a cheaper machine is "+state+" for project '"+slug+"'.")
	if on {
		s.lookForCheaperNow()
	}
	writeJSON(w, http.StatusOK, map[string]any{"slug": slug, "auto_move": on})
}

// handlePriceCap answers POST /api/settings/price-cap?usd=<dollars per hour>: the one hourly price
// ceiling the Projects view sets. An empty usd clears it (no cap). It applies to the next launch
// and the next look of the auto-move; a machine already running is not touched by it.
func (s *Server) handlePriceCap(w http.ResponseWriter, r *http.Request) {
	raw := strings.TrimSpace(r.URL.Query().Get("usd"))
	if raw == "" {
		if err := s.store.SetPriceCap(nil); err != nil {
			s.log.Add("console", "could not clear the price cap: "+err.Error())
			writeRefusal(w, http.StatusInternalServerError, "Could not clear the price cap.")
			return
		}
		s.log.Add("console", "the price cap is cleared: no hourly ceiling.")
		writeJSON(w, http.StatusOK, map[string]any{"price_cap_per_hour": nil})
		return
	}
	usd, err := strconv.ParseFloat(raw, 64)
	if err != nil || math.IsNaN(usd) || math.IsInf(usd, 0) || usd <= 0 {
		writeRefusal(w, http.StatusBadRequest, "The price cap must be a number of dollars per hour above zero, or empty for no cap.")
		return
	}
	if err := s.store.SetPriceCap(&usd); err != nil {
		s.log.Add("console", "could not write the price cap: "+err.Error())
		writeRefusal(w, http.StatusInternalServerError, "Could not save the price cap.")
		return
	}
	s.log.Add("console", "the price cap is "+priceCapText(usd)+".")
	writeJSON(w, http.StatusOK, map[string]any{"price_cap_per_hour": usd})
}
