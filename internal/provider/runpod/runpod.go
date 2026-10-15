// Package runpod is the RunPod GPU cloud behind the provider.Provider interface. Every method
// but CreateMachine and DestroyMachine is read-only.
//
// [DOC] RunPod's REST v2 API, base https://api.runpod.io/v2, over a bearer API key.
// GET /catalog/gpus answers the GPU-type catalog, one entry per GPU model with its per-card VRAM
// (memory) and its per-card SECURE on-demand price (price.secure).
// GET /catalog/gpus?include=AVAILABILITY&product=POD&count=N answers which data centres have
// stock of each GPU type for an order of N cards.
//
// RunPod has no fixed machine shape: a pod is a GPU type plus a card count the caller chooses.
// This package makes one Shape per (GPU type, card count) for counts 1, 2, 4, 8, clipped to that
// type's own maxCount.secure. SECURE cloud only; COMMUNITY is never offered. Every price here is
// the on-demand SECURE figure, and RunPod's REST v2 schemas carry no spot field at all.
package runpod

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/DavidTimar1/gpugate/internal/provider"
)

const (
	baseURL      = "https://api.runpod.io/v2"
	callTimeout  = 30 * time.Second
	providerKey  = "runpod"
	providerName = "RunPod"
)

// cardCounts are the card counts a pod may be ordered at, before clipping to a GPU type's own
// ceiling. [DOC] maxCount.secure is "the largest number of GPUs you can request on a single pod
// of this type": a ceiling, not a stock level.
var cardCounts = []int{1, 2, 4, 8}

// Client is RunPod behind the provider.Provider interface.
type Client struct {
	apiKey     string
	httpClient *http.Client
}

// New builds a RunPod client from the API key value. It makes no network call. logf is where a
// slow or failing outbound call is reported (provider.LogFunc); the client's *http.Transport is
// the one every provider client shares (provider.NewHTTPClient).
func New(apiKey string, logf provider.LogFunc) *Client {
	return &Client{apiKey: apiKey, httpClient: provider.NewHTTPClient(providerName, callTimeout, logf)}
}

func (c *Client) Key() string         { return providerKey }
func (c *Client) DisplayName() string { return providerName }

// CredentialsConfigured reports whether RUNPOD_API_GPU_GATE was non-empty when this client
// was built.
func (c *Client) CredentialsConfigured() bool {
	return c.apiKey != ""
}

// ServesFormat answers whether RunPod can serve a model published in format, through one of the
// two model servers DescribeServe and CreateMachine branch on. safetensors and fp8 go to
// RunPod's verified vLLM template. gguf goes to the pinned llama.cpp image (llamaCppImage), never
// to vLLM. [DOC] vLLM v0.28.0, docs/features/quantization/gguf.md: "GGUF support in vLLM is
// highly experimental and under-optimized"; support "has migrated to OOT vllm-gguf-plugin", an
// optional extra (setup.py:1544) that docker/Dockerfile at that tag does not install, and this
// program never rebuilds an image to add it.
func (c *Client) ServesFormat(format string) (bool, string) {
	switch format {
	case "safetensors", "fp8", "gguf":
		return true, ""
	default:
		return false, "RunPod's serving path does not recognise the '" + format + "' format"
	}
}

// ServesShape answers true for every RunPod shape: none is known to be unservable.
func (c *Client) ServesShape(shapeCode string) (bool, string) {
	return true, ""
}

type gpuType struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Memory *int   `json:"memory"`
	Price  struct {
		Secure *float64 `json:"secure"`
	} `json:"price"`
	MaxCount struct {
		Secure *int `json:"secure"`
	} `json:"maxCount"`
}

type gpuTypesResponse struct {
	Gpus []gpuType `json:"gpus"`
}

// gpuAvailabilityType is one GPU type of the availability read: the data centres RunPod lists
// for it, each with a stock level (HIGH, MEDIUM, LOW or NONE).
type gpuAvailabilityType struct {
	ID          string `json:"id"`
	DataCenters []struct {
		ID           string `json:"id"`
		Availability string `json:"availability"`
	} `json:"dataCenters"`
}

type gpuAvailabilityResponse struct {
	Gpus []gpuAvailabilityType `json:"gpus"`
}

