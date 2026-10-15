// Package fit is the memory arithmetic that decides whether a machine can serve a model at a
// context window and a chat count, and what is left over when it can. Pure functions: nothing
// here opens a file, a database or a socket.
//
// The judgement constants below (usableVRAMFraction, workingMemoryGiB,
// perGPUFixedOverheadGiB, estimatedTermsMargin) are judgement calls, not measurements. What is a
// fact rather than a judgement: the shape of the KV-cache formula (chats x context x bytes per
// token, from the model's stored per-token figure) and the two unit traps (decimal GB versus
// GiB, and a context "thousand" being 1024 tokens).
//
// Whole-machine VRAM is every card summed and one card's VRAM is not. FitsWholeMachineVRAM says
// so in its name; there is no bare Fits.
package fit

import "fmt"

const bytesPerGiB = 1024 * 1024 * 1024

// bytesPerCatalogSizeGB: a catalog entry's size_gb is DECIMAL gigabytes, the file's byte count
// divided by a thousand million. Converting with 2^30 instead overstates a model by 7.4%
// before anything else is added, so weights are always converted through this constant.
const bytesPerCatalogSizeGB = 1_000_000_000

// tokensPerContextUnit: the context box on the page is typed in THOUSANDS of tokens, and one of
// those thousands is 1024 tokens, so typing 256 means 262,144. It is a unit of the page, not of
// the arithmetic, so it is converted once, here, at the edge.
const tokensPerContextUnit = 1024

// The judgement constants. Each is a judgement, never a measurement, and every fit decision
// widens only the terms that are estimates: the weights, a published file's byte count, are never
// widened.
const (
	// usableVRAMFraction is the share of a card's advertised memory a plan may occupy. A
	// judgement leaving a tenth of the card for allocator fragmentation and for the gap
	// between advertised and truly usable memory.
	usableVRAMFraction = 0.90

	// workingMemoryGiB is a flat allowance for what a model server holds before a single
	// conversation exists: its activation buffers and pools. A judgement, deliberately not broken
	// out by model server. The term is of the order of 2 to 3 GiB for vLLM and well under a GiB
	// for llama.cpp at ordinary batch sizes, so 2.0 sits between the two rather than favouring
	// either.
	workingMemoryGiB = 2.0

	// perGPUFixedOverheadGiB is what one CUDA context costs per card, paid once per card because
	// a context exists per device. 1.0 GiB matches llama.cpp's published planning figure for
	// exactly this term (the fit_params_target default in common/common.h), a fact about that
	// project's own plan rather than a measurement of ours.
	perGPUFixedOverheadGiB = 1.0

	// estimatedTermsMargin widens the three estimated terms (working memory, fixed
	// overhead, conversations) above their point estimate. It does not touch the weights,
	// which are a fact about a published file rather than an estimate. 1.3 is a judgement:
	// enough to absorb an estimated term being a quarter low without inflating every
	// machine's requirement past what a smaller box could still serve.
	estimatedTermsMargin = 1.3
)

// MachineShape is one machine's cards and whole-machine memory, judged for one model. Never
// read as one card's memory: WholeMachineVRAMGB is every card summed.
type MachineShape struct {
	GPUCount           int
	WholeMachineVRAMGB int
	ComputeCapability  *int // nil means unknown, never "too old"
}

// Model is the figures the arithmetic needs about one catalog row.
type Model struct {
	Name                 string
	Format               string // catalog.Model.Format verbatim: "gguf", "safetensors", "fp8"
	WeightsGiB           float64
	KVCacheBytesPerToken *int // nil means this model cannot be sized; no figure is invented
	MinComputeCapability *int // nil means unknown, never excludes a machine
}

// ModelFromCatalog converts a catalog row's raw figures into what the arithmetic needs,
// converting the weight size from decimal GB (or an exact byte count, when the row carries
// one) into GiB.
func ModelFromCatalog(name, format string, sizeGB float64, diskBytes *int64, kvCacheBytesPerToken, minComputeCapability *int) Model {
	var weightsGiB float64
	if diskBytes != nil {
		weightsGiB = float64(*diskBytes) / bytesPerGiB
	} else {
		weightsGiB = sizeGB * bytesPerCatalogSizeGB / bytesPerGiB
	}
	return Model{
		Name:                 name,
		Format:               format,
		WeightsGiB:           weightsGiB,
		KVCacheBytesPerToken: kvCacheBytesPerToken,
		MinComputeCapability: minComputeCapability,
	}
}

