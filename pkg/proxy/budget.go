package proxy

import (
	"sort"
	"sync"
	"time"
)

// BudgetSpender is the narrow seam through which the proxy's hot path evaluates
// the daily budget. Both *BudgetMonitor (the file-only monitor) and the
// enterprise runtime-reconfigurable dispatcher satisfy it, so the proxy never
// depends on a concrete type. Spend must be non-blocking and nil-safe.
type BudgetSpender interface {
	Spend(spentMicro int64, now time.Time, emit func(Event) bool)
}

// BudgetConfig is the immutable configuration of the global daily budget
// monitor. Money is integer µUSD.
type BudgetConfig struct {
	DailyBudgetMicro int64 // 0 = daily budget disabled (the monitor is inert)
	ThresholdPct     []int // percentages of the daily budget that fire an event
}

// BudgetMonitor emits budget_level events when real daily spend crosses a
// configured percentage of the daily budget, at most once per threshold per UTC
// day. It is safe for concurrent use: Spend holds a mutex only for map lookups
// and integer comparisons, and the emit callback is invoked without any lock.
//
// State is in-memory only — a restart forgets today's fired thresholds, so an
// already-crossed threshold fires again after the process restarts. That is
// acceptable for advisory alerts, which are not accounting records.
type BudgetMonitor struct {
	mu         sync.Mutex
	cfg        BudgetConfig
	thresholds []int // sorted ascending, deduplicated, 1..100

	dayKey string       // current UTC day "2006-01-02"
	fired  map[int]bool // pct -> fired today
}

// NewBudgetMonitor builds a BudgetMonitor from a BudgetConfig. Thresholds are
// sorted ascending and deduplicated; values <= 0 or > 100 are dropped. A zero
// daily budget or no remaining thresholds makes the monitor inert (Spend is a
// no-op).
func NewBudgetMonitor(cfg BudgetConfig) *BudgetMonitor {
	m := &BudgetMonitor{
		cfg:        cfg,
		thresholds: sanitizeThresholds(cfg.ThresholdPct),
		fired:      make(map[int]bool),
	}
	if cfg.DailyBudgetMicro <= 0 || len(m.thresholds) == 0 {
		m.thresholds = nil
	}
	return m
}

// Spend evaluates the current daily spend against every threshold not yet
// fired today, emitting a budget_level event per newly crossed threshold. On a
// UTC day rollover the fired state resets. A threshold is recorded as fired
// only if emit returns true (so a dropped delivery re-fires on the next Spend).
func (m *BudgetMonitor) Spend(spentMicro int64, now time.Time, emit func(Event) bool) {
	if len(m.thresholds) == 0 || m.cfg.DailyBudgetMicro <= 0 {
		return
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	day := now.UTC().Format("2006-01-02")
	if day != m.dayKey {
		m.dayKey = day
		m.fired = make(map[int]bool)
	}

	for _, pct := range m.thresholds {
		if m.fired[pct] {
			continue
		}
		thresholdMicro := m.cfg.DailyBudgetMicro * int64(pct) / 100
		if spentMicro < thresholdMicro {
			continue
		}
		ev := Event{
			Type:           EventBudgetLevel,
			Timestamp:      now.Unix(),
			BudgetMicroUSD: m.cfg.DailyBudgetMicro,
			SpentMicroUSD:  spentMicro,
			ThresholdPct:   pct,
		}
		if emit(ev) {
			m.fired[pct] = true
		}
	}
}

// sanitizeThresholds sorts ascending, deduplicates, and drops values outside
// 1..100. It never errors — out-of-range entries are defensive garbage that the
// monitor ignores.
func sanitizeThresholds(in []int) []int {
	seen := make(map[int]bool, len(in))
	out := make([]int, 0, len(in))
	for _, p := range in {
		if p <= 0 || p > 100 || seen[p] {
			continue
		}
		seen[p] = true
		out = append(out, p)
	}
	sort.Ints(out)
	return out
}

// utcMonthStart returns the Unix epoch second of the UTC start of the month
// containing t. It is the boundary for the per-caller monthly budget gate.
func utcMonthStart(t time.Time) int64 {
	u := t.UTC()
	return time.Date(u.Year(), u.Month(), 1, 0, 0, 0, 0, time.UTC).Unix()
}

// nextMonthStart returns the Unix epoch second of the UTC start of the month
// after the one containing t. It drives the monthly budget_exceeded Retry-After.
func nextMonthStart(t time.Time) int64 {
	u := t.UTC()
	return time.Date(u.Year(), u.Month()+1, 1, 0, 0, 0, 0, time.UTC).Unix()
}

// utcDayStart returns the Unix epoch second of the UTC start of the day
// containing t. It is the boundary for the per-caller daily budget gate and the
// global daily budget monitor.
func utcDayStart(t time.Time) int64 {
	u := t.UTC()
	return time.Date(u.Year(), u.Month(), u.Day(), 0, 0, 0, 0, time.UTC).Unix()
}

// nextDayStart returns the Unix epoch second of the UTC start of the day after
// the one containing t. It drives the daily budget_exceeded Retry-After.
func nextDayStart(t time.Time) int64 {
	u := t.UTC()
	return time.Date(u.Year(), u.Month(), u.Day()+1, 0, 0, 0, 0, time.UTC).Unix()
}