// ListShapes asks RunPod for its GPU-type catalog and, for each card count, its live data-centre
// availability, and builds one Shape per (GPU type, card count) with its orderable locations.
// Costs nothing.
func (c *Client) ListShapes(ctx context.Context) ([]provider.Shape, error) {
	var types gpuTypesResponse
	if err := c.getJSON(ctx, "/catalog/gpus", &types); err != nil {
		return nil, fmt.Errorf("runpod: %w", err)
	}

	// locationsByGpuTypeAndCount: which data centres report stock of a GPU type for an order of
	// that many cards. [DOC] https://docs.runpod.io/api-reference-v2/catalog/list-gpu-types:
	// include=AVAILABILITY with product=POD and count=N answers, per GPU type, its data centres
	// and a level that is not NONE where stock exists; count is "GPU count for availability and
	// lowest-price calculations". [OBS] the data-centre sets narrow as count grows (H100 80GB
	// HBM3: 8, 7, 4, 0 centres at 1, 2, 4, 8). [UNKNOWN] whether count=2 means two free cards in
	// one machine: consistent with it, not proven. Even so the answer is a snapshot, and a create
	// can still be refused. A failed read fails the refresh and is never read as "nothing
	// orderable".
	locationsByGpuTypeAndCount := map[int]map[string][]string{}
	for _, count := range cardCounts {
		var avail gpuAvailabilityResponse
		path := fmt.Sprintf("/catalog/gpus?include=AVAILABILITY&product=POD&cloud=SECURE&count=%d", count)
		if err := c.getJSON(ctx, path, &avail); err != nil {
			return nil, fmt.Errorf("runpod: %w", err)
		}
		byType := map[string][]string{}
		for _, g := range avail.Gpus {
			if g.ID == "" {
				continue
			}
			for _, dc := range g.DataCenters {
				if dc.ID == "" || dc.Availability == "" || dc.Availability == "NONE" {
					continue
				}
				byType[g.ID] = append(byType[g.ID], dc.ID)
			}
		}
		locationsByGpuTypeAndCount[count] = byType
	}

	var shapes []provider.Shape
	for _, t := range types.Gpus {
		if t.ID == "" || t.Memory == nil || *t.Memory <= 0 {
			// A shape with no card memory at all is left out, as Verda's client does.
			continue
		}
		maxCount := 8
		if t.MaxCount.Secure != nil {
			maxCount = *t.MaxCount.Secure
		}
		displayName := t.Name
		if displayName == "" {
			displayName = t.ID
		}
		for _, count := range cardCounts {
			if count > maxCount {
				break
			}
			var price *float64
			if t.Price.Secure != nil {
				p := *t.Price.Secure * float64(count)
				price = &p
			}
			shown := fmt.Sprintf("%s x%d", displayName, count)
			if strings.Contains(displayName, "MIG") {
				// A MIG row is a slice of one physical card, never a card of its own, so the name
				// says so on the page. Whether tensor parallel across slices works is [UNKNOWN].
				shown += " (MIG: slices of one card, not separate cards)"
			}
			facts, isMIG, matched := provider.LookupCardFacts(displayName)
			shape := provider.Shape{
				ProviderKey:          providerKey,
				Code:                 shapeCode(t.ID, count),
				DisplayName:          shown,
				GPUName:              displayName,
				GPUCount:             count,
				WholeMachineVRAMGB:   *t.Memory * count,
				OnDemandPricePerHour: price,
				OrderableLocations:   locationsByGpuTypeAndCount[count][t.ID],
			}
			if matched {
				shape.ComputeCapability = facts.ComputeCapability
				arch := facts.Architecture
				shape.CardArchitecture = &arch
				shape.CardYear = facts.Year
				if !isMIG {
					shape.CardFP8 = facts.FP8()
				}
			}
			shapes = append(shapes, shape)
		}
	}
	return shapes, nil
}

// pod is one row of GET /pods, holding only the fields this program reads. The list answer is
// either a bare list or {"pods": [...]}, and a pod's gpu carries no display name, only the
// catalog's gpu type id.
type pod struct {
	ID           string  `json:"id"`
	Name         string  `json:"name"`
	Status       string  `json:"status"`
	DataCenterID string  `json:"dataCenterId"`
	Image        string  `json:"image"`
	Cost         float64 `json:"cost"`
	Gpu          struct {
		ID    string `json:"id"`
		Count int    `json:"count"`
	} `json:"gpu"`
	// Runtime is nil before a container exists (PROVISIONING or STARTING). [DOC]
	// https://api.runpod.io/v2/openapi.json, GET /v2/pods/{id}: "runtime.uptime", seconds since
	// the container started. Carried into provider.RunningMachine.UptimeSeconds for the readiness
	// wait's status line.
	Runtime *struct {
		Uptime *int `json:"uptime"`
	} `json:"runtime"`
}

