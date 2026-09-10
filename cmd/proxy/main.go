// Command proxy is the lean, Apache-2.0 FinOps Proxy engine.
//
// It runs the circuit-breaker reverse proxy from the public core module: a
// HashDetector for loop detection, a static (or remotely refreshed) price
// table, an in-memory or SQLite store, per-key budgets from an optional keys
// file, and standardized JSON logging to stdout. No enterprise dependency is
// pulled in — this is the whole open-source core in one binary.
//
// Usage:
//
//	proxy -upstream http://localhost:11434/v1
//	proxy -upstream https://api.openai.com -db ./finops.db -keys ./keys.json
//
// The proxy answers GET /healthz locally; every other request is hashed,
// loop-checked, budget-checked, and forwarded upstream.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/adaleks/finops-proxy/internal/logging"
	"github.com/adaleks/finops-proxy/internal/store/memory"
	"github.com/adaleks/finops-proxy/internal/store/sqlite"
	"github.com/adaleks/finops-proxy/pkg/circuitbreaker"
	"github.com/adaleks/finops-proxy/pkg/logstream"
	"github.com/adaleks/finops-proxy/pkg/pricing"
	"github.com/adaleks/finops-proxy/pkg/proxy"
)

// version is the core module build version. It is a var (not const) so it can be
// overridden at build time via -ldflags "-X main.version=<semver>" — the
// GoReleaser release workflow does exactly that.
var version = "finops-proxy core dev"

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "proxy: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	var (
		addr          = flag.String("addr", ":8080", "listen address")
		upstreamStr   = flag.String("upstream", "http://localhost:11434/v1", "upstream LLM API base URL")
		ttl           = flag.Duration("ttl", 5*time.Minute, "loop-detection TTL window")
		limit         = flag.Int("limit", 2, "loop-detection repeat threshold")
		pricingPath   = flag.String("pricing", "config/pricing.json", "static price table path")
		pricingURL    = flag.String("pricing-url", "", "remote catalog endpoint (empty = static-only)")
		dbPath        = flag.String("db", "", "SQLite path (empty = in-memory store)")
		keysPath      = flag.String("keys", "", "JSON keys file for per-key budgets (empty = no auth)")
		dailyBudget   = flag.Int64("daily-budget-micro", 0, "global daily budget in µUSD (0 = disabled)")
		thresholds    = flag.String("budget-thresholds", "50,80,100", "daily budget alert thresholds (comma-separated %)")
		metricsFlag   = flag.Bool("metrics", true, "expose GET /v1/metrics JSON observability endpoint")
		dashboardFlag = flag.Bool("dashboard", true, "expose the embedded GET /dashboard web UI")
		dynamic       = flag.Bool("dynamic", false, "enable all dynamic loop detectors (shorthand for -structural -tool-loop -velocity)")
		structural    = flag.Bool("structural", false, "enable structural near-duplicate loop detection")
		toolLoop      = flag.Bool("tool-loop", false, "enable tool-cycle loop detection")
		velocity      = flag.Bool("velocity", false, "enable token-velocity loop detection")
		showVersion   = flag.Bool("version", false, "print version and exit")
	)
	flag.Parse()

	if *showVersion {
		fmt.Fprintln(os.Stdout, version)
		return nil
	}

	upstream, err := url.Parse(*upstreamStr)
	if err != nil {
		return fmt.Errorf("parse -upstream: %w", err)
	}

	// Price source: static table, optionally wrapped in a self-refreshing
	// registry when -pricing-url is set. Load is fatal on any bad entry — a
	// proxy that cannot price its traffic must not start silently.
	base, err := pricing.Load(*pricingPath)
	if err != nil {
		return fmt.Errorf("load pricing: %w", err)
	}
	var priceSource pricing.PriceSource = base
	var registry *pricing.Registry
	if *pricingURL != "" {
		cfg := pricing.DefaultConfig()
		cfg.PricingPath = *pricingPath
		cfg.Endpoint = *pricingURL
		cfg.RefreshInterval = 6 * time.Hour
		registry = pricing.NewRegistry(base, pricing.NewFetcher(cfg), cfg)
		registry.Bootstrap(context.Background())
		registry.Start(context.Background())
		defer registry.Stop()
		priceSource = registry
	}

	// Detector: SHA-256 sliding-window hash matching, optionally composed with
	// dynamic detectors behind a Pipeline. Each detector is enabled independently
	// (-structural, -tool-loop, -velocity) or all at once (-dynamic). The
	// pipeline runs exact-hash first (unchanged behavior), then the enabled
	// dynamic checks in order.
	exact := circuitbreaker.NewHashDetector(*ttl, *limit)
	var det circuitbreaker.Detector = exact
	var pipelineOpts []circuitbreaker.PipelineOption
	if *dynamic || *structural {
		pipelineOpts = append(pipelineOpts, circuitbreaker.WithStructural(circuitbreaker.NewStructuralHasher(circuitbreaker.StructuralConfig{})))
	}
	if *dynamic || *toolLoop {
		pipelineOpts = append(pipelineOpts, circuitbreaker.WithTools(circuitbreaker.NewToolMatcher(circuitbreaker.ToolConfig{})))
	}
	if *dynamic || *velocity {
		pipelineOpts = append(pipelineOpts, circuitbreaker.WithVelocity(circuitbreaker.NewVelocityBreaker(circuitbreaker.VelocityConfig{})))
	}
	if len(pipelineOpts) > 0 {
		pipelineOpts = append(pipelineOpts, circuitbreaker.WithTokenEstimator(priceSource.EstimatePromptTokens))
		det = circuitbreaker.NewPipeline(exact, pipelineOpts...)
	}

	// Observability registry: feeds /v1/metrics and the embedded /dashboard.
	metrics := proxy.NewMetrics()

	// Store: SQLite when -db is set (core tables only), else in-memory. The
	// store doubles as the caller store when it is the in-memory one.
	var (
		store       proxy.Store
		callerStore proxy.CallerStore
		sqliteDB    *sqlite.DB
	)
	if *dbPath != "" {
		sqliteDB, err = sqlite.Open(*dbPath)
		if err != nil {
			return err
		}
		defer sqliteDB.Close()
		store = sqliteDB
		callerStore = memory.NewKeyStore()
		// Seed the persisted price book from the static config (INSERT OR IGNORE).
		if err := sqliteDB.SeedModelPrices(context.Background(), priceRowsFrom(base)); err != nil {
			return fmt.Errorf("seed model prices: %w", err)
		}
	} else {
		mem := memory.New()
		store = mem
		callerStore = mem
	}

	// Per-key budgets from an optional keys file.
	if *keysPath != "" {
		if err := loadKeys(*keysPath, callerStore); err != nil {
			return err
		}
	}

	// Runtime settings: read the loop threshold and cost-alert threshold once at
	// startup, apply the threshold to the detector, and expose the cost-alert
	// threshold on the hot path.
	var threshold atomic.Int64
	if row, err := store.GetSettings(context.Background()); err == nil {
		exact.SetLimit(row.LoopThresholdCount)
		threshold.Store(row.CostAlertThresholdMicro)
	}
	costReader := costThresholdReader{&threshold}

	// JSON logging to stdout: alert events directly, plus the full live log
	// stream through the hub.
	logger := logging.New(os.Stdout)
	hub := logstream.NewHub(nil)
	logger.Subscribe(hub)

	var opts []proxy.HandlerOption
	opts = append(opts,
		proxy.WithStore(store),
		proxy.WithNotifier(logger),
		proxy.WithLogStream(hub),
		proxy.WithCostThreshold(costReader),
		proxy.WithMetrics(metrics),
	)
	if *keysPath != "" {
		opts = append(opts, proxy.WithAuth(proxy.NewAuthenticator(callerStore)))
	}
	if *dailyBudget > 0 {
		opts = append(opts, proxy.WithBudget(proxy.BudgetConfig{
			DailyBudgetMicro: *dailyBudget,
			ThresholdPct:     parseThresholds(*thresholds),
		}))
	}

	handler := proxy.NewHandler(upstream, det, priceSource, opts...)

	root := http.NewServeMux()
	root.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, `{"status":"ok"}`)
	})
	if *metricsFlag {
		root.Handle("GET /v1/metrics", proxy.MetricsHandler(metrics, det))
	}
	if *dashboardFlag {
		root.Handle("GET /dashboard", proxy.DashboardHandler())
	}
	root.Handle("/", handler)

	log.Printf("finops-proxy %s listening on %s, upstream %s, store %s", version, *addr, upstream, storeKind(*dbPath))
	return http.ListenAndServe(*addr, root)
}

