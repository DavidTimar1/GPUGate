package provider

import "strings"

// CardFacts is what this table knows about one physical chip: its architecture family name, the
// year that architecture (or, where a card's own date was found instead, that card) launched,
// NVIDIA's (or AMD's) published compute capability, and an explicit FP8 answer for the one chip
// this table carries no compute capability for at all. It is keyed by chip, never by one
// provider's spelling: canonicalCardName folds every provider's form-factor and edition suffix
// onto the same key.
type CardFacts struct {
	Architecture      string
	Year              *int // nil and [UNKNOWN]: no dated vendor page was found
	ComputeCapability *int // nil: absent from both NVIDIA pages (RTX A4500), or an AMD chip
	// FP8Override answers FP8 directly when ComputeCapability is nil and FP8 is still known from
	// a vendor statement (MI300X alone: AMD publishes no NVIDIA-style compute capability, but
	// vLLM's own docs name it FP8-capable). Read only by FP8() below, and only when
	// ComputeCapability is nil.
	FP8Override *bool
}

// fp8ComputeCapabilityFloor is vLLM's threshold, stated once here and read by FP8 rather than
// stored per entry. [DOC] https://docs.vllm.ai/en/stable/features/quantization/fp8, read
// 2026-09-10: "FP8 computation is supported on NVIDIA GPUs with compute capability >= 8.9 (Ada
// Lovelace, Hopper)". Blackwell (10.0 and above) is listed among FP8's supported hardware further
// down the same page and is above this floor too.
const fp8ComputeCapabilityFloor = 89

// FP8 answers whether this chip serves FP8: derived from ComputeCapability against
// fp8ComputeCapabilityFloor when this table carries one, or FP8Override when it does not. nil
// means neither source answers it.
func (f CardFacts) FP8() *bool {
	if f.ComputeCapability != nil {
		v := *f.ComputeCapability >= fp8ComputeCapabilityFloor
		return &v
	}
	return f.FP8Override
}

// Sources for the table below. The tags in each row's comment name one of these entries.
//
//   - [DOC-CC] Compute capability of every NVIDIA row: https://developer.nvidia.com/cuda-gpus,
//     read 2026-09-10. B300 and GB300 sit in the page's 10.3 row, not 10.0. RTX A4500 is absent
//     from the page, so its ComputeCapability is nil rather than a guess from its family.
//   - [DOC-LEGACY] Tesla V100's compute capability: https://developer.nvidia.com/cuda-legacy-gpus,
//     read 2026-09-10 (NVIDIA moved it off the current page).
//   - [DOC-VOLTA] Volta's launch year is [UNKNOWN]: no NVIDIA page with a date for it was found.
//   - [DOC-AMPERE] https://nvidianews.nvidia.com/news/nvidias-new-ampere-data-center-gpu-in-full-production,
//     "the first GPU based on the NVIDIA Ampere architecture, the NVIDIA A100, is in full
//     production", dated May 14, 2020, read 2026-09-10.
//   - [DOC-ADA] https://blogs.nvidia.com/blog/keynote-gtc-nvidia-ceo/, "the first GPUs based on
//     the new NVIDIA Ada Lovelace architecture", dated September 20, 2022, read 2026-09-10.
//   - [DOC-HOPPER] https://blogs.nvidia.com/blog/h100-transformer-engine/, dated March 22, 2022,
//     read 2026-09-10 (the NVIDIA Hopper architecture and its first GPU, H100).
//   - [DOC-BLACKWELL] https://blogs.nvidia.com/blog/2024-gtc-keynote/, the new Blackwell computing
//     platform, dated March 18, 2024, read 2026-09-10 (B200, GB200).
//   - [DOC-BLACKWELL-ULTRA] https://nvidianews.nvidia.com/news/nvidia-blackwell-ultra-ai-factory-platform-paves-way-for-age-of-ai-reasoning,
//     dated March 18, 2025, read 2026-09-10 (B300, GB300).
//   - [DOC-MI300X] https://www.amd.com/en/products/accelerators/instinct/mi300/mi300x.html, read
//     2026-09-10: "Launch Date 12/06/2023". Compute capability is NVIDIA's metric and AMD
//     publishes none for this chip, so FP8Override carries FP8 instead: vLLM's quantization docs
//     (see fp8ComputeCapabilityFloor) name AMD "MI300+" as FP8 capable.

