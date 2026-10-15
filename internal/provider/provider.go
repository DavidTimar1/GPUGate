// Package provider holds the one interface both GPU providers answer to, and what the two
// adapters share: the vLLM command line, disk sizing, card facts, location sets and the HTTP
// client. The adapters themselves are in the runpod and verda packages.
package provider

import (
	"context"
	"errors"
	"fmt"
)

// Shape is one machine shape one provider offers: its cards, its whole-machine VRAM, its
// on-demand price, and where it can be ordered right now.
//
// WholeMachineVRAMGB is EVERY CARD SUMMED. Never a bare "VRAM", and this type never carries a
// bare "Fits" method for the same reason: the fit arithmetic lives in package fit and says in its
// own name which sum it means.
type Shape struct {
	ProviderKey          string
	Code                 string
	DisplayName          string
	GPUName              string
	GPUCount             int
	WholeMachineVRAMGB   int
	ComputeCapability    *int     // nil means unknown, never "too old"
	OnDemandPricePerHour *float64 // nil means the provider named no price
	OrderableLocations   []string // where an on-demand order would be accepted right now
	// CardArchitecture, CardYear and CardFP8 are LookupCardFacts's other answers for GPUName,
	// alongside ComputeCapability above: nil wherever cardFacts does not carry that fact (an
	// unmatched name, an [UNKNOWN] year, or a MIG slice's unchecked FP8). The fit arithmetic
	// uses ComputeCapability alone; these three exist only for the row the page shows.
	CardArchitecture *string
	CardYear         *int
	CardFP8          *bool
}

// RunningMachine is one machine this account holds right now, as a provider's live answer
// reports it, never as our notes remember it. ProviderID is the provider's id for the machine,
// the only thing later steps can address it by. Hostname is the name this program gave the
// machine at create time, carried through so a claim written before the create call can be
// matched back to a machine a later fresh answer reports, even when the create call's response
// was never seen (the process died first).
type RunningMachine struct {
	ProviderKey  string
	ProviderID   string
	ShapeCode    string
	DisplayName  string
	GPUName      string
	Hostname     string
	Location     string
	PricePerHour *float64 // nil means the provider named no price for this machine
	State        string   // the provider's own word for what it is doing, e.g. "running"
	// UptimeSeconds is RunPod's `runtime.uptime` (seconds since the container started), mapped by
	// both runpod.Client.GetMachine and ListMachines from the same pod row. It is nil before a
	// container exists (PROVISIONING or STARTING) and on a provider that names no such figure.
	// Only the readiness wait's status line reads it.
	UptimeSeconds *int
	// ServingBaseURL is where this machine's model server answers, scheme and host and port, no
	// trailing path: "https://<pod id>-8000.proxy.runpod.net" on RunPod (derived from the id, so
	// always set), always "" on Verda, where SSHAddr says how the machine is reached. Never
	// logged and never put on the page: read only to build one outgoing request.
	ServingBaseURL string
	// SSHAddr is "<ip>:22" on a Verda machine once GET /v1/instances names a non-null ip, "" while
	// it does not and always on RunPod. Only port 22 answers on a Verda machine
	// (docs/providers.md#verda), so a non-empty SSHAddr means the model server is reached only
	// through this program's SSH tunnel to the machine's own loopback (internal/sshtunnel).
	// Never logged.
	SSHAddr string
}

// CreateOptions is what a launch asks a provider to rent: one shape, named so a later fresh
// answer can be matched back to the claim that asked for it. Serve asks the provider to bring
// the machine up already serving that model. Every launch names a model (the launch is refused
// before any claim is written otherwise), so Serve is always set; a provider with no serving path
// for the model's format has already refused it through ServesFormat.
type CreateOptions struct {
	ShapeCode string
	// Locations is where this machine may be ordered, already resolved from the user's choice on
	// the launch panel: empty means "let the provider choose"; one or more names means "one of
	// these, the provider's pick" (RunPod's dataCenterIds semantics, [DOC]
	// docs.runpod.io/api-reference-v2/pods/create-a-pod). RunPod sends the list as dataCenterIds,
	// or omits it when empty. Verda's create takes one location: the first entry, and an empty
	// list is refused because its `location_code` is required ([DOC] Verda's openapi.json).
	Locations []string
	Hostname  string // this claim's own name for the machine
	Serve     *ServeOptions
}