// rowToRunningMachine turns one GET /pods or GET /pods/{id} row into the shape this program
// tracks a machine by. A card count RunPod names as zero or less is read as 1: a pod always has
// at least the one card it was created with.
func rowToRunningMachine(row pod) provider.RunningMachine {
	count := row.Gpu.Count
	if count <= 0 {
		count = 1
	}
	price := row.Cost
	var uptime *int
	if row.Runtime != nil {
		uptime = row.Runtime.Uptime
	}
	return provider.RunningMachine{
		ProviderKey:   providerKey,
		ProviderID:    row.ID,
		ShapeCode:     shapeCode(row.Gpu.ID, count),
		DisplayName:   row.Image,
		GPUName:       row.Gpu.ID,
		Hostname:      row.Name,
		Location:      row.DataCenterID,
		PricePerHour:  &price,
		State:         row.Status,
		UptimeSeconds: uptime,
		// RunPod's proxy address is derived from the pod id alone, so it is known the moment the
		// id is, unlike Verda's, which waits on a live `ip`.
		ServingBaseURL: "https://" + row.ID + "-8000.proxy.runpod.net",
	}
}

// podsResponse is the wrapped shape of the pods list: an object carrying a "pods" list. The
// endpoint can also answer a bare list (see pod).
type podsResponse struct {
	Pods []pod `json:"pods"`
}

// ListMachines asks RunPod for every pod this account is running right now. Costs nothing. An
// error means RunPod could not be asked at all, and the caller must report that rather than
// showing nothing running.
func (c *Client) ListMachines(ctx context.Context) ([]provider.RunningMachine, error) {
	raw, err := c.getRaw(ctx, "/pods")
	if err != nil {
		return nil, fmt.Errorf("runpod: list pods: %w", err)
	}
	var rows []pod
	var wrapped podsResponse
	if err := json.Unmarshal(raw, &rows); err != nil {
		if err := json.Unmarshal(raw, &wrapped); err != nil {
			return nil, fmt.Errorf("runpod: list pods: answered with something that is not the pods shape: %w", err)
		}
		rows = wrapped.Pods
	}
	machines := make([]provider.RunningMachine, 0, len(rows))
	for _, row := range rows {
		machines = append(machines, rowToRunningMachine(row))
	}
	return machines, nil
}

// GetMachine asks RunPod directly for one pod by id, bypassing the account-wide list. Costs
// nothing. [OBS] `GET /v2/pods/{id}` answers 200 with the pod's `args`, `gpu`, `dataCenterId` and
// `cost` while it exists. Used only as the tie-breaker before the readiness poll gives up waiting
// on a pod that two consecutive fresh live-poll answers have missed (console.podGoneByDirectRead).
func (c *Client) GetMachine(ctx context.Context, providerID string) (provider.RunningMachine, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+"/pods/"+providerID, nil)
	if err != nil {
		return provider.RunningMachine{}, fmt.Errorf("runpod: build the request for pod %s: %w", providerID, err)
	}
	req.Header.Set("Authorization", "Bearer "+c.apiKey)
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return provider.RunningMachine{}, fmt.Errorf("runpod: could not be reached for pod %s: %w", providerID, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode == http.StatusNotFound {
		return provider.RunningMachine{}, fmt.Errorf("runpod: pod %s: %w", providerID, provider.ErrMachineNotFound)
	}
	if resp.StatusCode >= 400 {
		return provider.RunningMachine{}, fmt.Errorf("runpod: refused pod %s, status %d: %s", providerID, resp.StatusCode, provider.Truncate(raw, 300))
	}
	var row pod
	if err := json.Unmarshal(raw, &row); err != nil {
		return provider.RunningMachine{}, fmt.Errorf("runpod: pod %s answered with something that is not the pod shape: %w", providerID, err)
	}
	return rowToRunningMachine(row), nil
}

// podLogsReadDeadline bounds how long GetLogs waits for tail's backfill to arrive. [DOC]
// https://api.runpod.io/v2/openapi.json, GET /v2/pods/{id}/logs "Streams pod logs as
// Server-Sent Events" and never closes on its own (docs/providers.md#runpod: a plain GET hangs
// forever), so a bounded caller must cut the connection itself rather than wait for an end that
// never comes.
const podLogsReadDeadline = 10 * time.Second

