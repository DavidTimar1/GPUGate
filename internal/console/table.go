package console

import (
	"context"
	"net/http"
	"sort"
	"strconv"

	"github.com/DavidTimar1/gpugate/internal/fit"
	"github.com/DavidTimar1/gpugate/internal/provider"
)

func (s *Server) handleShapePermission(w http.ResponseWriter, r *http.Request) {
	shapeCode := r.PathValue("shape_code")
	if shapeCode == "" {
		writeRefusal(w, http.StatusBadRequest, "shape_code is required.")
		return
	}
	checkedRaw := r.URL.Query().Get("checked")
	var checked bool
	switch checkedRaw {
	case "true":
		checked = true
	case "false":
		checked = false
	default:
		writeRefusal(w, http.StatusBadRequest, "checked must be the string true or false.")
		return
	}
	if err := s.store.SetShapePermission(shapeCode, checked); err != nil {
		s.log.Add("console", "could not write the shape permission for "+shapeCode+": "+err.Error())
		writeRefusal(w, http.StatusInternalServerError, "Could not change that permission.")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"shape_code":      shapeCode,
		"allow_on_demand": checked,
	})
}

// modelFitJSON is one shape's answer for the one model chosen in the dropdown: whether it can
// serve it, and what is left over, or why not in a few words. ui/src/app.js's fitCell() reads
// model_name, verdict and reason.
type modelFitJSON struct {
	ModelName string `json:"model_name"`
	Verdict   string `json:"verdict"`
	Reason    string `json:"reason"`
}

// shapeJSON is one row of the machine table, as ui/src/app.js's renderTable() reads it.
// CardArchitecture, CardYear, CardPerGPUVRAMGB, CardComputeCapability and CardFP8 are the row's
// card facts: nil wherever the source is nil, and the page reads a nil as "unknown".
// CardPerGPUVRAMGB is the PROVIDER's figure divided back down to one card, never a vendor number.
// It is computed beside this struct's literal rather than carried on provider.Shape, because it
// is arithmetic on two fields the struct already carries.
type shapeJSON struct {
	ProviderKey           string        `json:"provider_key"`
	ShapeCode             string        `json:"shape_code"`
	DisplayName           string        `json:"display_name"`
	GPUName               string        `json:"gpu_name"`
	GPUCount              int           `json:"gpu_count"`
	CardArchitecture      *string       `json:"card_architecture"`
	CardYear              *int          `json:"card_year"`
	CardPerGPUVRAMGB      *int          `json:"card_per_gpu_vram_gb"`
	CardComputeCapability *int          `json:"card_compute_capability"`
	CardFP8               *bool         `json:"card_fp8"`
	WholeMachineVRAMGB    int           `json:"whole_machine_vram_gb"`
	OnDemandPricePerHour  *float64      `json:"on_demand_price_per_hour"`
	OrderableLocations    []string      `json:"orderable_locations"`
	IsTicked              bool          `json:"is_ticked"`
	ModelFit              *modelFitJSON `json:"model_fit"`
}

// perGPUVRAMGB is the provider's whole-machine figure divided back down to one card: nil when
// GPUCount is not a positive count, which never happens on a real shape but is checked anyway
// rather than risking a divide by zero.
func perGPUVRAMGB(wholeMachineVRAMGB, gpuCount int) *int {
	if gpuCount <= 0 {
		return nil
	}
	v := wholeMachineVRAMGB / gpuCount
	return &v
}

// modelForFit resolves the chosen model's arithmetic figures from the catalog. An empty name
// means no model is chosen yet and answers (nil, nil, ""). A name the catalog does not carry is
// refused: this program never guesses which model was meant.
func (s *Server) modelForFit(name string) (*fit.Model, string) {
	if name == "" {
		return nil, ""
	}
	row, ok := s.byName[name]
	if !ok {
		return nil, "'" + name + "' is not a model this console's catalog carries."
	}
	m := fit.ModelFromCatalog(row.Name, row.Format, row.SizeGB, row.DiskBytes, row.KVCacheBytesPerToken, row.MinComputeCapability)
	return &m, ""
}