// ServeOptions is what to serve on the machine as it is created: the model's catalog figures,
// and the console's context-window and chat-count boxes, already converted to the model
// server's units (raw tokens, not the page's "thousands"). Read by both providers'
// CreateMachine and DescribeServe.
type ServeOptions struct {
	ModelName string // the catalog row's own name, for the claim record and the log
	HFRepo    string // the model's Hugging Face repository, what --model (vLLM) or -hf (llama.cpp) is given
	// Format is the catalog row's format ("gguf", "safetensors", "fp8"): which model server a
	// RunPod create branches to. Verda serves safetensors and fp8 only (ServesFormat refuses
	// gguf), so a gguf row never reaches Verda's DescribeServe or CreateMachine.
	Format string
	// ShapeCode is the shape this launch is for (CreateOptions.ShapeCode, carried here too).
	// Verda only: DescribeServe reads it to choose a host image via
	// GET /v1/images?instance_type=<code>, one image per shape. RunPod never reads it.
	ShapeCode string
	// HFFile is the exact GGUF file `-hff` names on a llama.cpp create; "" on a vLLM row, where
	// it is never read.
	HFFile          string
	ReasoningParser string // vLLM's --reasoning-parser value; "" means the catalog carries none. Ignored for a GGUF row: llama.cpp has no such flag.
	ToolCallParser  string // vLLM's --tool-call-parser value; "" means the catalog carries none. Ignored for a GGUF row: llama.cpp has no such flag.
	ContextTokens   int    // the per-chat context window, in raw tokens: vLLM's --max-model-len directly, or llama.cpp's -c after multiplying by ConcurrentChats
	ConcurrentChats int    // --max-num-seqs (vLLM) or -np (llama.cpp)
	DiskBytes       *int64 // the catalog row's disk_bytes, for sizing disk and mounts.persistent
	// GPUCount is how many cards the machine has. vLLM uses ONE card unless told otherwise, so a
	// machine that fits a model only by summing its cards (whole-machine VRAM) needs
	// --tensor-parallel-size set to this, or the load fails on the first card and the machine
	// never serves (docs/providers.md#multi-card-machines).
	GPUCount int
	// APIKey is this machine's randomly generated key, sent as the model server's VLLM_API_KEY so
	// that it, and not RunPod's template default (derived from the pod id, which is public in the
	// pod's URL), is what the machine requires. Minted once per launch, before the provider is
	// asked to create anything, and never logged.
	APIKey string
	// HFToken is the user's Hugging Face token (HF_API_KEY, falling back to HF_TOKEN, read once in
	// cmd/gpugate/main.go), sent so the machine's download of the model's weights from the Hub is
	// authenticated instead of rate-limited. "" means no token is configured and the download
	// proceeds unauthenticated; an empty token never refuses a launch. It is never a provider
	// account credential, never this machine's serving key, and never put in an EnvKeys list or a
	// claim record by value: only its env key name, HF_TOKEN, travels that far.
	HFToken string
	// Temperature, TopK, TopP, MinP and RepeatPenalty are the five sampling values a launch may
	// carry. nil means the box was empty, so no flag is sent for it; when all five are nil,
	// VLLMServeArgs omits --override-generation-config entirely.
	Temperature   *float64
	TopK          *float64
	TopP          *float64
	MinP          *float64
	RepeatPenalty *float64
	// GPUMemoryUtilization is the launch's --gpu-memory-utilization value; nil means the box was
	// empty and VLLMServeArgs falls back to 0.95. vLLM only: ignored on a GGUF row, which has no
	// such flag; the page labels this box "vLLM only".
	GPUMemoryUtilization *float64
	// MaxNumBatchedTokens is the launch's --max-num-batched-tokens value; nil means the box was
	// empty and VLLMServeArgs derives the figure from ContextTokens and ConcurrentChats. vLLM
	// only: ignored on a GGUF row, which has no such flag.
	MaxNumBatchedTokens *int
	// BatchTokens, MicroBatchTokens and CacheReuseTokens are the launch panel's -b, -ub and
	// --cache-reuse boxes for a GGUF row: per launch, never saved. nil means the box was empty,
	// so the runpod adapter's llama.cpp command line omits that flag and llama.cpp's compiled-in
	// default (2048, 512, 0) applies. llama.cpp only: VLLMServeArgs ignores them.
	BatchTokens      *int
	MicroBatchTokens *int
	CacheReuseTokens *int
	// Thinking is the "Thinking" model-setting box's value: nil or true sends the flag that turns
	// thinking on, false sends the flag that turns it off, for both model servers. Read by
	// VLLMServeArgs and by the runpod adapter's llama.cpp command line (see
	// docs/providers.md#model-servers for what each sends).
	Thinking *bool
	// TrustRemoteCode is the catalog row's trust_remote_code (internal/catalog): true means
	// VLLMServeArgs appends --trust-remote-code, so the machine runs the model repository's own
	// Python. vLLM only: llama.cpp has no such flag.
	TrustRemoteCode bool
	// HostImage is Verda's chosen boot image (DescribeServe's ServeDescription.HostImage, copied
	// on by the launch), sent as the create's `image`. "" on RunPod, where it is never read.
	HostImage string
	// StartupScriptID is the id of the Verda startup script created for this launch
	// (verda.Client.CreateStartupScript), sent as the create's `startup_script_id`. "" on RunPod,
	// which has no startup script.
	StartupScriptID string
}

