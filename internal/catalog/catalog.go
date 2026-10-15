// Package catalog loads catalog/models.json, the models this console offers in its dropdown, and
// holds nothing else. It never edits that file. The file also carries reference columns Go does
// not read (attention_layers, kv_heads, head_dim, state_bytes_per_layer, weight_bits,
// quant_method, context_size, architecture_basis, notes): they are the figures and sources the
// numbers in the read columns come from.
package catalog

import (
	"encoding/json"
	"fmt"
	"os"
)

// Model is one row of catalog/models.json, holding only the fields the fit arithmetic and the
// dropdown need. A column not named here is ignored by json.Unmarshal, never an error.
type Model struct {
	Name   string `json:"name"`
	HFRepo string `json:"hf_repo"`
	// HFFile is the exact GGUF file inside HFRepo a llama.cpp create names with `-hff`, nil for
	// a non-GGUF row. A GGUF row carries exactly one non-empty value here.
	HFFile               *string `json:"hf_file"`
	Format               string  `json:"format"`
	SizeGB               float64 `json:"size_gb"`
	DiskBytes            *int64  `json:"disk_bytes"`
	KVCacheBytesPerToken *int    `json:"kv_cache_bytes_per_token"`
	MinComputeCapability *int    `json:"min_compute_capability"`
	// ContextLength is the model's own published maximum context, in raw tokens, nil when the
	// catalog carries none. Shown as the "model" line under the context box; never read by the
	// fit arithmetic.
	ContextLength *int `json:"context_length"`
	// ReasoningParser and ToolCallParser are vLLM's own flag values for this model, empty when
	// the catalog carries none. Only the launch path reads them, to build the serving command.
	ReasoningParser string `json:"reasoning_parser"`
	ToolCallParser  string `json:"tool_call_parser"`
	// StateLayers is how many of the model's layers carry a per-conversation recurrent state
	// (a hybrid attention and state-space architecture) rather than only a per-token KV cache.
	// `internal/fit` does not size that state, so its sum is slightly optimistic for such a row,
	// roughly 0.4 to 1.5 GiB at ten chats. Such rows are marked rather than the arithmetic
	// reworked.
	StateLayers int `json:"state_layers"`
	// Temperature, TopK, TopP, MinP and RepeatPenalty are the model publisher's own recommended
	// sampling values; nil means the catalog carries none for this model. This is the fallback
	// layer only: the user's own saved value (`internal/store`'s `model_setting` table) always
	// wins over it, and neither wins over an empty box, which sends no flag at all.
	Temperature   *float64 `json:"temperature"`
	TopK          *float64 `json:"top_k"`
	TopP          *float64 `json:"top_p"`
	MinP          *float64 `json:"min_p"`
	RepeatPenalty *float64 `json:"repeat_penalty"`
	// TrustRemoteCode is true for a row whose weights need vLLM's --trust-remote-code to load:
	// the machine then runs the model repository's own Python. False when a row does not set it.
	TrustRemoteCode bool `json:"trust_remote_code"`
}

// IsHybrid reports whether this model carries a recurrent state the fit arithmetic does not
// size (see StateLayers).
func (m Model) IsHybrid() bool {
	return m.StateLayers > 0
}

// Load reads every model in path, in the order the file states them.
func Load(path string) ([]Model, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	var models []Model
	if err := json.Unmarshal(raw, &models); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	return models, nil
}