// judge answers one provider shape for one chosen model at one plan, or nil when no model is
// chosen. Whether shape's provider can serve the model's format is judged first, before any
// memory arithmetic, so a model the provider cannot serve reads cannot_serve_format with that
// reason rather than a memory figure that was never the problem.
func (s *Server) judge(model *fit.Model, plan fit.Plan, shape provider.Shape) *modelFitJSON {
	if model == nil {
		return nil
	}
	if p := provider.ByKey(s.providers, shape.ProviderKey); p != nil {
		if serves, reason := p.ServesFormat(model.Format); !serves {
			return &modelFitJSON{ModelName: model.Name, Verdict: string(fit.CannotServeFormat), Reason: reason}
		}
		if serves, reason := p.ServesShape(shape.Code); !serves {
			return &modelFitJSON{ModelName: model.Name, Verdict: string(fit.CannotServeShape), Reason: reason}
		}
	}
	machineShape := fit.MachineShape{
		GPUCount:           shape.GPUCount,
		WholeMachineVRAMGB: shape.WholeMachineVRAMGB,
		ComputeCapability:  shape.ComputeCapability,
	}
	answer := fit.FitsWholeMachineVRAM(*model, machineShape, plan)
	return &modelFitJSON{ModelName: model.Name, Verdict: string(answer.Verdict), Reason: answer.Reason}
}

// modelOptionJSON is one row of the dropdown's full list: the model's name, its format
// (catalog.Model.Format verbatim: "gguf", "safetensors", "fp8"), and whether it carries a
// recurrent state the fit arithmetic does not size (catalog.Model.IsHybrid).
type modelOptionJSON struct {
	Name     string `json:"name"`
	Format   string `json:"format"`
	IsHybrid bool   `json:"is_hybrid"`
	// TrustRemoteCode mirrors the catalog row's field (internal/catalog): the launch panel's
	// confirmation sentence reads it to say the machine will run the repository's own code
	// (ui/src/app.js, launchServingText).
	TrustRemoteCode bool `json:"trust_remote_code"`
}

// modelOptions answers every model in the catalog, in the file's order: the dropdown's full list,
// each marked with its format and whether it is hybrid.
func (s *Server) modelOptions() []modelOptionJSON {
	out := make([]modelOptionJSON, 0, len(s.models))
	for _, m := range s.models {
		out = append(out, modelOptionJSON{Name: m.Name, Format: m.Format, IsHybrid: m.IsHybrid(), TrustRemoteCode: m.TrustRemoteCode})
	}
	return out
}