// Plan is what a backend is asked to do: how long a conversation, and how many at once. The
// context is in raw tokens; ContextTokensFromThousands converts the page's own unit.
type Plan struct {
	ContextTokens   int
	ConcurrentChats int
}

// ContextTokensFromThousands converts the page's "thousands of tokens" box into raw tokens,
// where one of those thousands is 1024 tokens (tokensPerContextUnit).
func ContextTokensFromThousands(thousands int) int {
	return thousands * tokensPerContextUnit
}

// Verdict is why a machine can or cannot serve a model. Never merged into one bare "no".
type Verdict string

const (
	Launchable      Verdict = "launchable"
	TooSmall        Verdict = "too_small"
	CardCannotRunIt Verdict = "card_cannot_run_it"
	CannotBeSized   Verdict = "cannot_be_sized"
	// CannotServeFormat is judged before any of the memory arithmetic below, by the caller that
	// knows both the model's format and the provider it is judging: package fit never asks a
	// provider anything. The constant lives here because every other Verdict does, and because
	// the string it marshals to is part of the contract ui/src/app.js reads (rowIsLaunchable).
	CannotServeFormat Verdict = "cannot_serve_format"
	// CannotServeShape is judged in the same place and for the same reason as CannotServeFormat,
	// but about the machine shape rather than the model's format: the provider's serving path
	// cannot run on that shape at all, whatever the model.
	CannotServeShape Verdict = "cannot_serve_shape"
)

// Answer is one machine shape judged for one model at one plan.
type Answer struct {
	Verdict Verdict
	// NeedGiB is what this backend is treated as taking, with the estimated-terms margin
	// applied. Zero when the verdict is CannotBeSized.
	NeedGiB float64
	// FreeAfterGiB is what would be left on the whole machine once this model is loaded,
	// against the usable share of its advertised memory. Negative when it does not fit.
	FreeAfterGiB float64
	// Reason is one short sentence a table cell can show: why it fits, or why it does not.
	Reason string
}

// FitsWholeMachineVRAM judges one machine SHAPE (every card summed) for one model at
// one plan. The card generation is checked before memory, because no amount of memory fixes a
// card that cannot execute the build; a card or a model this program has no generation figure
// for is let through, because not knowing is not the same as knowing it will not work.
func FitsWholeMachineVRAM(model Model, shape MachineShape, plan Plan) Answer {
	if model.MinComputeCapability != nil && shape.ComputeCapability != nil &&
		*shape.ComputeCapability < *model.MinComputeCapability {
		return Answer{
			Verdict: CardCannotRunIt,
			Reason: fmt.Sprintf(
				"this card's compute capability (%d) is below what this build needs (%d)",
				*shape.ComputeCapability, *model.MinComputeCapability,
			),
		}
	}

	if model.KVCacheBytesPerToken == nil {
		return Answer{
			Verdict: CannotBeSized,
			Reason:  "this model carries no KV-cache figure, so its memory need is not known",
		}
	}

	conversationsGiB := conversationMemoryGiB(plan, *model.KVCacheBytesPerToken)
	estimatedTerms := workingMemoryGiB + perGPUFixedOverheadGiB*float64(shape.GPUCount) + conversationsGiB
	needGiB := model.WeightsGiB + estimatedTerms*estimatedTermsMargin

	usableGiB := float64(shape.WholeMachineVRAMGB) * usableVRAMFraction
	freeAfter := usableGiB - needGiB

	if freeAfter < 0 {
		return Answer{
			Verdict:      TooSmall,
			NeedGiB:      needGiB,
			FreeAfterGiB: freeAfter,
			Reason:       fmt.Sprintf("short by %.1f GiB", -freeAfter),
		}
	}
	return Answer{
		Verdict:      Launchable,
		NeedGiB:      needGiB,
		FreeAfterGiB: freeAfter,
		Reason:       fmt.Sprintf("%.1f GiB free", freeAfter),
	}
}

// conversationMemoryGiB is the concurrency term: every live conversation's cache, together.
//
//	chats x context tokens x KV bytes per token
//
// This arithmetic does not model a hybrid model's per-conversation recurrent state or a model
// server's block-alignment rounding. That is a stated simplification, and it errs toward
// slightly under-counting a hybrid model's memory rather than inventing a figure for it.
func conversationMemoryGiB(plan Plan, kvCacheBytesPerToken int) float64 {
	bytes := float64(plan.ConcurrentChats) * float64(plan.ContextTokens) * float64(kvCacheBytesPerToken)
	return bytes / bytesPerGiB
}
