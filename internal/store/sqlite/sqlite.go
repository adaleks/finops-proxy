// Package sqlite implements the reference SQLite store for the open-source
// core: the request log, budget ledger, settings, and a static model-price
// book. It deliberately migrates only the core tables — request_logs,
// model_prices, settings — never any enterprise table (tenants, api_keys,
// users, incidents, alert configs). Those belong to the enterprise module,
// which keeps its own handle and adapts to the core seams.
//
// GORM and the pure-Go glebarez/sqlite driver are confined to this package (and
// its tests) per the core dependency rule.
package sqlite

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/glebarez/sqlite"
	"github.com/adaleks/finops-proxy/pkg/proxy"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// requestLog maps one row of the request_logs table. KeyID is the flat caller
// id (the enterprise module maps its tenant hierarchy onto it); "" is the
// anonymous/legacy bucket. Money is integer micro-dollars (µUSD).
type requestLog struct {
	ID               uint   `gorm:"primaryKey"`
	AgentID          string `gorm:"column:agent_id"`
	KeyID            string `gorm:"column:key_id"`
	ProjectName      string `gorm:"column:project_name"`
	Model            string `gorm:"column:model"`
	PromptTokens     int64  `gorm:"column:prompt_tokens"`
	CompletionTokens int64  `gorm:"column:completion_tokens"`
	CostUSD          int64  `gorm:"column:cost_usd"` // µUSD
	IsLoopBlocked    bool   `gorm:"column:is_loop_blocked"`
	Timestamp        int64  `gorm:"column:timestamp"` // Unix epoch seconds, UTC
}

// settings maps the single-row settings table (id = 1).
type settings struct {
	ID                      uint  `gorm:"primaryKey"`
	LoopThresholdCount      int   `gorm:"column:loop_threshold_count;not null;default:2"`
	CostAlertThresholdMicro int64 `gorm:"column:cost_alert_threshold_micro;not null;default:0"`
	UpdatedAt               int64 `gorm:"column:updated_at;not null"`
}

// modelPrice maps one row of the model_prices table.
type modelPrice struct {
	Model          string `gorm:"column:model;primaryKey"`
	Provider       string `gorm:"column:provider;not null;default:''"`
	InputMicroUSD  int64  `gorm:"column:input_micro_usd;not null"`
	OutputMicroUSD int64  `gorm:"column:output_micro_usd;not null"`
	Source         string `gorm:"column:source;not null"`
	UpdatedAt      int64  `gorm:"column:updated_at;not null"` // Unix seconds (UTC)
}

// PriceRow is the public form of one model_prices row, for seeding from the
// static config/pricing.json table and for listing.
type PriceRow struct {
	Model          string
	Provider       string
	InputMicroUSD  int64 // µUSD per 1M input (prompt) tokens
	OutputMicroUSD int64 // µUSD per 1M output (completion) tokens
	Source         string
}

// Source values for the model_prices.source column.
const (
	PriceSourceSeed   = "seed"
	PriceSourceManual = "manual"
	PriceSourceSync   = "sync"
)

// DB wraps the gorm handle and serializes access. Writes and reads are
// serialized through a single mutex, and the SQLite handle is pinned to one
// connection so the connection-scoped WAL pragmas apply to every query.
type DB struct {
	mu   sync.Mutex
	gorm *gorm.DB
}