// GetLogs reads the last tail lines (RunPod's default is 100 and its maximum 5000; this program's
// callers default to 200) of providerID's container log through GET /v2/pods/{id}/logs: a
// server-sent-event stream of `data: {"source":...,"line":...,"ts":...}` events ([DOC]
// openapi.json's "logLine" example: `data: {"ts":"...","source":"container","line":"Model
// loaded."}`). It reads only lines starting "data:", decodes each payload's "line" field, and
// stops, returning whatever backfill arrived, once podLogsReadDeadline cuts the connection: the
// stream itself would otherwise keep it open forever waiting for new output. A deadline cutting
// the connection before any response, or mid-read, is not a failure and answers (nil, nil) or a
// partial list with no error; any other failure (RunPod refused, a 404, a malformed line) is.
func (c *Client) GetLogs(ctx context.Context, providerID string, tail int) ([]string, error) {
	if tail <= 0 {
		tail = 200
	}
	ctx, cancel := context.WithTimeout(ctx, podLogsReadDeadline)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+"/pods/"+providerID+"/logs?tail="+strconv.Itoa(tail), nil)
	if err != nil {
		return nil, fmt.Errorf("runpod: build the log request for pod %s: %w", providerID, err)
	}
	req.Header.Set("Authorization", "Bearer "+c.apiKey)
	resp, err := c.httpClient.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			// The deadline cut the connection before any response arrived: the bounded read's
			// normal way to end when the backfill takes too long, never a failure.
			return nil, nil
		}
		return nil, fmt.Errorf("runpod: could not be reached for pod %s's log: %w", providerID, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return nil, fmt.Errorf("runpod: pod %s: %w", providerID, provider.ErrMachineNotFound)
	}
	if resp.StatusCode >= 400 {
		raw, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("runpod: refused pod %s's log, status %d: %s", providerID, resp.StatusCode, provider.Truncate(raw, 300))
	}
	var lines []string
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for scanner.Scan() {
		text := scanner.Text()
		if !strings.HasPrefix(text, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(text, "data:"))
		if payload == "" {
			continue
		}
		var ev struct {
			Line string `json:"line"`
		}
		if err := json.Unmarshal([]byte(payload), &ev); err != nil {
			continue
		}
		lines = append(lines, ev.Line)
	}
	if err := scanner.Err(); err != nil && ctx.Err() == nil {
		return lines, fmt.Errorf("runpod: read pod %s's log: %w", providerID, err)
	}
	return lines, nil
}

// runpodServeTemplateID is RunPod's verified vLLM template. The template's `env` sets
// `VLLM_API_KEY=sk-<pod id>`, derivable from the pod's public URL; the create overrides it with
// its own `env`, one random key per machine. [DOC]
// `docs.runpod.io/api-reference-v2/pods/create-a-pod`, `templateId`: body fields override the
// template's, except `env`, which is merged per key with body values winning, so sending only
// `VLLM_API_KEY` here replaces the template's key and keeps its `HF_HOME`.
const runpodServeTemplateID = "pvcdqlwm9r"

// llamaCppImage is the pinned llama.cpp server image a GGUF create names as its `image`, never a
// `templateId`: there is no RunPod template for llama.cpp, and a community template is never
// used. [OBS] `gh attestation verify
// oci://ghcr.io/ggml-org/llama.cpp@sha256:e61f29b37c471f956a772f91f4e9952d29f237e5d1a1a748e14421aae090305f
// --owner ggml-org` exited 0 when this tag was pinned. Bump this pin only after the same command
// exits 0 against the new digest.
const llamaCppImage = "ghcr.io/ggml-org/llama.cpp:server-cuda-b10818"

// createGpuConfig is the two fields of GpuConfig this program sets: `id`, the GPU type identifier
// GET /catalog/gpus answers, and `count`, the card count. [DOC]
// https://api.runpod.io/v2/openapi.json, GpuConfig.
type createGpuConfig struct {
	ID    string `json:"id"`
	Count int    `json:"count"`
}

// createServingPodRequest is the body `POST /v2/pods` takes for a vLLM create. It names RunPod's
// verified vLLM template (runpodServeTemplateID) and carries `args`, `ports` and
// `mounts.persistent` so the template's model server starts serving as the pod comes up. [DOC]
// https://api.runpod.io/v2/openapi.json, CreatePodRequest. `cloud` is SECURE because this
// program is secure cloud only, and there is no startSsh: the container is the whole machine.
type createServingPodRequest struct {
	Name          string          `json:"name"`
	TemplateID    string          `json:"templateId"`
	Gpu           createGpuConfig `json:"gpu"`
	Cloud         string          `json:"cloud"`
	Ports         []string        `json:"ports"`
	Disk          int             `json:"disk"`
	Mounts        *podMounts      `json:"mounts,omitempty"`
	DataCenterIDs []string        `json:"dataCenterIds,omitempty"`
	Args          string          `json:"args"`
	// Env carries this machine's random VLLM_API_KEY and, on a multi-card machine, the two NCCL
	// settings provider.VLLMServeEnv sets, merged by RunPod's create endpoint over the template's
	// per key (see runpodServeTemplateID). HF_HOME stays the template's default because it is
	// left out here.
	Env map[string]string `json:"env,omitempty"`
}

