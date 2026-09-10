package pricing

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// validPricingJSON is a self-contained, hermetic price table used by most
// tests. It mirrors the shape of config/pricing.json (version/currency/
// per_million_tokens/estimation/default/models) but only carries the models the
// tests exercise, so a change to the real config file can never affect results.
const validPricingJSON = `{
  "version": 1,
  "currency": "USD",
  "per_million_tokens": true,
  "estimation": {
    "assumed_completion_tokens": 0,
    "prompt_estimation_chars_per_token": 4.0
  },
  "default": { "input_usd_per_1m": 2.50, "output_usd_per_1m": 10.00 },
  "models": {
    "gpt-4o":     { "input_usd_per_1m": 2.50, "output_usd_per_1m": 10.00 },
    "gpt-4o-mini": { "input_usd_per_1m": 0.15, "output_usd_per_1m": 0.60 }
  }
}`

// writePricing writes contents to a fresh temp file and returns its path.
func writePricing(t *testing.T, contents string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "pricing.json")
	if err := os.WriteFile(path, []byte(contents), 0o644); err != nil {
		t.Fatalf("write pricing.json: %v", err)
	}
	return path
}

// loadTestPricing loads the valid fixture table, failing the test on error.
func loadTestPricing(t *testing.T) *Pricing {
	t.Helper()
	p, err := Load(writePricing(t, validPricingJSON))
	if err != nil {
		t.Fatalf("pricing.Load(valid fixture): %v", err)
	}
	return p
}

func TestLoadAndPriceForExactMatch(t *testing.T) {
	p := loadTestPricing(t)

	tests := []struct {
		model string
		want  ModelPrice
	}{
		{model: "gpt-4o", want: ModelPrice{InputUSD: 2_500_000, OutputUSD: 10_000_000}},
		{model: "gpt-4o-mini", want: ModelPrice{InputUSD: 150_000, OutputUSD: 600_000}},
	}
	for _, tt := range tests {
		got, ok := p.PriceFor(tt.model)
		if !ok {
			t.Errorf("PriceFor(%q): ok = false, want true (exact key)", tt.model)
		}
		if got != tt.want {
			t.Errorf("PriceFor(%q) = %+v, want %+v", tt.model, got, tt.want)
		}
	}
}

func TestPriceForLongestPrefix(t *testing.T) {
	p := loadTestPricing(t)

	tests := []struct {
		model string
		want  ModelPrice
	}{
		// Date-suffixed alias resolves to the longest configured prefix.
		{model: "gpt-4o-2024-08-06", want: ModelPrice{InputUSD: 2_500_000, OutputUSD: 10_000_000}},
		// gpt-4o-mini is also a prefix of this string but gpt-4o-mini is longer
		// and must win — never gpt-4o.
		{model: "gpt-4o-mini-2024-07-18", want: ModelPrice{InputUSD: 150_000, OutputUSD: 600_000}},
	}
	for _, tt := range tests {
		got, ok := p.PriceFor(tt.model)
		if !ok {
			t.Errorf("PriceFor(%q): ok = false, want true (longest-prefix match)", tt.model)
		}
		if got != tt.want {
			t.Errorf("PriceFor(%q) = %+v, want %+v", tt.model, got, tt.want)
		}
	}
}

func TestPriceForUnknownFallsBackToDefault(t *testing.T) {
	p := loadTestPricing(t)

	for _, model := range []string{"unknown-model-xyz", "", "gpt-3.5-turbo"} {
		got, ok := p.PriceFor(model)
		if ok {
			t.Errorf("PriceFor(%q): ok = true, want false (default fallback)", model)
		}
		want := ModelPrice{InputUSD: 2_500_000, OutputUSD: 10_000_000}
		if got != want {
			t.Errorf("PriceFor(%q) = %+v, want default %+v", model, got, want)
		}
	}
}

func TestCost(t *testing.T) {
	mp := ModelPrice{InputUSD: 2_500_000, OutputUSD: 10_000_000}

	// (1000×2_500_000 + 500×10_000_000) / 1_000_000 = 7_500 µUSD = $0.0075.
	if got := Cost(mp, 1000, 500); got != 7_500 {
		t.Errorf("Cost(gpt-4o, 1000, 500) = %d, want 7500", got)
	}
	// Zero tokens cost nothing.
	if got := Cost(mp, 0, 0); got != 0 {
		t.Errorf("Cost(gpt-4o, 0, 0) = %d, want 0", got)
	}
	// Pure completion: (0 + 500×10_000_000)/1M = 5000.
	if got := Cost(mp, 0, 500); got != 5_000 {
		t.Errorf("Cost(gpt-4o, 0, 500) = %d, want 5000", got)
	}
}

func TestSavedCost(t *testing.T) {
	p := loadTestPricing(t)
	body := []byte(`{"model":"gpt-4o","messages":[{"role":"user","content":"Hi there"}]}`)

	mp, _ := p.PriceFor("gpt-4o")
	want := Cost(mp, EstimatePromptTokens(body), 0) // AssumedCompletionTokens defaults to 0
	if got := p.SavedCost(body); got != want {
		t.Errorf("SavedCost(body) = %d, want %d (= Cost(gpt-4o, promptEstimate, 0))", got, want)
	}
}

func TestExtractModel(t *testing.T) {
	tests := []struct {
		body string
		want string
	}{
		{body: `{"model":"gpt-4o","messages":[]}`, want: "gpt-4o"},
		{body: `{"model":"gpt-4o"}`, want: "gpt-4o"},
		{body: `{}`, want: ""},
		{body: `not json at all`, want: ""},
		{body: `{"model":123}`, want: ""}, // type mismatch → unparseable → ""
	}
	for _, tt := range tests {
		if got := ExtractModel([]byte(tt.body)); got != tt.want {
			t.Errorf("ExtractModel(%q) = %q, want %q", tt.body, got, tt.want)
		}
	}
}

