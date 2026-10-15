package provider

import (
	"encoding/json"
	"math"
	"sort"
	"strconv"
	"strings"
)

// VLLMServeArgs turns one ServeOptions into the single `args` string `vllm serve` takes: the
// model repository, the address to bind, the console's figures, then the catalog's optional
// parsers. RunPod's template and Verda's docker run line take the identical string.
//
// --enforce-eager is never sent, and --trust-remote-code only for a catalog row that asks for it.
//
// The two batched-token flags scale with the context and chat-count boxes rather than using a
// fixed pair, because a fixed pair fails small contexts outright ([DOC] vLLM's
// verify_max_model_len). T = min(8192, ContextTokens) never exceeds the model length; B =
// max(3*T, ConcurrentChats) never falls under the chat count, and is used only when
// o.MaxNumBatchedTokens is nil.
//
// o.GPUMemoryUtilization and the five sampling fields are per-model settings: an empty box on
// the page is nil here and contributes nothing.
func VLLMServeArgs(o ServeOptions) string {
	prefillThreshold := o.ContextTokens
	if prefillThreshold > 8192 {
		prefillThreshold = 8192
	}
	batchedTokens := 3 * prefillThreshold
	if o.ConcurrentChats > batchedTokens {
		batchedTokens = o.ConcurrentChats
	}
	if o.MaxNumBatchedTokens != nil {
		batchedTokens = *o.MaxNumBatchedTokens
	}
	gpuMemUtil := 0.95
	if o.GPUMemoryUtilization != nil {
		gpuMemUtil = *o.GPUMemoryUtilization
	}
	parts := []string{
		o.HFRepo,
		"--host", "0.0.0.0", "--port", "8000",
		"--dtype", "auto", "--gpu-memory-utilization", FormatFlagNumber(gpuMemUtil),
		"--max-model-len", strconv.Itoa(o.ContextTokens),
		"--max-num-seqs", strconv.Itoa(o.ConcurrentChats),
		"--long-prefill-token-threshold", strconv.Itoa(prefillThreshold),
		"--max-num-batched-tokens", strconv.Itoa(batchedTokens),
		// Without this flag vLLM never populates prompt_tokens_details, so cached tokens appear in
		// no answer on either endpoint and the Sessions view would show 0 cached tokens for every
		// rental.
		"--enable-prompt-tokens-details",
	}
	if o.GPUCount > 1 {
		// Every card, or the weights are loaded onto one and the machine never serves. See
		// ServeOptions.GPUCount.
		parts = append(parts, "--tensor-parallel-size", strconv.Itoa(o.GPUCount))
		// GOTCHA: two-card machines can hang after warm-up, printing only vLLM's "No available
		// shared memory broadcast block" each minute. The only basis for this flag is one upstream
		// report (vllm-project/vllm#45198, same symptom, workaround --disable-custom-all-reduce).
		// The cause here is unproven and the decode-speed cost is unmeasured
		// (docs/providers.md#multi-card-machines).
		parts = append(parts, "--disable-custom-all-reduce")
	}
	if sampling := overrideGenerationConfigJSON(o); sampling != "" {
		parts = append(parts, "--override-generation-config", "'"+sampling+"'")
	}
	parts = append(parts, "--default-chat-template-kwargs", "'"+thinkingChatTemplateKwargsJSON(o.Thinking)+"'")
	if o.ReasoningParser != "" {
		parts = append(parts, "--reasoning-parser", o.ReasoningParser)
	}
	if o.ToolCallParser != "" {
		// A parser alone is not enough: without --enable-auto-tool-choice vLLM refuses every
		// request that sends tool_choice "auto", which is what a coding harness sends.
		parts = append(parts, "--enable-auto-tool-choice", "--tool-call-parser", o.ToolCallParser)
	}
	if o.TrustRemoteCode {
		// A permission to run whatever Python the catalog row's repository ships, sent only for a
		// row that asks for it. The machine that runs it holds only its own random key (o.APIKey,
		// VLLMServeEnv), never an account credential.
		parts = append(parts, "--trust-remote-code")
	}
	return strings.Join(parts, " ")
}

// FormatFlagNumber prints v the way a command-line flag reads best: a whole number with no
// trailing ".0".
func FormatFlagNumber(v float64) string {
	if v == math.Trunc(v) {
		return strconv.FormatFloat(v, 'f', 0, 64)
	}
	return strconv.FormatFloat(v, 'f', -1, 64)
}

// thinkingChatTemplateKwargsJSON is the --default-chat-template-kwargs value sent on every vLLM
// launch, a per-model choice: thinking nil or true sends both keys true, false sends both
// false.
func thinkingChatTemplateKwargsJSON(thinking *bool) string {
	v := thinking == nil || *thinking
	raw, err := json.Marshal(map[string]bool{"enable_thinking": v, "thinking": v})
	if err != nil {
		// Unreachable: a map[string]bool of two literal keys always marshals.
		if v {
			return `{"enable_thinking":true,"thinking":true}`
		}
		return `{"enable_thinking":false,"thinking":false}`
	}
	return string(raw)
}

// overrideGenerationConfigJSON builds the --override-generation-config value: one JSON object
// carrying only the sampling fields o sets, with repeat_penalty renamed repetition_penalty. ""
// means none of the five was set.
func overrideGenerationConfigJSON(o ServeOptions) string {
	values := map[string]json.Number{}
	set := func(key string, v *float64) {
		if v != nil {
			values[key] = json.Number(FormatFlagNumber(*v))
		}
	}
	set("temperature", o.Temperature)
	set("top_k", o.TopK)
	set("top_p", o.TopP)
	set("min_p", o.MinP)
	set("repetition_penalty", o.RepeatPenalty)
	if len(values) == 0 {
		return ""
	}
	raw, err := json.Marshal(values)
	if err != nil {
		// Unreachable: a map[string]json.Number of already-formatted numbers always marshals.
		return ""
	}
	return string(raw)
}

// VLLMServeEnv is the `env` a serving vLLM create sends: this machine's key, plus the two NCCL
// settings a multi-card machine needs. RunPod merges it over its template's env per key; Verda's
// startup script writes it to an env file (verda.buildStartupScript). Both read this one
// function, so the values cannot drift apart.
//
// A single-card machine gets neither NCCL setting. HF_TOKEN is sent only when o.HFToken is
// non-empty: an empty token means an unauthenticated download, never a refused launch.
func VLLMServeEnv(o ServeOptions) map[string]string {
	env := map[string]string{"VLLM_API_KEY": o.APIKey}
	if o.HFToken != "" {
		env["HF_TOKEN"] = o.HFToken
	}
	if o.GPUCount > 1 {
		// GOTCHA: a two-card machine can stall silently right after both workers print the NCCL
		// version, and at default logging nothing says which step it stopped in. [DOC] NCCL's
		// environment-variable docs: NCCL_DEBUG=INFO prints each rank's topology and chosen
		// transport during init. NCCL_P2P_DISABLE=1 is vLLM's documented workaround when NCCL's
		// sanity check hangs (docs/providers.md#multi-card-machines). Cost: cross-card traffic goes
		// the slower way, unmeasured ([UNKNOWN]).
		env["NCCL_DEBUG"] = "INFO"
		env["NCCL_P2P_DISABLE"] = "1"
	}
	return env
}

// VLLMServeEnvKeys names VLLMServeEnv's keys, sorted, and never a value: the claim record and
// anything reading it can say what was sent without VLLM_API_KEY's value leaving this package.
func VLLMServeEnvKeys(o ServeOptions) []string {
	env := VLLMServeEnv(o)
	keys := make([]string, 0, len(env))
	for k := range env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