// handleMachinesAndModels answers GET /api/machines: it asks every provider with a configured
// credential for its shapes and its live on-demand availability, judges every shape against the
// chosen model, and answers the whole table. A provider that could not be asked is reported in
// availability_problems, never silently walked past, and its shapes are absent rather than shown
// stale.
func (s *Server) handleMachinesAndModels(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeRefusal(w, http.StatusMethodNotAllowed, "GET is the only method this call answers.")
		return
	}
	ctxThousands, chats, detail := parseFitFigures(r)
	if detail != "" {
		writeRefusal(w, http.StatusBadRequest, detail)
		return
	}
	model, detail := s.modelForFit(r.URL.Query().Get("model"))
	if detail != "" {
		writeRefusal(w, http.StatusBadRequest, detail)
		return
	}
	plan := fit.Plan{ContextTokens: fit.ContextTokensFromThousands(ctxThousands), ConcurrentChats: chats}

	var allShapes []provider.Shape
	var problems []string
	attempted := 0
	failed := 0
	for _, p := range s.providers {
		if !p.CredentialsConfigured() {
			problems = append(problems, p.DisplayName()+": no credential configured, so it was not asked.")
			continue
		}
		attempted++
		callCtx, cancel := context.WithTimeout(r.Context(), providerCallTimeout)
		shapes, err := p.ListShapes(callCtx)
		cancel()
		if err != nil {
			failed++
			s.log.Add("providers", p.DisplayName()+" did not answer the catalog refresh: "+err.Error())
			problems = append(problems, p.DisplayName()+" could not be asked: "+err.Error())
			continue
		}
		s.log.Add("providers", "refreshed "+p.DisplayName()+": "+strconv.Itoa(len(shapes))+" shape(s)")
		allShapes = append(allShapes, shapes...)
	}
	availabilityKnown := attempted > 0 && failed == 0

	ticked, err := s.store.TickedShapeCodes()
	if err != nil {
		s.log.Add("console", "could not read the shape ticks: "+err.Error())
		writeRefusal(w, http.StatusInternalServerError, "Could not read which shapes are ticked.")
		return
	}

	sortByPrice(allShapes)

	out := make([]shapeJSON, 0, len(allShapes))
	for _, shape := range allShapes {
		out = append(out, shapeJSON{
			ProviderKey:           shape.ProviderKey,
			ShapeCode:             shape.Code,
			DisplayName:           shape.DisplayName,
			GPUName:               shape.GPUName,
			GPUCount:              shape.GPUCount,
			CardArchitecture:      shape.CardArchitecture,
			CardYear:              shape.CardYear,
			CardPerGPUVRAMGB:      perGPUVRAMGB(shape.WholeMachineVRAMGB, shape.GPUCount),
			CardComputeCapability: shape.ComputeCapability,
			CardFP8:               shape.CardFP8,
			WholeMachineVRAMGB:    shape.WholeMachineVRAMGB,
			OnDemandPricePerHour:  shape.OnDemandPricePerHour,
			OrderableLocations:    emptyIfNil(shape.OrderableLocations),
			IsTicked:              ticked[shape.Code],
			ModelFit:              s.judge(model, plan, shape),
		})
	}

	s.cacheMu.Lock()
	s.cachedFrom = allShapes
	s.haveCache = true
	s.cacheMu.Unlock()

	writeJSON(w, http.StatusOK, map[string]any{
		"shapes":                out,
		"availability_known":    availabilityKnown,
		"availability_problems": emptyIfNil(problems),
		"models":                s.modelOptions(),
	})
}

// recalcShapeJSON is one row of GET /api/models's answer.
type recalcShapeJSON struct {
	ShapeCode string        `json:"shape_code"`
	ModelFit  *modelFitJSON `json:"model_fit"`
}

// handleRecalculate answers GET /api/models: it asks no provider anything, and re-judges the
// shapes already read by the last successful Refresh against the figures now in the boxes.
func (s *Server) handleRecalculate(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeRefusal(w, http.StatusMethodNotAllowed, "GET is the only method this call answers.")
		return
	}
	ctxThousands, chats, detail := parseFitFigures(r)
	if detail != "" {
		writeRefusal(w, http.StatusBadRequest, detail)
		return
	}
	model, detail := s.modelForFit(r.URL.Query().Get("model"))
	if detail != "" {
		writeRefusal(w, http.StatusBadRequest, detail)
		return
	}
	plan := fit.Plan{ContextTokens: fit.ContextTokensFromThousands(ctxThousands), ConcurrentChats: chats}

	s.cacheMu.Lock()
	shapes := s.cachedFrom
	have := s.haveCache
	s.cacheMu.Unlock()
	if !have {
		writeRefusal(w, http.StatusBadRequest, "Press Refresh first; there is nothing to recalculate yet.")
		return
	}

	out := make([]recalcShapeJSON, 0, len(shapes))
	for _, shape := range shapes {
		out = append(out, recalcShapeJSON{ShapeCode: shape.Code, ModelFit: s.judge(model, plan, shape)})
	}
	writeJSON(w, http.StatusOK, map[string]any{"shapes": out})
}

// sortByPrice orders shapes cheapest first, the table's default opening sort, so a table answered
// before the page applies its own sort still reads sensibly. Shapes with no price sort last,
// never scattered.
func sortByPrice(shapes []provider.Shape) {
	sort.SliceStable(shapes, func(i, j int) bool {
		pi, pj := shapes[i].OnDemandPricePerHour, shapes[j].OnDemandPricePerHour
		if pi == nil && pj == nil {
			return false
		}
		if pi == nil {
			return false
		}
		if pj == nil {
			return true
		}
		return *pi < *pj
	})
}

func emptyIfNil(v []string) []string {
	if v == nil {
		return []string{}
	}
	return v
}
