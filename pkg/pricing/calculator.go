// Package pricing implements the price table and cost math for the FinOps
// circuit-breaker proxy.
//
// Money is always integer micro-USD (µUSD): 1 USD = 1_000_000 µUSD. The
// floating-point dollar amounts in config/pricing.json are normalized to
// integers exactly once at Load time; every cost computation afterwards is
// integer-only.
package pricing

import (
	"encoding/json"
	"math"
	"strings"
)

// MicroUSDPerUSD is the fixed-point scale: 1 USD = 1_000_000 µUSD.
const MicroUSDPerUSD = 1_000_000

// defaultCharsPerToken is the chars-per-token heuristic used by
// EstimatePromptTokens.
const defaultCharsPerToken = 4.0

// maxUSDPer1M is the sanity cap on a single price (USD per 1M tokens) accepted
// by Load; anything above it is rejected as a likely data-entry error.
const maxUSDPer1M = 1000.0

// ModelPrice is one model's price in µUSD per 1,000,000 tokens.
type ModelPrice struct {
	InputUSD  int64 // µUSD per 1M input (prompt) tokens
	OutputUSD int64 // µUSD per 1M output (completion) tokens
}

// Pricing is an immutable price table. Safe for concurrent reads (no mutex —
// the model map is never mutated after Load returns).
type Pricing struct {
	// assumedCompletionTokens is the completion token count credited to a
	// blocked request's saved-cost estimate. Default 0 (conservative: only the
	// prompt cost is credited). It is unexported because the exported accessor is
	// the AssumedCompletionTokens() method below (a Go type cannot have a field
	// and a method with the same name).
	assumedCompletionTokens int
	// PromptEstimationCharsPerToken is the chars-per-token heuristic used to
	// estimate prompt tokens. Default 4.0.
	PromptEstimationCharsPerToken float64

	models map[string]ModelPrice
	def    ModelPrice
}

// PriceFor returns the price for model: exact key, else longest configured key
// that is a prefix of model, else the default. ok=false means "fell back to
// default".
func (p *Pricing) PriceFor(model string) (ModelPrice, bool) {
	if mp, ok := p.models[model]; ok {
		return mp, true
	}
	best := ""
	for key := range p.models {
		if len(key) > len(best) && strings.HasPrefix(model, key) {
			best = key
		}
	}
	if best != "" {
		return p.models[best], true
	}
	return p.def, false
}

// Cost returns the cost in µUSD of a completed call:
// (promptTok*InputUSD + completionTok*OutputUSD) / 1_000_000.
func Cost(mp ModelPrice, promptTok, completionTok int) int64 {
	return (int64(promptTok)*mp.InputUSD + int64(completionTok)*mp.OutputUSD) / MicroUSDPerUSD
}

// SavedCost estimates (µUSD) the spend avoided by blocking this request:
// Cost(PriceFor(ExtractModel(body)), EstimatePromptTokens(body),
// AssumedCompletionTokens). It is an estimate, never an exact charge — the
// request never reached upstream, so there is no real usage.
func (p *Pricing) SavedCost(body []byte) int64 {
	mp, _ := p.PriceFor(ExtractModel(body))
	return Cost(mp, p.EstimatePromptTokens(body), p.assumedCompletionTokens)
}

// AssumedCompletionTokens returns the completion token count credited to a
// blocked request's saved-cost estimate. It exists so *Pricing satisfies the
// PriceSource contract; the registry delegates to the same knob on its current
// table. The backing value is stored unexported because a Go type cannot have a
// field and a method with the same name.
func (p *Pricing) AssumedCompletionTokens() int { return p.assumedCompletionTokens }

// Models returns a shallow copy of the exact-key price map. It exists for boot
// seeding: main seeds the SQLite price book from the static config table (with
// INSERT OR IGNORE) so a DB-first Rebuild preserves the guaranteed seed. A nil
// receiver yields nil.
func (p *Pricing) Models() map[string]ModelPrice {
	if p == nil {
		return nil
	}
	out := make(map[string]ModelPrice, len(p.models))
	for k, v := range p.models {
		out[k] = v
	}
	return out
}

// ExtractModel returns the "model" field of a chat-completions request body,
// "" if absent or unparseable.
func ExtractModel(body []byte) string {
	var v struct {
		Model string `json:"model"`
	}
	if err := json.Unmarshal(body, &v); err != nil {
		return ""
	}
	return v.Model
}

// EstimatePromptTokens estimates token count using the default chars-per-token
// heuristic (4.0). Prefer (*Pricing).EstimatePromptTokens to honor the loaded
// prompt_estimation_chars_per_token knob.
func EstimatePromptTokens(body []byte) int {
	return estimateTokens(len(body), defaultCharsPerToken)
}

// EstimatePromptTokens estimates token count using p's chars-per-token knob
// (prompt_estimation_chars_per_token; default 4.0). For JSON bodies it slightly
// overestimates, which is the safe direction for a saved-cost estimate.
func (p *Pricing) EstimatePromptTokens(body []byte) int {
	return estimateTokens(len(body), p.PromptEstimationCharsPerToken)
}

// estimateTokens counts tokens as ceil(chars / charsPerToken), guarding
// against a zero or negative divisor.
func estimateTokens(chars int, charsPerToken float64) int {
	if charsPerToken <= 0 {
		charsPerToken = defaultCharsPerToken
	}
	if chars <= 0 {
		return 0
	}
	return int(math.Ceil(float64(chars) / charsPerToken))
}