// createLlamaCppPodRequest is the body `POST /v2/pods` takes for a GGUF create: like
// createServingPodRequest, but it names the pinned llama.cpp image (llamaCppImage) as `image`
// instead of a `templateId`, because there is no RunPod template for llama.cpp. [DOC]
// docs.runpod.io/api-reference-v2/pods/create-a-pod: "image" is required unless "templateId" is
// set, and both are legal, mutually substitutable fields on this schema.
type createLlamaCppPodRequest struct {
	Name          string          `json:"name"`
	Image         string          `json:"image"`
	Gpu           createGpuConfig `json:"gpu"`
	Cloud         string          `json:"cloud"`
	Ports         []string        `json:"ports"`
	Disk          int             `json:"disk"`
	Mounts        *podMounts      `json:"mounts,omitempty"`
	DataCenterIDs []string        `json:"dataCenterIds,omitempty"`
	Args          string          `json:"args"`
	// Env carries LLAMA_API_KEY and LLAMA_CACHE (buildLlamaCppEnv). Nothing is merged over a
	// template's default here, because there is no template on this create.
	Env map[string]string `json:"env,omitempty"`
}

// podMounts and persistentMount are `mounts.persistent` on a serving create: the volume the
// model's weights are downloaded onto, sized well past the template's 25 GB default.
type podMounts struct {
	Persistent *persistentMount `json:"persistent,omitempty"`
}

type persistentMount struct {
	Size int    `json:"size"`
	Path string `json:"path"`
}

// buildLlamaCppArgs turns one provider.ServeOptions into the `args` string a GGUF create sends
// to the pinned llama.cpp image. [DOC] `tools/server/README.md` at tag b10818:
//   - `-hf <hf_repo> -hff <hf_file>` names the exact weights file, never a guessed quant tag
//     (lines 105-106).
//   - `--alias <hf_repo>` sets the model `id` /v1/models answers (line 1253) to o.HFRepo, so
//     console.modelReady's exact-match check against catRow.HFRepo holds.
//   - `-c <ContextTokens*ConcurrentChats> -np <ConcurrentChats> --no-kv-unified` together: `-c`
//     is the whole pool shared by every slot (line 50), so it is sized for every chat at once,
//     which makes the per-chat figure mean what vLLM's --max-model-len means; `--no-kv-unified`
//     stops one long conversation occupying the whole shared pool and starving the others
//     (llama.cpp's default in `common/arg.cpp`).
//   - `-ngl -1` loads every layer onto the GPU (line 87, default auto).
//   - `--no-webui` (line 211, default enabled) keeps the pod's one public port an API only.
//   - `--reasoning on` or `--reasoning off` (line 235: `[on|off|auto]`), a per-model choice:
//     o.Thinking nil or true sends `on`, false sends `off`, regardless of the model's template
//     keyword or default. What `off` does on a template with no reasoning keyword at all is
//     `[UNKNOWN]`. No `--reasoning-budget`: its default (-1) is already unrestricted, which is
//     the wanted behaviour.
//
// The five sampling values ride as their own bare flags (`--temp --top-k --top-p --min-p
// --repeat-penalty`, line 129 onward), never a JSON blob: llama.cpp has no
// --override-generation-config equivalent. o.ReasoningParser, o.ToolCallParser,
// o.GPUMemoryUtilization and o.MaxNumBatchedTokens are vLLM-only and never read here.
//
// o.BatchTokens, o.MicroBatchTokens and o.CacheReuseTokens are the launch panel's -b, -ub and
// --cache-reuse boxes: nil means the box was empty, so the flag is omitted and llama.cpp's
// compiled-in default applies: `-b, --batch-size N` (default: 2048), `-ub, --ubatch-size N`
// (default: 512) and `--cache-reuse N` (default: 0). [DOC] `tools/server/README.md:52-53,223` at
// tag b10818.
func buildLlamaCppArgs(o provider.ServeOptions) string {
	parts := []string{
		"-hf", o.HFRepo,
		"-hff", o.HFFile,
		"--alias", o.HFRepo,
		"--host", "0.0.0.0", "--port", "8000",
		"-c", strconv.Itoa(o.ContextTokens * o.ConcurrentChats),
		"-np", strconv.Itoa(o.ConcurrentChats),
		"--no-kv-unified",
		"-ngl", "-1",
		"--no-webui",
		"--reasoning", reasoningFlagValue(o.Thinking),
	}
	if o.Temperature != nil {
		parts = append(parts, "--temp", provider.FormatFlagNumber(*o.Temperature))
	}
	if o.TopK != nil {
		parts = append(parts, "--top-k", provider.FormatFlagNumber(*o.TopK))
	}
	if o.TopP != nil {
		parts = append(parts, "--top-p", provider.FormatFlagNumber(*o.TopP))
	}
	if o.MinP != nil {
		parts = append(parts, "--min-p", provider.FormatFlagNumber(*o.MinP))
	}
	if o.RepeatPenalty != nil {
		parts = append(parts, "--repeat-penalty", provider.FormatFlagNumber(*o.RepeatPenalty))
	}
	if o.BatchTokens != nil {
		parts = append(parts, "-b", strconv.Itoa(*o.BatchTokens))
	}
	if o.MicroBatchTokens != nil {
		parts = append(parts, "-ub", strconv.Itoa(*o.MicroBatchTokens))
	}
	if o.CacheReuseTokens != nil {
		parts = append(parts, "--cache-reuse", strconv.Itoa(*o.CacheReuseTokens))
	}
	return strings.Join(parts, " ")
}

