package console

import (
	"encoding/json"
	"net/http"
	"strconv"

	"github.com/DavidTimar1/gpugate/internal/catalog"
	"github.com/DavidTimar1/gpugate/internal/store"
)

// modelSettingFieldJSON is one key's answer on GET /api/models/{name}/settings: the user's saved
// value, the model's published value, the model server's default as fixed text, and whether this
// key belongs to this model's server at all. Saved and Model are nil when that source carries
// none.
type modelSettingFieldJSON struct {
	Saved         *float64 `json:"saved"`
	Model         *float64 `json:"model"`
	EngineDefault string   `json:"engine_default"`
	Applies       bool     `json:"applies"`
}

// modelSettingApplies answers whether key is ever read for a model published in format: every
// key applies to both model servers except gpu_memory_utilization, which vLLM alone reads. format
// "gguf" means llama.cpp serves it; anything else (safetensors, fp8) means vLLM does.
func modelSettingApplies(key, format string) bool {
	if key == "gpu_memory_utilization" {
		return format != "gguf"
	}
	return true
}

// modelValueForKey answers key's "model" value from catRow: the five sampling fields read
// straight from the catalog, context_thousands from the catalog's ContextLength (in raw tokens,
// the unit the settings line names it in, never converted to the box's thousands unit), and nil
// for chats and gpu_memory_utilization, which the catalog carries no figure for.
func modelValueForKey(key string, catRow catalog.Model) *float64 {
	switch key {
	case "temperature":
		return catRow.Temperature
	case "top_k":
		return catRow.TopK
	case "top_p":
		return catRow.TopP
	case "min_p":
		return catRow.MinP
	case "repeat_penalty":
		return catRow.RepeatPenalty
	case "context_thousands":
		if catRow.ContextLength == nil {
			return nil
		}
		v := float64(*catRow.ContextLength)
		return &v
	default:
		return nil
	}
}

// engineDefaultText answers key's fixed "engine default" sentence for a model published in
// format: what each model server does with this key when nothing was typed into its box. It is
// literal text, never a live computation: none of these figures depend on anything this program
// tracks per launch.
func engineDefaultText(key, format string) string {
	isGGUF := format == "gguf"
	switch key {
	case "temperature":
		if isGGUF {
			return "0.80, engine default; llama.cpp does not read the model's generation config"
		}
		return "uses the model's value; fallback 1.0 when none is published"
	case "top_k":
		if isGGUF {
			return "40, engine default; llama.cpp does not read the model's generation config"
		}
		return "uses the model's value; fallback 0 (off) when none is published"
	case "top_p":
		if isGGUF {
			return "0.95, engine default; llama.cpp does not read the model's generation config"
		}
		return "uses the model's value; fallback 1.0 when none is published"
	case "min_p":
		if isGGUF {
			return "0.05, engine default; llama.cpp does not read the model's generation config"
		}
		return "uses the model's value; fallback 0 when none is published"
	case "repeat_penalty":
		if isGGUF {
			return "1.00, engine default; llama.cpp does not read the model's generation config"
		}
		return "uses the model's value; fallback 1.0 when none is published"
	case "gpu_memory_utilization":
		return "engine default 0.92; this program sends 0.95 when empty"
	case "context_thousands":
		if isGGUF {
			return "from the model when 0"
		}
		return "the model's own maximum"
	case "chats":
		if isGGUF {
			return "auto"
		}
		return "engine default depends on the card"
	case "thinking":
		// Every launch sends thinking on unless this setting says otherwise, for both model
		// servers.
		return "on"
	default:
		return ""
	}
}

// handleModelSettingsGet answers GET /api/models/{name}/settings: for each of
// store.ModelSettingKeys, the user's saved value, the model's published value, the model
// server's fixed default text, and whether the key applies to this model's server. An unknown
// model name answers 404.
func (s *Server) handleModelSettingsGet(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	catRow, ok := s.byName[name]
	if !ok {
		writeRefusal(w, http.StatusNotFound, "'"+name+"' is not a model this console's catalog carries.")
		return
	}
	saved, err := s.store.ModelSettings(name)
	if err != nil {
		s.log.Add("console", "could not read the saved settings for "+name+": "+err.Error())
		writeRefusal(w, http.StatusInternalServerError, "Could not read the saved settings for '"+name+"'.")
		return
	}
	settings := make(map[string]modelSettingFieldJSON, len(store.ModelSettingKeys))
	for _, key := range store.ModelSettingKeys {
		field := modelSettingFieldJSON{
			Model:         modelValueForKey(key, catRow),
			EngineDefault: engineDefaultText(key, catRow.Format),
			Applies:       modelSettingApplies(key, catRow.Format),
		}
		if raw, ok := saved[key]; ok {
			if v, err := strconv.ParseFloat(raw, 64); err == nil {
				field.Saved = &v
			}
		}
		settings[key] = field
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"model_name": name,
		"settings":   settings,
	})
}

// handleModelSettingsPut answers PUT /api/models/{name}/settings: the body is a JSON object of
// setting name to value, one of store.ModelSettingKeys per key. An empty or absent value deletes
// that key's saved row; a present, non-empty value that does not parse as a number is refused
// with 400 and the reason, before anything is written. An unknown model name answers 404.
func (s *Server) handleModelSettingsPut(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	catRow, ok := s.byName[name]
	if !ok {
		writeRefusal(w, http.StatusNotFound, "'"+name+"' is not a model this console's catalog carries.")
		return
	}
	var body map[string]string
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeRefusal(w, http.StatusBadRequest, "The request body must be a JSON object of setting name to value.")
		return
	}
	values := make(map[string]string, len(store.ModelSettingKeys))
	for _, key := range store.ModelSettingKeys {
		raw, present := body[key]
		if !present || raw == "" {
			values[key] = ""
			continue
		}
		if !modelSettingApplies(key, catRow.Format) {
			writeRefusal(w, http.StatusBadRequest, "'"+key+"' does not apply to '"+name+"' ("+catRow.Format+").")
			return
		}
		v, err := strconv.ParseFloat(raw, 64)
		if err != nil {
			writeRefusal(w, http.StatusBadRequest, "'"+key+"' must be a number, got '"+raw+"'.")
			return
		}
		// A saved thinking value other than 0 or 1 is refused with the same 400 wording a
		// non-numeric value gets.
		if key == "thinking" && v != 0 && v != 1 {
			writeRefusal(w, http.StatusBadRequest, "'"+key+"' must be a number, got '"+raw+"'.")
			return
		}
		values[key] = raw
	}
	if err := s.store.SaveModelSettings(name, values); err != nil {
		s.log.Add("console", "could not save the settings for "+name+": "+err.Error())
		writeRefusal(w, http.StatusInternalServerError, "Could not save the settings for '"+name+"'.")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"model_name": name,
		"detail":     "Saved '" + name + "'s settings.",
	})
}
