// Network price-table fetcher: pulls the OpenRouter model catalog, parses each
// entry's prompt/completion pricing strings into ModelPrice, and returns the
// table plus skip diagnostics. This layer is pure I/O — it owns no state and no
// background loop; the Registry decides when to fetch and what to do with the
// result (including treating an empty table as an error, D7).
package pricing

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// defaultEndpoint is the OpenRouter model catalog served when the operator does
// not override it (§10). An empty endpoint means static-only mode (no fetcher).
const defaultEndpoint = "https://openrouter.ai/api/v1/models"

// defaultUserAgent identifies this proxy to the catalog API.
const defaultUserAgent = "finops-proxy/1.0 (openrouter-fetcher)"

// Config is the single, consolidated configuration for the pricing layer. It is
// stdlib-only and carries no dependencies. Zero fields get their documented
// defaults via DefaultConfig (or a defensive normalize inside the constructors);
// the two fields with meaningful zero values are Endpoint ("" ⇒ static-only) and
// RefreshInterval (<=0 ⇒ no background refresh).
type Config struct {
	PricingPath     string        // static baseline file (config/pricing.json)
	Endpoint        string        // "" => static-only mode (no fetcher)
	APIKey          string        // optional Bearer token
	UserAgent       string        // default "finops-proxy/1.0 (openrouter-fetcher)"
	RefreshInterval time.Duration // default 6h; <=0 disables background refresh
	StartupTimeout  time.Duration // default 3s (bootstrap budget)
	HTTPTimeout     time.Duration // default 30s
	BackoffBase     time.Duration // default 10s
	BackoffMax      time.Duration // default 5m
	StaleAfter      time.Duration // default 24h
	HardFailOnEmpty bool          // default true
	MaxBodyBytes    int64         // default 8 MiB
}

// DefaultConfig returns the §10 defaults. Endpoint defaults to the OpenRouter
// catalog; an operator who wants static-only mode overrides it to "".
func DefaultConfig() Config {
	return Config{
		PricingPath:     "config/pricing.json",
		Endpoint:        defaultEndpoint,
		UserAgent:       defaultUserAgent,
		RefreshInterval: 6 * time.Hour,
		StartupTimeout:  3 * time.Second,
		HTTPTimeout:     30 * time.Second,
		BackoffBase:     10 * time.Second,
		BackoffMax:      5 * time.Minute,
		StaleAfter:      24 * time.Hour,
		HardFailOnEmpty: true,
		MaxBodyBytes:    8 << 20,
	}
}

// normalizeConfig fills zero duration/size/string fields with their defaults.
// It deliberately does NOT touch Endpoint ("" means static-only) or
// RefreshInterval (<=0 means no background refresh) — both zero values are
// meaningful and must survive.
func normalizeConfig(cfg Config) Config {
	if cfg.PricingPath == "" {
		cfg.PricingPath = "config/pricing.json"
	}
	if cfg.UserAgent == "" {
		cfg.UserAgent = defaultUserAgent
	}
	if cfg.StartupTimeout <= 0 {
		cfg.StartupTimeout = 3 * time.Second
	}
	if cfg.HTTPTimeout <= 0 {
		cfg.HTTPTimeout = 30 * time.Second
	}
	if cfg.BackoffBase <= 0 {
		cfg.BackoffBase = 10 * time.Second
	}
	if cfg.BackoffMax <= 0 {
		cfg.BackoffMax = 5 * time.Minute
	}
	if cfg.StaleAfter <= 0 {
		cfg.StaleAfter = 24 * time.Hour
	}
	if cfg.MaxBodyBytes <= 0 {
		cfg.MaxBodyBytes = 8 << 20
	}
	return cfg
}

// Fetcher is a stateless HTTP client for the model catalog. It owns no table
// and no loop; callers drive it.
type Fetcher struct {
	cfg    Config
	client *http.Client // one client; Transport keeps idle connections open
}

// NewFetcher builds a Fetcher for cfg. It returns nil in static-only mode
// (cfg.Endpoint == ""), in which case there is nothing to fetch from.
func NewFetcher(cfg Config) *Fetcher {
	if cfg.Endpoint == "" {
		return nil
	}
	cfg = normalizeConfig(cfg)
	return &Fetcher{
		cfg: cfg,
		client: &http.Client{
			Timeout: cfg.HTTPTimeout,
			Transport: &http.Transport{
				MaxIdleConnsPerHost: 2,
			},
		},
	}
}

// FetchResult is the outcome of one catalog fetch: the priced models, an
// informative (seed) default, the models skipped for being unpriceable, and
// when the table was pulled.
type FetchResult struct {
	Models  map[string]ModelPrice
	Default ModelPrice // informative seed default (the registry keeps base's)
	Skipped []SkippedModel
	Updated time.Time
}

// SkippedModel is one catalog entry dropped for a non-fatal reason (empty id or
// unpriceable pricing). It is diagnostic only — a bad entry never fails a fetch.
type SkippedModel struct {
	ID, Reason string
}

// ErrEmptyTable marks a fetch that succeeded syntactically but produced no
// priced models. The registry treats it as a refresh failure (D7) rather than
// swapping in an empty table.
var ErrEmptyTable = errors.New("pricing: fetch succeeded but produced no priced models")

