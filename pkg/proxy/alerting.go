package proxy

import (
	"context"
	"time"

	"github.com/adaleks/finops-proxy/pkg/logstream"
)

// emit is fire-and-forget: Notify is contractually non-blocking, so this never
// delays the request path. nil-safe.
func (h *handler) emit(ev Event) {
	if h.notifier != nil {
		h.notifier.Notify(ev)
	}
}

// loopAlertsEnabled reports whether loop_blocked notification emission is
// enabled. It type-asserts the notifier for the LoopAlertGate seam: a notifier
// that implements the gate (a runtime manager) reports its live flag; one that
// does not (a noop, the JSON stdout logger, or a test fake) defaults to enabled,
// preserving the pre-gate behavior. A nil notifier means no sink exists, so it
// reports false.
func (h *handler) loopAlertsEnabled() bool {
	if h.notifier == nil {
		return false
	}
	if gate, ok := h.notifier.(LoopAlertGate); ok {
		return gate.LoopAlertsEnabled()
	}
	return true
}

// checkBudget evaluates the daily budget after a forwarded request's cost has
// been recorded. It runs strictly after LogRequest in onCost, so the just-
// completed request is included in the sum. Never blocks or panics the request
// path: nil-guards + swallowed ledger errors.
func (h *handler) checkBudget() {
	if h.notifier == nil || h.budget == nil || h.ledger == nil {
		return
	}
	start := utcDayStart(time.Now())
	spent, err := h.ledger.SumActualCostSince(context.Background(), start)
	if err != nil {
		return
	}
	h.budget.Spend(spent, time.Now(), func(ev Event) bool {
		if h.notifier == nil {
			return false
		}
		h.notifier.Notify(ev)
		// Live log: mirror the budget_level notification onto the stream with
		// the same daily-budget / spend / threshold figures. Fires at most once
		// per threshold per UTC day (BudgetMonitor dedupes).
		h.publishLog(logstream.LogEvent{
			Type:           logstream.TypeBudgetLevel,
			Level:          "warn",
			Timestamp:      ev.Timestamp,
			BudgetMicroUSD: ev.BudgetMicroUSD,
			SpentMicroUSD:  ev.SpentMicroUSD,
			ThresholdPct:   ev.ThresholdPct,
			Reason:         "daily budget level crossed",
		})
		return true
	})
}