// storeKind is a one-word store label for the startup log.
func storeKind(dbPath string) string {
	if dbPath != "" {
		return "sqlite(" + dbPath + ")"
	}
	return "memory"
}

// priceRowsFrom converts the static price table to the store's seed rows.
func priceRowsFrom(p *pricing.Pricing) []sqlite.PriceRow {
	models := p.Models()
	rows := make([]sqlite.PriceRow, 0, len(models))
	for model, mp := range models {
		rows = append(rows, sqlite.PriceRow{
			Model:          model,
			InputMicroUSD:  mp.InputUSD,
			OutputMicroUSD: mp.OutputUSD,
			Source:         sqlite.PriceSourceSeed,
		})
	}
	return rows
}

// keyEntry is one entry of the optional keys file.
type keyEntry struct {
	Key                string `json:"key"`
	ID                 string `json:"id"`
	Name               string `json:"name"`
	MonthlyBudgetMicro int64  `json:"monthly_budget_micro"`
	DailyBudgetMicro   int64  `json:"daily_budget_micro"`
}

// loadKeys reads the keys file and populates the caller store, keyed by the
// SHA-256 hash of each raw key (raw keys are never persisted).
func loadKeys(path string, cs proxy.CallerStore) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read keys file: %w", err)
	}
	var entries []keyEntry
	if err := json.Unmarshal(data, &entries); err != nil {
		return fmt.Errorf("parse keys file %s: %w", path, err)
	}
	setter, ok := cs.(interface {
		Set(hash string, c proxy.Caller)
	})
	if !ok {
		return fmt.Errorf("caller store does not support seeding")
	}
	for _, e := range entries {
		if e.Key == "" || e.ID == "" {
			return fmt.Errorf("keys file %s: entry missing key or id", path)
		}
		setter.Set(proxy.HashAPIKey(e.Key), proxy.Caller{
			ID:                 e.ID,
			Name:               e.Name,
			MonthlyBudgetMicro: e.MonthlyBudgetMicro,
			DailyBudgetMicro:   e.DailyBudgetMicro,
		})
	}
	return nil
}

// parseThresholds parses a comma-separated list of percentages.
func parseThresholds(s string) []int {
	var out []int
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		n, err := strconv.Atoi(part)
		if err != nil || n <= 0 || n > 100 {
			continue
		}
		out = append(out, n)
	}
	return out
}

// costThresholdReader is a lock-free reader of the per-request cost-alert
// threshold, satisfying proxy.CostThresholdReader on the hot path.
type costThresholdReader struct {
	v *atomic.Int64
}

func (r costThresholdReader) CostAlertThresholdMicro() int64 { return r.v.Load() }