// Open opens (creating if needed) the SQLite database at path, applies WAL
// pragmas, and migrates only the core tables. It creates the parent directory
// if it does not exist.
func Open(path string) (*DB, error) {
	if path == "" {
		path = "finops.db"
	}
	if dir := filepath.Dir(path); dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("sqlite: open %s: create parent dir: %w", path, err)
		}
	}

	g, err := gorm.Open(sqlite.Open(path), &gorm.Config{})
	if err != nil {
		return nil, fmt.Errorf("sqlite: open %s: %w", path, err)
	}
	sqlDB, err := g.DB()
	if err != nil {
		return nil, fmt.Errorf("sqlite: open %s: %w", path, err)
	}
	sqlDB.SetMaxOpenConns(1)

	for _, pragma := range []string{
		"PRAGMA journal_mode = WAL",
		"PRAGMA synchronous = NORMAL",
		"PRAGMA busy_timeout = 5000",
	} {
		if err := g.Exec(pragma).Error; err != nil {
			return nil, fmt.Errorf("sqlite: open %s: %s: %w", path, pragma, err)
		}
	}

	// Core tables only — no tenants, api_keys, users, incidents, or alert
	// configs. Those are the enterprise module's concern.
	if err := g.AutoMigrate(&requestLog{}, &modelPrice{}, &settings{}); err != nil {
		return nil, fmt.Errorf("sqlite: open %s: migrate: %w", path, err)
	}
	// Covering index for the global daily budget aggregation.
	if err := g.Exec(`CREATE INDEX IF NOT EXISTS idx_request_logs_actual_time ON request_logs(is_loop_blocked, timestamp)`).Error; err != nil {
		return nil, fmt.Errorf("sqlite: open %s: create index: %w", path, err)
	}
	// Composite index backing the per-key budget query.
	if err := g.Exec(`CREATE INDEX IF NOT EXISTS idx_request_logs_key_actual_time ON request_logs(key_id, is_loop_blocked, timestamp)`).Error; err != nil {
		return nil, fmt.Errorf("sqlite: open %s: create index: %w", path, err)
	}
	// model_prices provider index: backs the ?provider= filter.
	if err := g.Exec(`CREATE INDEX IF NOT EXISTS idx_model_prices_provider ON model_prices(provider)`).Error; err != nil {
		return nil, fmt.Errorf("sqlite: open %s: create index: %w", path, err)
	}

	return &DB{gorm: g}, nil
}

// LogRequest implements proxy.RequestLogStore by persisting one request row.
func (d *DB) LogRequest(ctx context.Context, r proxy.RequestRecord) error {
	d.mu.Lock()
	defer d.mu.Unlock()

	row := requestLog{
		AgentID:          r.AgentID,
		KeyID:            r.KeyID,
		ProjectName:      r.ProjectName,
		Model:            r.Model,
		PromptTokens:     r.PromptTokens,
		CompletionTokens: r.CompletionTokens,
		CostUSD:          r.CostUSD,
		IsLoopBlocked:    r.IsLoopBlocked,
		Timestamp:        r.Timestamp,
	}
	if err := d.gorm.WithContext(ctx).Create(&row).Error; err != nil {
		return fmt.Errorf("sqlite: log request: %w", err)
	}
	return nil
}

// SumActualCostSince implements proxy.BudgetLedger: the sum of actual
// (non-loop-blocked) cost at or after since, across every caller.
func (d *DB) SumActualCostSince(ctx context.Context, since int64) (int64, error) {
	d.mu.Lock()
	defer d.mu.Unlock()

	var total int64
	err := d.gorm.WithContext(ctx).Raw(`
		SELECT COALESCE(SUM(cost_usd), 0)
		FROM request_logs
		WHERE is_loop_blocked = 0 AND timestamp >= ?`, since).Scan(&total).Error
	if err != nil {
		return 0, fmt.Errorf("sqlite: sum actual cost since %d: %w", since, err)
	}
	return total, nil
}

// SumActualCostForKeySince implements proxy.BudgetLedger: the sum of actual
// cost for one caller's requests at or after since. The empty keyID selects the
// anonymous/legacy bucket.
func (d *DB) SumActualCostForKeySince(ctx context.Context, keyID string, since int64) (int64, error) {
	d.mu.Lock()
	defer d.mu.Unlock()

	var total int64
	err := d.gorm.WithContext(ctx).Raw(`
		SELECT COALESCE(SUM(cost_usd), 0)
		FROM request_logs
		WHERE key_id = ? AND is_loop_blocked = 0 AND timestamp >= ?`, keyID, since).Scan(&total).Error
	if err != nil {
		return 0, fmt.Errorf("sqlite: sum actual cost since %d for key %q: %w", since, keyID, err)
	}
	return total, nil
}

