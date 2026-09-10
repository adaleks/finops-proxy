package pricing

// Build constructs an immutable serving *Pricing from a persisted price book:
// the exact-key model map plus the shared default price and estimation knobs
// that a remote catalog or database overlay never supplies. It is the rebuild
// seam the enterprise module and the reference stores use to turn a persisted
// price book (SQLite model_prices rows, or any in-memory map) into a live
// serving table.
//
// models is used as-is (not copied): the caller must not mutate it after the
// call, mirroring the immutability contract of *Pricing. promptEstimationCharsPerToken
// is clamped to the default (4.0) when non-positive.
func Build(models map[string]ModelPrice, def ModelPrice, assumedCompletionTokens int, promptEstimationCharsPerToken float64) *Pricing {
	if promptEstimationCharsPerToken <= 0 {
		promptEstimationCharsPerToken = defaultCharsPerToken
	}
	if models == nil {
		models = make(map[string]ModelPrice)
	}
	return &Pricing{
		assumedCompletionTokens:       assumedCompletionTokens,
		PromptEstimationCharsPerToken: promptEstimationCharsPerToken,
		models:                        models,
		def:                           def,
	}
}

// Default returns the fallback price used when a model resolves to neither an
// exact key nor a configured prefix. It pairs with Build so a caller can carry a
// table's default price and estimation knobs across a rebuild. A nil receiver
// yields the zero price.
func (p *Pricing) Default() ModelPrice {
	if p == nil {
		return ModelPrice{}
	}
	return p.def
}