// reasoningFlagValue answers llama.cpp's `--reasoning` value for thinking: nil or true means on,
// false means off.
func reasoningFlagValue(thinking *bool) string {
	if thinking != nil && !*thinking {
		return "off"
	}
	return "on"
}

// buildLlamaCppEnv is the `env` a GGUF create sends: this machine's key as LLAMA_API_KEY, the
// env equivalent of --api-key ([DOC] `tools/server/README.md:214-215`: "API key to use for
// authentication... env LLAMA_API_KEY"), sent as an env var rather than a command-line flag so it
// never lands in `args` (`args` is written into `machine_claim.serve_args`); and LLAMA_CACHE
// pointed at the persistent mount (`/workspace`) so the weights `-hf` downloads survive a
// container restart ([DOC] `tools/server/README.md:1664-1671`: "Model sources... 1. Cached models
// (controlled by the LLAMA_CACHE environment variable)"). No NCCL settings: llama.cpp splits by
// layer (`-sm layer`, its default) without NCCL, even with more than one card.
func buildLlamaCppEnv(o provider.ServeOptions) map[string]string {
	env := map[string]string{"LLAMA_API_KEY": o.APIKey, "LLAMA_CACHE": "/workspace"}
	if o.HFToken != "" {
		// A GGUF row downloads from the Hub too (`-hf`/`-hff`). [DOC]
		// github.com/ggml-org/llama.cpp common/arg.cpp:3103-3108 at tag b10818: `--hf-token`'s
		// default is "value from HF_TOKEN environment variable" (`.set_env("HF_TOKEN")`), so no
		// flag is needed: the env var alone authenticates the download. "" means unauthenticated.
		env["HF_TOKEN"] = o.HFToken
	}
	return env
}

