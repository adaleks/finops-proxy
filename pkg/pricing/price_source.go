package pricing

// PriceSource is the single contract the proxy's cost layer consumes. Both the
// static *Pricing and the dynamic *Registry satisfy it, so the handler never
// knows — or needs to know — where the table came from.
//
// Implementations MUST be safe for concurrent reads and MUST NOT return an
// empty or partial table: every model resolves (exact key → longest prefix →
// default), and no read ever blocks on the network.
type PriceSource interface {
	// PriceFor resolves one model's price (exact key, else longest configured
	// prefix, else the default). ok=false means "fell back to default".
	PriceFor(model string) (ModelPrice, bool)
	// EstimatePromptTokens estimates the prompt token count of a request body
	// using the table's chars-per-token knob.
	EstimatePromptTokens(body []byte) int
	// SavedCost estimates the µUSD spend avoided by blocking this request.
	SavedCost(body []byte) int64
	// AssumedCompletionTokens is the completion token count credited to a
	// blocked request's saved-cost estimate.
	AssumedCompletionTokens() int
}

// Compile-time assertions: both implementations expose the full contract.
var _ PriceSource = (*Pricing)(nil)
var _ PriceSource = (*Registry)(nil)