// RefreshError describes a failed table refresh: which endpoint, the HTTP status
// (0 when no HTTP response was received — network/DNS/timeout), a short response
// snippet, and the underlying error. Status lets a caller tell a permanent
// 401/403 from a transient 5xx or network failure.
type RefreshError struct {
	Endpoint string
	Status   int // 0 if no HTTP response
	Snippet  string
	Err      error
}

func (e *RefreshError) Error() string {
	switch {
	case e == nil:
		return "pricing: nil refresh error"
	case e.Status != 0 && e.Snippet != "":
		return fmt.Sprintf("pricing: refresh %s: http %d: %s", e.Endpoint, e.Status, e.Snippet)
	case e.Status != 0:
		return fmt.Sprintf("pricing: refresh %s: http %d", e.Endpoint, e.Status)
	default:
		return fmt.Sprintf("pricing: refresh %s: %v", e.Endpoint, e.Err)
	}
}

// Unwrap exposes the underlying error for errors.Is/As.
func (e *RefreshError) Unwrap() error { return e.Err }

// FetchModels fetches the catalog and returns the priced models, dropping
// unpriceable entries silently. endpoint == "" falls back to f.cfg.Endpoint.
// An empty map is not an error here — the registry decides (D7).
func (f *Fetcher) FetchModels(ctx context.Context, endpoint string) (map[string]ModelPrice, error) {
	return f.fetchModels(ctx, endpoint, nil)
}

// Fetch is FetchModels plus skip diagnostics and an Updated timestamp.
func (f *Fetcher) Fetch(ctx context.Context) (FetchResult, error) {
	var skipped []SkippedModel
	models, err := f.fetchModels(ctx, f.cfg.Endpoint, &skipped)
	if err != nil {
		return FetchResult{}, err
	}
	return FetchResult{
		Models:  models,
		Skipped: skipped,
		Updated: time.Now(),
	}, nil
}

// modelsResponse is the wire form of GET /api/v1/models (the only fields this
// layer reads; cache/audio/overrides etc. are deliberately ignored).
type modelsResponse struct {
	Data []struct {
		ID      string `json:"id"`
		Pricing struct {
			Prompt     string `json:"prompt"`
			Completion string `json:"completion"`
		} `json:"pricing"`
	} `json:"data"`
}

// fetchModels is the shared implementation behind FetchModels and Fetch.
// skipped, when non-nil, collects every entry dropped for a non-fatal reason.
func (f *Fetcher) fetchModels(ctx context.Context, endpoint string, skipped *[]SkippedModel) (map[string]ModelPrice, error) {
	if endpoint == "" {
		endpoint = f.cfg.Endpoint
	}
	if endpoint == "" {
		return nil, errors.New("pricing: no catalog endpoint configured")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, &RefreshError{Endpoint: endpoint, Err: err}
	}
	req.Header.Set("Accept", "application/json")
	if f.cfg.UserAgent != "" {
		req.Header.Set("User-Agent", f.cfg.UserAgent)
	}
	if f.cfg.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+f.cfg.APIKey)
	}

	resp, err := f.client.Do(req)
	if err != nil {
		// Transport-level failure (DNS, dial, timeout): no HTTP response.
		return nil, &RefreshError{Endpoint: endpoint, Err: err}
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, &RefreshError{
			Endpoint: endpoint,
			Status:   resp.StatusCode,
			Snippet:  readSnippet(resp.Body, 512),
		}
	}

	body, err := readCappedBody(resp.Body, f.cfg.MaxBodyBytes)
	if err != nil {
		return nil, &RefreshError{Endpoint: endpoint, Status: resp.StatusCode, Err: err}
	}
	var parsed modelsResponse
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, &RefreshError{
			Endpoint: endpoint,
			Status:   resp.StatusCode,
			Snippet:  snippetFromBody(body),
			Err:      err,
		}
	}

	models := make(map[string]ModelPrice)
	for _, m := range parsed.Data {
		if m.ID == "" {
			if skipped != nil {
				*skipped = append(*skipped, SkippedModel{ID: "", Reason: "empty id"})
			}
			continue
		}
		mp, ok := modelPrice(m.Pricing.Prompt, m.Pricing.Completion)
		if !ok {
			if skipped != nil {
				*skipped = append(*skipped, SkippedModel{
					ID:     m.ID,
					Reason: fmt.Sprintf("unpriceable pricing prompt=%q completion=%q", m.Pricing.Prompt, m.Pricing.Completion),
				})
			}
			continue
		}
		// "~"-prefixed alias/router ids are ordinary exact-match keys; the last
		// duplicate id wins.
		models[m.ID] = mp
	}
	return models, nil
}

// readSnippet reads at most n bytes of a body for an error message.
func readSnippet(r io.Reader, n int64) string {
	b, _ := io.ReadAll(io.LimitReader(r, n))
	return strings.TrimSpace(string(b))
}

// snippetFromBody trims a body down for an error message.
func snippetFromBody(b []byte) string {
	const max = 512
	if len(b) > max {
		b = b[:max]
	}
	return strings.TrimSpace(string(b))
}

