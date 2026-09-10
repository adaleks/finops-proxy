package pricing

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
)

// priceJSON is the on-disk form of a single model price in config/pricing.json.
// Dollar amounts are USD per 1M tokens; pointers distinguish an absent field
// from an explicit zero.
type priceJSON struct {
	InputUSD  *float64 `json:"input_usd_per_1m"`
	OutputUSD *float64 `json:"output_usd_per_1m"`
}

// estimationJSON is the optional tuning block of config/pricing.json.
type estimationJSON struct {
	AssumedCompletionTokens       *int     `json:"assumed_completion_tokens"`
	PromptEstimationCharsPerToken *float64 `json:"prompt_estimation_chars_per_token"`
}

// pricingJSON is the full on-disk document of config/pricing.json.
type pricingJSON struct {
	Version          *int                 `json:"version"`
	Currency         string               `json:"currency"`
	PerMillionTokens *bool                `json:"per_million_tokens"`
	Estimation       estimationJSON       `json:"estimation"`
	Default          *priceJSON           `json:"default"`
	Models           map[string]priceJSON `json:"models"`
}

// Load reads, validates and normalizes config/pricing.json. It errors on any
// invalid entry — a proxy that cannot price its traffic must not start
// silently with a wrong table.
func Load(path string) (*Pricing, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var raw pricingJSON
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("pricing %s: %w", path, err)
	}

	if raw.Version != nil && *raw.Version != 1 {
		return nil, fmt.Errorf("pricing %s: unsupported version %d (want 1)", path, *raw.Version)
	}
	if raw.Currency != "USD" {
		return nil, fmt.Errorf("pricing %s: currency %q (want USD)", path, raw.Currency)
	}
	if raw.PerMillionTokens == nil || !*raw.PerMillionTokens {
		return nil, fmt.Errorf("pricing %s: per_million_tokens must be true", path)
	}
	if raw.Default == nil {
		return nil, fmt.Errorf("pricing %s: default price entry is required", path)
	}

	p := &Pricing{
		assumedCompletionTokens:       0,
		PromptEstimationCharsPerToken: defaultCharsPerToken,
		models:                        make(map[string]ModelPrice, len(raw.Models)),
	}
	if raw.Estimation.AssumedCompletionTokens != nil {
		if *raw.Estimation.AssumedCompletionTokens < 0 {
			return nil, fmt.Errorf("pricing %s: negative assumed_completion_tokens %d", path, *raw.Estimation.AssumedCompletionTokens)
		}
		p.assumedCompletionTokens = *raw.Estimation.AssumedCompletionTokens
	}
	if raw.Estimation.PromptEstimationCharsPerToken != nil {
		f := *raw.Estimation.PromptEstimationCharsPerToken
		if math.IsNaN(f) || math.IsInf(f, 0) || f <= 0 {
			return nil, fmt.Errorf("pricing %s: invalid prompt_estimation_chars_per_token %v", path, f)
		}
		p.PromptEstimationCharsPerToken = f
	}

	def, err := normalizePrice(raw.Default, path, "default")
	if err != nil {
		return nil, err
	}
	p.def = def

	for key, m := range raw.Models {
		if key == "" {
			return nil, fmt.Errorf("pricing %s: empty model key", path)
		}
		mp, err := normalizePrice(&m, path, "model "+key)
		if err != nil {
			return nil, err
		}
		p.models[key] = mp
	}

	return p, nil
}

// normalizePrice validates one price entry and converts its USD-per-1M floats
// to integer µUSD-per-1M. Rejects negative, NaN/Inf, and absurdly large values.
func normalizePrice(m *priceJSON, path, what string) (ModelPrice, error) {
	in, err := normalizeDollar(m.InputUSD, path, what+" input_usd_per_1m")
	if err != nil {
		return ModelPrice{}, err
	}
	out, err := normalizeDollar(m.OutputUSD, path, what+" output_usd_per_1m")
	if err != nil {
		return ModelPrice{}, err
	}
	return ModelPrice{InputUSD: in, OutputUSD: out}, nil
}

// normalizeDollar converts one float dollar price to integer µUSD, rejecting
// NaN/Inf, negatives, and values above the sanity cap. The numeric validation
// and rounding live in the shared toMicroUSDPer1M (used by the fetcher too), so
// the static file and the remote catalog enforce identical bounds; the error
// messages here stay load-specific.
func normalizeDollar(v *float64, path, what string) (int64, error) {
	if v == nil {
		return 0, fmt.Errorf("pricing %s: %s is required", path, what)
	}
	f := *v
	usd, ok := toMicroUSDPer1M(f)
	if !ok {
		switch {
		case math.IsNaN(f) || math.IsInf(f, 0):
			return 0, fmt.Errorf("pricing %s: %s is NaN/Inf", path, what)
		case f < 0:
			return 0, fmt.Errorf("pricing %s: %s is negative (%v)", path, what, f)
		default:
			return 0, fmt.Errorf("pricing %s: %s (%v) exceeds sanity cap %v USD/1M", path, what, f, maxUSDPer1M)
		}
	}
	return usd, nil
}