// cardFacts has one entry per chip, never per provider spelling. canonicalCardName must run on a
// provider's name before this map is read.
var cardFacts = map[string]CardFacts{
	"Tesla V100":   {Architecture: "Volta", ComputeCapability: ip(70)},                                         // CC [DOC-LEGACY]. Year [DOC-VOLTA].
	"A100":         {Architecture: "Ampere", Year: ip(2020), ComputeCapability: ip(80)},                        // CC [DOC-CC]. Year [DOC-AMPERE].
	"A40":          {Architecture: "Ampere", Year: ip(2020), ComputeCapability: ip(86)},                        // CC [DOC-CC]. Year [DOC-AMPERE].
	"RTX 3090":     {Architecture: "Ampere", Year: ip(2020), ComputeCapability: ip(86)},                        // CC [DOC-CC]. Year [DOC-AMPERE].
	"RTX A4000":    {Architecture: "Ampere", Year: ip(2020), ComputeCapability: ip(86)},                        // CC [DOC-CC]. Year [DOC-AMPERE].
	"RTX A4500":    {Architecture: "Ampere", Year: ip(2020), ComputeCapability: nil},                           // CC [DOC-CC]: absent from the page. Year [DOC-AMPERE].
	"RTX A5000":    {Architecture: "Ampere", Year: ip(2020), ComputeCapability: ip(86)},                        // CC [DOC-CC]. Year [DOC-AMPERE].
	"RTX A6000":    {Architecture: "Ampere", Year: ip(2020), ComputeCapability: ip(86)},                        // CC [DOC-CC]. Year [DOC-AMPERE].
	"H100":         {Architecture: "Hopper", Year: ip(2022), ComputeCapability: ip(90)},                        // CC [DOC-CC]. Year [DOC-HOPPER].
	"H200":         {Architecture: "Hopper", Year: ip(2022), ComputeCapability: ip(90)},                        // CC [DOC-CC]. Year [DOC-HOPPER] (architecture year; H200's own later date was not sourced).
	"L4":           {Architecture: "Ada Lovelace", Year: ip(2022), ComputeCapability: ip(89)},                  // CC [DOC-CC]. Year [DOC-ADA].
	"L40":          {Architecture: "Ada Lovelace", Year: ip(2022), ComputeCapability: ip(89)},                  // CC [DOC-CC]. Year [DOC-ADA].
	"L40S":         {Architecture: "Ada Lovelace", Year: ip(2022), ComputeCapability: ip(89)},                  // CC [DOC-CC]. Year [DOC-ADA].
	"RTX 2000 Ada": {Architecture: "Ada Lovelace", Year: ip(2022), ComputeCapability: ip(89)},                  // CC [DOC-CC]. Year [DOC-ADA].
	"RTX 4000 Ada": {Architecture: "Ada Lovelace", Year: ip(2022), ComputeCapability: ip(89)},                  // CC [DOC-CC]. Year [DOC-ADA].
	"RTX 6000 Ada": {Architecture: "Ada Lovelace", Year: ip(2022), ComputeCapability: ip(89)},                  // CC [DOC-CC]. Year [DOC-ADA].
	"RTX 4090":     {Architecture: "Ada Lovelace", Year: ip(2022), ComputeCapability: ip(89)},                  // CC [DOC-CC]. Year [DOC-ADA].
	"B200":         {Architecture: "Blackwell", Year: ip(2024), ComputeCapability: ip(100)},                    // CC [DOC-CC]. Year [DOC-BLACKWELL].
	"RTX PRO 4000": {Architecture: "Blackwell", Year: ip(2024), ComputeCapability: ip(120)},                    // CC [DOC-CC]. Year [DOC-BLACKWELL].
	"RTX PRO 4500": {Architecture: "Blackwell", Year: ip(2024), ComputeCapability: ip(120)},                    // CC [DOC-CC]. Year [DOC-BLACKWELL].
	"RTX PRO 6000": {Architecture: "Blackwell", Year: ip(2024), ComputeCapability: ip(120)},                    // CC [DOC-CC]. Year [DOC-BLACKWELL].
	"RTX 5090":     {Architecture: "Blackwell", Year: ip(2024), ComputeCapability: ip(120)},                    // CC [DOC-CC]. Year [DOC-BLACKWELL].
	"B300":         {Architecture: "Blackwell Ultra", Year: ip(2025), ComputeCapability: ip(103)},              // CC [DOC-CC] (10.3, not 10.0). Year [DOC-BLACKWELL-ULTRA].
	"GB300":        {Architecture: "Blackwell Ultra", Year: ip(2025), ComputeCapability: ip(103)},              // CC [DOC-CC]. Year [DOC-BLACKWELL-ULTRA].
	"MI300X":       {Architecture: "AMD CDNA3", Year: ip(2023), ComputeCapability: nil, FP8Override: bp(true)}, // Year and FP8Override [DOC-MI300X].
}