func TestEstimatePromptTokens(t *testing.T) {
	tests := []struct {
		body string
		want int
	}{
		{body: "", want: 0},
		{body: "1234", want: 1},                    // 4/4 = 1
		{body: "12345", want: 2},                   // ceil(5/4) = 2
		{body: strings.Repeat("a", 99), want: 25},  // ceil(99/4) = 25
		{body: strings.Repeat("a", 100), want: 25}, // 100/4 = 25
		{body: strings.Repeat("a", 101), want: 26}, // ceil(101/4) = 26
	}
	for _, tt := range tests {
		if got := EstimatePromptTokens([]byte(tt.body)); got != tt.want {
			t.Errorf("EstimatePromptTokens(len=%d) = %d, want %d", len(tt.body), got, tt.want)
		}
	}
}

func TestLoadTuningBlock(t *testing.T) {
	const json = `{
	  "version": 1,
	  "currency": "USD",
	  "per_million_tokens": true,
	  "estimation": {
	    "assumed_completion_tokens": 50,
	    "prompt_estimation_chars_per_token": 5.0
	  },
	  "default": { "input_usd_per_1m": 1.0, "output_usd_per_1m": 2.0 },
	  "models": {}
	}`
	p, err := Load(writePricing(t, json))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if p.AssumedCompletionTokens() != 50 {
		t.Errorf("AssumedCompletionTokens() = %d, want 50", p.AssumedCompletionTokens())
	}
	if p.PromptEstimationCharsPerToken != 5.0 {
		t.Errorf("PromptEstimationCharsPerToken = %v, want 5.0", p.PromptEstimationCharsPerToken)
	}
}

// TestLoadValidationErrors is table-driven over every invalid input Load must
// reject: a proxy that cannot price its traffic must not start silently.
func TestLoadValidationErrors(t *testing.T) {
	tests := []struct {
		name string
		json string
	}{
		{name: "missing default", json: `{"version":1,"currency":"USD","per_million_tokens":true,"models":{"gpt-4o":{"input_usd_per_1m":2.5,"output_usd_per_1m":10}}}`},
		{name: "bad currency", json: `{"version":1,"currency":"EUR","per_million_tokens":true,"default":{"input_usd_per_1m":2.5,"output_usd_per_1m":10}}`},
		{name: "per_million_tokens false", json: `{"version":1,"currency":"USD","per_million_tokens":false,"default":{"input_usd_per_1m":2.5,"output_usd_per_1m":10}}`},
		{name: "per_million_tokens absent", json: `{"version":1,"currency":"USD","default":{"input_usd_per_1m":2.5,"output_usd_per_1m":10}}`},
		{name: "unsupported version", json: `{"version":2,"currency":"USD","per_million_tokens":true,"default":{"input_usd_per_1m":2.5,"output_usd_per_1m":10}}`},
		{name: "negative default input", json: `{"version":1,"currency":"USD","per_million_tokens":true,"default":{"input_usd_per_1m":-2.5,"output_usd_per_1m":10}}`},
		{name: "negative model output", json: `{"version":1,"currency":"USD","per_million_tokens":true,"default":{"input_usd_per_1m":2.5,"output_usd_per_1m":10},"models":{"gpt-4o":{"input_usd_per_1m":2.5,"output_usd_per_1m":-10}}}`},
		{name: "price over sanity cap", json: `{"version":1,"currency":"USD","per_million_tokens":true,"default":{"input_usd_per_1m":2000,"output_usd_per_1m":10}}`},
		{name: "NaN price", json: `{"version":1,"currency":"USD","per_million_tokens":true,"default":{"input_usd_per_1m":NaN,"output_usd_per_1m":10}}`},
		{name: "Inf price", json: `{"version":1,"currency":"USD","per_million_tokens":true,"default":{"input_usd_per_1m":2.5,"output_usd_per_1m":Infinity}}`},
		{name: "missing input field", json: `{"version":1,"currency":"USD","per_million_tokens":true,"default":{"output_usd_per_1m":10}}`},
		{name: "empty model key", json: `{"version":1,"currency":"USD","per_million_tokens":true,"default":{"input_usd_per_1m":2.5,"output_usd_per_1m":10},"models":{"":{"input_usd_per_1m":2.5,"output_usd_per_1m":10}}}`},
		{name: "negative assumed_completion_tokens", json: `{"version":1,"currency":"USD","per_million_tokens":true,"estimation":{"assumed_completion_tokens":-1},"default":{"input_usd_per_1m":2.5,"output_usd_per_1m":10}}`},
		{name: "invalid chars_per_token", json: `{"version":1,"currency":"USD","per_million_tokens":true,"estimation":{"prompt_estimation_chars_per_token":0},"default":{"input_usd_per_1m":2.5,"output_usd_per_1m":10}}`},
		{name: "malformed json", json: `{not json`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := Load(writePricing(t, tt.json)); err == nil {
				t.Errorf("Load(%s) = nil error, want error", tt.name)
			}
		})
	}
}

// TestLoadMissingFile verifies Load surfaces a filesystem error for a path that
// does not exist.
func TestLoadMissingFile(t *testing.T) {
	if _, err := Load(filepath.Join(t.TempDir(), "does-not-exist.json")); err == nil {
		t.Error("Load(missing file) = nil error, want error")
	}
}