// GetSettings implements proxy.SettingsStore, seeding the defaults on first
// access so the table always holds exactly one row.
func (d *DB) GetSettings(ctx context.Context) (proxy.SettingsRow, error) {
	d.mu.Lock()
	defer d.mu.Unlock()

	var row settings
	err := d.gorm.WithContext(ctx).First(&row, 1).Error
	if err == nil {
		return proxy.SettingsRow{
			LoopThresholdCount:      row.LoopThresholdCount,
			CostAlertThresholdMicro: row.CostAlertThresholdMicro,
		}, nil
	}
	if !errors.Is(err, gorm.ErrRecordNotFound) {
		return proxy.SettingsRow{}, fmt.Errorf("sqlite: get settings: %w", err)
	}

	seed := settings{
		ID:                      1,
		LoopThresholdCount:      DefaultLoopThreshold,
		CostAlertThresholdMicro: 0,
		UpdatedAt:               time.Now().Unix(),
	}
	if err := d.gorm.WithContext(ctx).Create(&seed).Error; err != nil {
		return proxy.SettingsRow{}, fmt.Errorf("sqlite: seed settings: %w", err)
	}
	return proxy.SettingsRow{
		LoopThresholdCount:      seed.LoopThresholdCount,
		CostAlertThresholdMicro: seed.CostAlertThresholdMicro,
	}, nil
}

// UpdateSettings implements proxy.SettingsStore by upserting the single row onto
// id = 1.
func (d *DB) UpdateSettings(ctx context.Context, s proxy.SettingsRow) error {
	d.mu.Lock()
	defer d.mu.Unlock()

	row := settings{
		ID:                      1,
		LoopThresholdCount:      s.LoopThresholdCount,
		CostAlertThresholdMicro: s.CostAlertThresholdMicro,
		UpdatedAt:               time.Now().Unix(),
	}
	err := d.gorm.WithContext(ctx).Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "id"}},
		DoUpdates: clause.AssignmentColumns([]string{"loop_threshold_count", "cost_alert_threshold_micro", "updated_at"}),
	}).Create(&row).Error
	if err != nil {
		return fmt.Errorf("sqlite: update settings: %w", err)
	}
	return nil
}

// SeedModelPrices inserts the static price rows with INSERT OR IGNORE semantics
// on the model key: an existing row is never overwritten. It is the core
// equivalent of the enterprise boot seeding, fed from the static
// config/pricing.json table.
func (d *DB) SeedModelPrices(ctx context.Context, rows []PriceRow) error {
	d.mu.Lock()
	defer d.mu.Unlock()

	if len(rows) == 0 {
		return nil
	}
	out := make([]modelPrice, 0, len(rows))
	for _, r := range rows {
		out = append(out, modelPrice{
			Model:          r.Model,
			Provider:       r.Provider,
			InputMicroUSD:  r.InputMicroUSD,
			OutputMicroUSD: r.OutputMicroUSD,
			Source:         r.Source,
			UpdatedAt:      time.Now().Unix(),
		})
	}
	if err := d.gorm.WithContext(ctx).Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "model"}},
		DoNothing: true,
	}).Create(&out).Error; err != nil {
		return fmt.Errorf("sqlite: seed model prices: %w", err)
	}
	return nil
}

// ListModelPrices returns every price row, ordered by model case-insensitively.
func (d *DB) ListModelPrices(ctx context.Context) ([]PriceRow, error) {
	d.mu.Lock()
	defer d.mu.Unlock()

	rows := make([]modelPrice, 0)
	if err := d.gorm.WithContext(ctx).Order("model COLLATE NOCASE").Find(&rows).Error; err != nil {
		return nil, fmt.Errorf("sqlite: list model prices: %w", err)
	}
	out := make([]PriceRow, 0, len(rows))
	for _, r := range rows {
		out = append(out, PriceRow{
			Model:          r.Model,
			Provider:       r.Provider,
			InputMicroUSD:  r.InputMicroUSD,
			OutputMicroUSD: r.OutputMicroUSD,
			Source:         r.Source,
		})
	}
	return out, nil
}

// Close closes the underlying database. Idempotent.
func (d *DB) Close() error {
	d.mu.Lock()
	defer d.mu.Unlock()

	if d.gorm == nil {
		return nil // already closed
	}
	sqlDB, err := d.gorm.DB()
	if err != nil {
		return fmt.Errorf("sqlite: close: %w", err)
	}
	d.gorm = nil
	if err := sqlDB.Close(); err != nil {
		return fmt.Errorf("sqlite: close: %w", err)
	}
	return nil
}

// DefaultLoopThreshold mirrors the detector's repeat-threshold default.
const DefaultLoopThreshold = 2

// Compile-time assertion that the SQLite store satisfies the core seam.
var _ proxy.Store = (*DB)(nil)