// cardNameSuffixes are a provider's form-factor and edition markings, never a different chip:
// "PCIe", "SXM", "NVL", "WK" (Workstation Edition), "CC" (confidential computing), "SE" (Server
// Edition) and the per-size "40GB" and "80GB" A100 split. A MIG slice's own size ("24GB", "48GB")
// goes with the literal word "MIG", which canonicalCardName cuts first.
var cardNameSuffixes = []string{"PCIe", "SXM", "NVL", "WK", "CC", "SE", "40GB", "80GB"}

// cardNameAliases map a provider's spelling to the key cardFacts uses, once the suffixes above
// are stripped. RunPod's MIG catalog names the parent card "PRO 6000", not "RTX PRO 6000".
var cardNameAliases = map[string]string{
	"PRO 6000": "RTX PRO 6000",
}

// canonicalCardName is the one normaliser: it folds a provider's card name onto the chip key
// cardFacts answers under, stripping a MIG slice's size and every suffix in cardNameSuffixes,
// then applying cardNameAliases. isMIG reports that raw named a slice of one physical card, not
// a card of its own: its parent's architecture and year still apply, but its FP8 is never read
// from the parent's (see LookupCardFacts).
func canonicalCardName(raw string) (canonical string, isMIG bool) {
	name := strings.TrimSpace(raw)
	if idx := strings.Index(name, "MIG"); idx >= 0 {
		isMIG = true
		name = strings.TrimSpace(name[:idx])
	}
	for _, suf := range cardNameSuffixes {
		name = strings.TrimSpace(strings.TrimSuffix(name, " "+suf))
	}
	if alias, ok := cardNameAliases[name]; ok {
		name = alias
	}
	return name, isMIG
}

// LookupCardFacts answers what cardFacts knows about a provider's card name: matched is false
// when canonicalCardName's result names no chip this table carries, and every other return is
// then the zero value, unknown and never a guess. On a MIG slice (isMIG), FP8 is always nil
// regardless of what CardFacts.FP8 would derive from the parent's compute capability: nobody
// has checked whether a slice of a card serves FP8 the way the whole card does.
func LookupCardFacts(gpuName string) (facts CardFacts, isMIG bool, matched bool) {
	canonical, isMIG := canonicalCardName(gpuName)
	facts, matched = cardFacts[canonical]
	return facts, isMIG, matched
}

// ip and bp build a pointer to a fresh copy of v for the literal table above, so no caller can
// mutate a shared package-level variable through it.
func ip(v int) *int   { return &v }
func bp(v bool) *bool { return &v }