// llamaCppEnvKeys names buildLlamaCppEnv's keys, sorted, and never a value, for the same reason
// as provider.VLLMServeEnvKeys.
func llamaCppEnvKeys(o provider.ServeOptions) []string {
	env := buildLlamaCppEnv(o)
	keys := make([]string, 0, len(env))
	for k := range env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// DescribeServe reports the image-or-template string, the disk and volume sizes, the exact
// `args` string and the names of the `env` keys a serving CreateMachine call will send for o.
// The console calls it to write the same figures into the claim record before the provider is
// asked to create anything. CreateMachine calls the same helper functions, so the record and the
// request cannot drift apart. EnvKeys are names only: no env value, and above all not the
// machine's key, is returned here. It ignores ctx and never errors, unlike Verda's, which asks
// GET /v1/images to choose a host image.
//
// o.Format branches this to one of two model servers: "gguf" answers the pinned llama.cpp image
// and its args and env; anything else (safetensors, fp8) answers RunPod's verified vLLM
// template, with the vLLM args and env built in package provider (provider.VLLMServeArgs,
// provider.VLLMServeEnvKeys) so Verda's create sends the identical string.
func (c *Client) DescribeServe(ctx context.Context, o provider.ServeOptions) (provider.ServeDescription, error) {
	containerGB, persistentGB := provider.ServingDiskSizes(o.DiskBytes)
	if o.Format == "gguf" {
		return provider.ServeDescription{
			ImageOrTemplate:  llamaCppImage,
			ContainerDiskGB:  containerGB,
			PersistentDiskGB: persistentGB,
			Args:             buildLlamaCppArgs(o),
			EnvKeys:          llamaCppEnvKeys(o),
		}, nil
	}
	return provider.ServeDescription{
		ImageOrTemplate:  runpodServeTemplateID,
		ContainerDiskGB:  containerGB,
		PersistentDiskGB: persistentGB,
		Args:             provider.VLLMServeArgs(o),
		EnvKeys:          provider.VLLMServeEnvKeys(o),
	}, nil
}

// BuildCreateBody builds the exact body CreateMachine marshals and POSTs for o, with no network
// call, so the body can be inspected without renting. CreateMachine calls this same function, so
// the request actually sent and whatever this answers cannot drift apart.
func BuildCreateBody(o provider.CreateOptions) (any, error) {
	gpuTypeID, count, err := parseShapeCode(o.ShapeCode)
	if err != nil {
		return nil, err
	}
	if o.Serve == nil {
		return nil, errors.New("a create must name a model to serve")
	}
	containerGB, persistentGB := provider.ServingDiskSizes(o.Serve.DiskBytes)
	if o.Serve.Format == "gguf" {
		body := createLlamaCppPodRequest{
			Name:   o.Hostname,
			Image:  llamaCppImage,
			Gpu:    createGpuConfig{ID: gpuTypeID, Count: count},
			Cloud:  "SECURE",
			Ports:  []string{"8000/http"},
			Disk:   containerGB,
			Mounts: &podMounts{Persistent: &persistentMount{Size: persistentGB, Path: "/workspace"}},
			Args:   buildLlamaCppArgs(*o.Serve),
			Env:    buildLlamaCppEnv(*o.Serve),
		}
		if len(o.Locations) > 0 {
			body.DataCenterIDs = o.Locations
		}
		return body, nil
	}
	body := createServingPodRequest{
		Name:       o.Hostname,
		TemplateID: runpodServeTemplateID,
		Gpu:        createGpuConfig{ID: gpuTypeID, Count: count},
		Cloud:      "SECURE",
		Ports:      []string{"8000/http"},
		Disk:       containerGB,
		Mounts:     &podMounts{Persistent: &persistentMount{Size: persistentGB, Path: "/workspace"}},
		Args:       provider.VLLMServeArgs(*o.Serve),
		Env:        provider.VLLMServeEnv(*o.Serve),
	}
	if len(o.Locations) > 0 {
		body.DataCenterIDs = o.Locations
	}
	return body, nil
}

// CreateMachine asks RunPod to create one pod of o.ShapeCode ("<gpu type id> x<count>", parsed
// back apart here), sending o.Locations as `dataCenterIds` when it carries any (RunPod's
// scheduler picks among them, or picks anywhere at all when the list is omitted, [DOC]
// docs.runpod.io/api-reference-v2/pods/create-a-pod). Costs real money. [DOC]
// https://api.runpod.io/v2/openapi.json, `POST /v2/pods` answers 201 with the created Pod, the
// shape ListMachines reads, `id` and `name` both required and non-empty on that schema. When the
// body cannot be read as that shape, or reads as one with no id, this is never settled as a
// refusal, because the status was already checked and was not one; it is
// provider.ErrCreateOutcomeUnknown instead, and the caller must leave the claim open for the
// watcher to reconcile against a fresh answer. The pod is created already serving o.Serve's
// model (BuildCreateBody): RunPod's verified vLLM template for safetensors and fp8, or the
// pinned llama.cpp image for gguf, plus `args`, `ports ["8000/http"]`, `mounts.persistent` sized
// for the weights, and the model server's `env`.
func (c *Client) CreateMachine(ctx context.Context, o provider.CreateOptions) (provider.RunningMachine, error) {
	_, count, err := parseShapeCode(o.ShapeCode)
	if err != nil {
		return provider.RunningMachine{}, fmt.Errorf("runpod: %w", err)
	}
	body, err := BuildCreateBody(o)
	if err != nil {
		return provider.RunningMachine{}, fmt.Errorf("runpod: %w", err)
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return provider.RunningMachine{}, fmt.Errorf("runpod: build the create request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, baseURL+"/pods", bytes.NewReader(raw))
	if err != nil {
		return provider.RunningMachine{}, fmt.Errorf("runpod: build the create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.apiKey)
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return provider.RunningMachine{}, fmt.Errorf("runpod: could not be reached to create %s, so the request may or may not have arrived: %w: %w", o.ShapeCode, err, provider.ErrCreateOutcomeUnknown)
	}
	defer resp.Body.Close()
	respRaw, _ := io.ReadAll(resp.Body)
	// respText is RunPod's response body, with this machine's key (o.Serve.APIKey) redacted before
	// it reaches an error string: nobody has checked whether RunPod ever echoes a submitted `env`
	// value back in a validation error, and the log is read back over the network by GET /api/log,
	// so the key must not be able to appear there.
	respText := provider.RedactSecrets(provider.Truncate(respRaw, 300), o.Serve)
	if resp.StatusCode >= 400 {
		return provider.RunningMachine{}, fmt.Errorf("runpod: refused to create %s, status %d: %s", o.ShapeCode, resp.StatusCode, respText)
	}
	var row pod
	unmarshalErr := json.Unmarshal(respRaw, &row)
	if unmarshalErr != nil || row.ID == "" {
		// This status was already checked above and is not a refusal (< 400), per [DOC]
		// https://api.runpod.io/v2/openapi.json, POST /v2/pods: a success answers 201 with the
		// Pod shape `pod` reads, `id` and `name` both required and non-empty on that schema. So
		// an answer that will not parse as a pod, or parses but names no id, is not a pod this
		// program recognises, and that is not proof the pod was never made. It is never settled
		// as a plain refusal: provider.ErrCreateOutcomeUnknown tells the caller to leave the
		// claim open instead, for reconcileClaims to match it by hostname against a fresh
		// ListMachines answer.
		if unmarshalErr != nil {
			return provider.RunningMachine{}, fmt.Errorf("runpod: created %s but answered with something that is not the pod shape: %s: %w", o.ShapeCode, respText, provider.ErrCreateOutcomeUnknown)
		}
		return provider.RunningMachine{}, fmt.Errorf("runpod: created %s but answered a pod with no id: %s: %w", o.ShapeCode, respText, provider.ErrCreateOutcomeUnknown)
	}
	gotCount := row.Gpu.Count
	if gotCount <= 0 {
		gotCount = count
	}
	price := row.Cost
	return provider.RunningMachine{
		ProviderKey:    providerKey,
		ProviderID:     row.ID,
		ShapeCode:      shapeCode(row.Gpu.ID, gotCount),
		DisplayName:    row.Image,
		GPUName:        row.Gpu.ID,
		Hostname:       row.Name,
		Location:       row.DataCenterID,
		PricePerHour:   &price,
		State:          row.Status,
		ServingBaseURL: "https://" + row.ID + "-8000.proxy.runpod.net",
	}, nil
}

// parseShapeCode reverses shapeCode: "<gpu type id> x<count>" back into the two parts a create
// call needs. A GPU type id may itself contain spaces ("AMD Instinct MI300X OAM"), so this looks
// for the LAST " x" followed by a positive whole number, never a bare split.
func parseShapeCode(code string) (gpuTypeID string, count int, err error) {
	idx := strings.LastIndex(code, " x")
	if idx < 0 {
		return "", 0, fmt.Errorf("'%s' does not carry a ' x<count>' card count", code)
	}
	n, convErr := strconv.Atoi(code[idx+2:])
	if convErr != nil || n <= 0 {
		return "", 0, fmt.Errorf("'%s' does not carry a valid card count", code)
	}
	return code[:idx], n, nil
}

// DestroyMachine asks RunPod to terminate the pod addressed by providerID. [DOC]
// https://api.runpod.io/v2/openapi.json, `DELETE /v2/pods/{id}`: "Permanently terminates and
// deletes a pod. This is irreversible: compute is released, any mounts.persistent host-local
// storage is destroyed with it (a mounts.network volume is only detached - the volume itself is
// not deleted)." Answers 204 with no body on success, 404 when providerID names no pod.
func (c *Client) DestroyMachine(ctx context.Context, providerID string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, baseURL+"/pods/"+providerID, nil)
	if err != nil {
		return fmt.Errorf("build the destroy request for %s: %w", providerID, err)
	}
	req.Header.Set("Authorization", "Bearer "+c.apiKey)
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("could not be reached to destroy %s: %w", providerID, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode == http.StatusNotFound {
		// A 404 here means providerID names no pod: RunPod already has nothing to destroy, which
		// is not a refusal to retry. Without this case a destroy of a pod that is already gone
		// would be read as refused, retried until the give-up limit, and reported as still
		// billing.
		return fmt.Errorf("runpod: pod %s: %w", providerID, provider.ErrMachineNotFound)
	}
	if resp.StatusCode >= 400 {
		return fmt.Errorf("refused to destroy %s, status %d: %s", providerID, resp.StatusCode, provider.Truncate(raw, 300))
	}
	return nil
}

func shapeCode(gpuTypeID string, count int) string {
	return fmt.Sprintf("%s x%d", gpuTypeID, count)
}

func (c *Client) getJSON(ctx context.Context, path string, out any) error {
	raw, err := c.getRaw(ctx, path)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("answered %s with something that is not JSON: %w", path, err)
	}
	return nil
}

// getRaw makes the GET call and returns the response body unparsed, for a call whose answer
// shape varies (ListMachines above).
func (c *Client) getRaw(ctx context.Context, path string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+path, nil)
	if err != nil {
		return nil, fmt.Errorf("build the request for %s: %w", path, err)
	}
	req.Header.Set("Authorization", "Bearer "+c.apiKey)
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("could not be reached for %s: %w", path, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("refused %s, status %d: %s", path, resp.StatusCode, provider.Truncate(raw, 300))
	}
	return raw, nil
}