// readCappedBody reads the whole body but refuses to buffer more than max
// bytes, so a runaway catalog cannot exhaust memory.
func readCappedBody(r io.Reader, max int64) ([]byte, error) {
	if max <= 0 {
		max = 8 << 20
	}
	lr := &io.LimitedReader{R: r, N: max + 1}
	body, err := io.ReadAll(lr)
	if err != nil {
		return nil, err
	}
	if int64(len(body)) > max {
		return nil, fmt.Errorf("catalog response exceeds %d bytes", max)
	}
	return body, nil
}

// toMicroUSDPer1M converts a USD-per-1M-token float to integer µUSD, rejecting
// NaN/Inf, negatives, and values above the sanity cap. It is the single
// float→µUSD step shared by load.go (static config) and fetcher.go (remote
// table), so both enforce exactly the same numeric bounds.
func toMicroUSDPer1M(v float64) (int64, bool) {
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return 0, false
	}
	if v < 0 {
		return 0, false
	}
	if v > maxUSDPer1M {
		return 0, false
	}
	return int64(math.Round(v * MicroUSDPerUSD)), true
}

// parsePriceString converts one catalog pricing string into USD per 1M tokens,
// or ok=false when the string has no usable numeric price. It handles the bare
// decimal form ("0.000000834", USD per token) and the defensive display form
// ("$0.27/M input tokens", USD per 1M) via the §4 grammar: a "/M"-family scale
// means the value is already per-1M; its absence means multiply by 1e6.
func parsePriceString(s string) (float64, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, false
	}
	switch s {
	case "-1", "per_request", "per_request_limits":
		return 0, false // variable routing / per-request pricing: indeterminate
	}
	s = strings.TrimPrefix(s, "$")
	s = strings.ReplaceAll(s, ",", "") // strip thousands separators
	s = strings.TrimSpace(s)

	v, rest, ok := splitFloatPrefix(s)
	if !ok {
		return 0, false
	}
	if hasPerMillionScale(rest) {
		return v, true // already USD per 1M
	}
	return v * 1_000_000, true // USD per token → USD per 1M
}

// hasPerMillionScale reports whether the suffix after the numeric prefix marks
// the price as already USD-per-1M-tokens (§4.2 unit grammar).
func hasPerMillionScale(rest string) bool {
	rest = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(rest), "/"))
	rest = strings.ToLower(rest)
	for _, p := range []string{"1m", "million", "mtokens", "mtok", "m"} {
		if strings.HasPrefix(rest, p) {
			return true
		}
	}
	return false
}

// splitFloatPrefix splits s into its longest valid Go floating-point prefix and
// the remaining suffix. ok=false when s has no numeric prefix at all (e.g. a
// bare word). A hand-rolled scanner is required because strconv.ParseFloat
// rejects a string with trailing junk instead of returning the parsed prefix.
func splitFloatPrefix(s string) (v float64, rest string, ok bool) {
	i, n := 0, len(s)
	if i < n && (s[i] == '+' || s[i] == '-') {
		i++
	}
	digits := false
	for i < n && s[i] >= '0' && s[i] <= '9' {
		i++
		digits = true
	}
	if i < n && s[i] == '.' {
		dot := i
		i++
		for i < n && s[i] >= '0' && s[i] <= '9' {
			i++
			digits = true
		}
		// A lone "." with no digits on either side is not a float literal.
		if i == dot+1 && !digits {
			i = dot
		}
	}
	if !digits {
		return 0, "", false
	}
	if i < n && (s[i] == 'e' || s[i] == 'E') {
		j := i + 1
		if j < n && (s[j] == '+' || s[j] == '-') {
			j++
		}
		if j < n && s[j] >= '0' && s[j] <= '9' {
			for j < n && s[j] >= '0' && s[j] <= '9' {
				j++
			}
			i = j
		}
		// A trailing "e" with no exponent digits is not part of the float; it
		// stays in rest.
	}
	var err error
	v, err = strconv.ParseFloat(s[:i], 64)
	if err != nil {
		// The prefix is syntactically valid, so this is a range error (e.g.
		// "1e999" → +Inf); treat as unparseable.
		return 0, "", false
	}
	return v, s[i:], true
}

// modelPrice converts the prompt/completion pricing strings of one catalog
// entry into a ModelPrice. Either side unpriceable ⇒ the whole model is dropped
// (§4.4): a partial price is worse than falling through to prefix/default.
func modelPrice(promptStr, completionStr string) (ModelPrice, bool) {
	pUSD, ok := parsePriceString(promptStr)
	if !ok {
		return ModelPrice{}, false
	}
	cUSD, ok := parsePriceString(completionStr)
	if !ok {
		return ModelPrice{}, false
	}
	pUS, ok := toMicroUSDPer1M(pUSD)
	if !ok {
		return ModelPrice{}, false
	}
	cUS, ok := toMicroUSDPer1M(cUSD)
	if !ok {
		return ModelPrice{}, false
	}
	return ModelPrice{InputUSD: pUS, OutputUSD: cUS}, true
}