// ServeDescription is what a provider's DescribeServe answers: what a serving CreateMachine call
// for the same ServeOptions will ask for, so the claim can record it before the provider is
// asked to create anything. EnvKeys are names only, sorted: no env value, and above all not the
// machine's key, is carried here.
type ServeDescription struct {
	ImageOrTemplate  string   // the claim's image_or_template column
	HostImage        string   // Verda only: the image_type chosen from GET /v1/images; "" on RunPod
	ContainerDiskGB  int      // RunPod's disk; on Verda part of os_volume.size
	PersistentDiskGB int      // RunPod's mounts.persistent.size; on Verda the other part
	Args             string   // the exact vllm serve / llama-server argument string
	EnvKeys          []string // NAMES only, sorted
}

// ErrMachineNotFound marks a direct single-machine read (GetMachine) answering that the provider
// holds no machine under this id: RunPod's 404, or Verda's not-found answer. Any other error or
// status is never read the same way: it means the provider could not be asked, or answered
// something else, never that the machine is gone.
var ErrMachineNotFound = errors.New("the provider holds no machine with this id")

// ErrCreateOutcomeUnknown marks a CreateMachine error where the provider's answer was not a
// refusal (no error status, no clear "no capacity") but could not be understood well enough to
// name the machine it may have created, or where the provider could not be reached to answer at
// all (the request may have arrived and made a machine). The machine may exist and may be
// billing: a caller must never settle the claim as refused for this error. It must leave the
// claim open so the next fresh provider answer reconciles it by hostname, because our own call's
// outcome is never the truth about what is running.
var ErrCreateOutcomeUnknown = errors.New("the create answered but the outcome could not be told apart from a refusal")

// Provider is what this program asks a GPU cloud for: its catalog and live availability, the
// machines the account holds, and the two calls that spend or stop money, CreateMachine and
// DestroyMachine.
type Provider interface {
	// Key is the short name this provider is stored and addressed under, e.g. "verda".
	Key() string
	// DisplayName is what a person sees on the page, e.g. "Verda".
	DisplayName() string
	// CredentialsConfigured reports whether this provider has been handed the credential
	// names it needs, without making a network call.
	CredentialsConfigured() bool
	// ServesFormat answers whether this provider's serving path can run a model published in
	// format (a catalog.Model.Format value: "gguf", "safetensors", "fp8"). false carries a short
	// reason a table cell or a refusal can show. This is a fact about the provider's model
	// server, never about one model's memory need: the memory arithmetic is package fit's job and
	// is judged separately, after this.
	ServesFormat(format string) (bool, string)
	// ServesShape answers whether this provider's serving path can run on the machine shape with
	// this code, whatever the model. false carries a short reason a table cell or a refusal can
	// show. Judged beside ServesFormat, before any memory arithmetic.
	ServesShape(shapeCode string) (bool, string)
	// ListShapes answers every on-demand machine shape this provider offers, with its price, its
	// whole-machine VRAM, and its orderable locations right now. An error means the provider
	// could not be asked at all: that is its own answer, never read as "nothing on offer".
	ListShapes(ctx context.Context) ([]Shape, error)
	// ListMachines answers every machine this account holds right now, whether this program
	// started it or not. An error means the provider could not be asked at all, and that is never
	// read as "nothing is running".
	ListMachines(ctx context.Context) ([]RunningMachine, error)
	// DestroyMachine asks this provider to permanently tear down the machine addressed by
	// providerID (the same value RunningMachine.ProviderID carries). It starts an irreversible
	// teardown. An error means the provider refused or could not be reached, and the caller
	// retries. Its success is never proof the machine is gone: only a fresh ListMachines answer
	// that no longer carries it is that proof.
	DestroyMachine(ctx context.Context, providerID string) error
	// CreateMachine asks this provider to rent one machine of the shape and in the place o
	// names. It spends real money. An error means the provider refused or could not be reached;
	// the caller must not retry with a different shape or location on its own initiative, because
	// a refusal is reported and never walked past. When the provider's answer named a machine id
	// before the error struck (a create that half-succeeded), the returned RunningMachine's
	// ProviderID is non-empty even though err is not nil, so the caller can still record what was
	// actually rented.
	CreateMachine(ctx context.Context, o CreateOptions) (RunningMachine, error)
	// DescribeServe reports what a serving CreateMachine call for o will ask for, and costs
	// nothing: RunPod ignores ctx and never errors; Verda asks GET /v1/images (bound by ctx) to
	// choose a host image, and fails when that call does. Called before the claim is written, so
	// the record and the request cannot drift apart.
	DescribeServe(ctx context.Context, o ServeOptions) (ServeDescription, error)
}

// ByKey answers the provider in providers whose Key is key, or nil when none matches.
func ByKey(providers []Provider, key string) Provider {
	for _, p := range providers {
		if p.Key() == key {
			return p
		}
	}
	return nil
}

// PriceText writes an hourly price the way a log line or a listing shows it, or "price unknown"
// when the provider named none.
func PriceText(p *float64) string {
	if p == nil {
		return "price unknown"
	}
	return fmt.Sprintf("$%.3f/hr", *p)
}
