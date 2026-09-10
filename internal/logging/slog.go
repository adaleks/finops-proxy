// Package logging implements the core's standardized JSON logging: it writes
// every proxy.Event and every logstream.LogEvent as one JSON object per line to
// stdout (or any io.Writer). This file is the slog-based variant; jsonlog.go is
// the encoding/json reference. Both satisfy the same proxy.Notifier seam.
package logging

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"sync"

	"github.com/adaleks/finops-proxy/pkg/logstream"
	"github.com/adaleks/finops-proxy/pkg/proxy"
)

// SlogLogger writes proxy.Events and logstream.LogEvents as newline-delimited
// JSON via slog's JSONHandler, so each line is a single JSON object pipeable
// through `jq`. It is safe for concurrent use. It runs off the request path
// (driven by the hub subscription goroutine or the control plane), so its
// per-record allocation is not part of the hot-path budget.
type SlogLogger struct {
	mu sync.Mutex
	l  *slog.Logger
}

// NewSlogLogger returns a SlogLogger writing JSON to w at the given level. The
// handler lowercases slog's built-in "level" (INFO → info) so it matches the
// event's own lowercase level convention (matching jsonlog.go).
func NewSlogLogger(w io.Writer, level slog.Level) *SlogLogger {
	h := slog.NewJSONHandler(w, &slog.HandlerOptions{
		Level: level,
		ReplaceAttr: func(_ []string, a slog.Attr) slog.Attr {
			if a.Key == slog.LevelKey {
				a.Value = slog.StringValue(strings.ToLower(a.Value.String()))
			}
			return a
		},
	})
	return &SlogLogger{l: slog.New(h)}
}

// Notify implements proxy.Notifier: it writes the alert event as one JSON line.
// It never blocks beyond a single write and never panics.
func (s *SlogLogger) Notify(ev proxy.Event) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.l.LogAttrs(context.Background(), slog.LevelWarn, string(ev.Type), eventAttrs(ev)...)
}

// Close implements proxy.Notifier. There is no worker or queue to drain.
func (s *SlogLogger) Close() {}

// Log writes one live log event as a JSON line.
func (s *SlogLogger) Log(ev logstream.LogEvent) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.l.LogAttrs(context.Background(), levelFor(ev.Level), ev.Type, logEventAttrs(ev)...)
}

// Subscribe attaches the logger to a hub: every published logstream.LogEvent is
// written as a JSON line. It returns a stop func safe to call once.
func (s *SlogLogger) Subscribe(hub *logstream.Hub) (stop func()) {
	if hub == nil {
		return func() {}
	}
	sub := hub.Subscribe(context.Background(), 1024)
	go func() {
		for ev := range sub.C {
			s.Log(ev)
		}
	}()
	return func() { sub.Close() }
}

// levelFor maps a logstream level string to a slog.Level.
func levelFor(l string) slog.Level {
	switch l {
	case "error":
		return slog.LevelError
	case "warn":
		return slog.LevelWarn
	case "debug":
		return slog.LevelDebug
	default:
		return slog.LevelInfo
	}
}

// eventAttrs flattens a proxy.Event into slog attrs, reusing the same JSON field
// names the jsonlog.go encoder emits so jq paths are unchanged.
func eventAttrs(ev proxy.Event) []slog.Attr {
	a := make([]slog.Attr, 0, 10)
	a = append(a, slog.String("type", string(ev.Type)), slog.Int64("timestamp", ev.Timestamp))
	if ev.AgentID != "" {
		a = append(a, slog.String("agent_id", ev.AgentID))
	}
	if ev.Project != "" {
		a = append(a, slog.String("project", ev.Project))
	}
	if ev.Model != "" {
		a = append(a, slog.String("model", ev.Model))
	}
	if ev.SavedCostMicro != 0 {
		a = append(a, slog.Int64("saved_cost_micro_usd", ev.SavedCostMicro))
	}
	if ev.Reason != "" {
		a = append(a, slog.String("reason", ev.Reason))
	}
	if ev.RetryAfter != 0 {
		a = append(a, slog.Int("retry_after", ev.RetryAfter))
	}
	if ev.BudgetMicroUSD != 0 {
		a = append(a, slog.Int64("budget_micro_usd", ev.BudgetMicroUSD))
	}
	if ev.SpentMicroUSD != 0 {
		a = append(a, slog.Int64("spent_micro_usd", ev.SpentMicroUSD))
	}
	if ev.ThresholdPct != 0 {
		a = append(a, slog.Int("threshold_pct", ev.ThresholdPct))
	}
	return a
}

// logEventAttrs flattens a logstream.LogEvent into slog attrs with the same flat
// JSON field names the jsonlog.go encoder emits. Zero-value fields are omitted
// (omitempty semantics) to keep lines minimal.
func logEventAttrs(ev logstream.LogEvent) []slog.Attr {
	a := make([]slog.Attr, 0, 20)
	a = append(a,
		slog.String("type", ev.Type),
		slog.Int64("timestamp", ev.Timestamp),
	)
	if ev.TenantID != "" {
		a = append(a, slog.String("tenant_id", ev.TenantID))
	}
	if ev.TenantName != "" {
		a = append(a, slog.String("tenant_name", ev.TenantName))
	}
	if ev.AgentID != "" {
		a = append(a, slog.String("agent_id", ev.AgentID))
	}
	if ev.Model != "" {
		a = append(a, slog.String("model", ev.Model))
	}
	if ev.ProjectName != "" {
		a = append(a, slog.String("project_name", ev.ProjectName))
	}
	if ev.PromptTokens != 0 {
		a = append(a, slog.Int64("prompt_tokens", ev.PromptTokens))
	}
	if ev.CompletionTokens != 0 {
		a = append(a, slog.Int64("completion_tokens", ev.CompletionTokens))
	}
	if ev.CostMicroUSD != 0 {
		a = append(a, slog.Int64("cost_micro_usd", ev.CostMicroUSD))
	}
	if ev.SavedCostMicroUSD != 0 {
		a = append(a, slog.Int64("saved_cost_micro_usd", ev.SavedCostMicroUSD))
	}
	if ev.BudgetMicroUSD != 0 {
		a = append(a, slog.Int64("budget_micro_usd", ev.BudgetMicroUSD))
	}
	if ev.SpentMicroUSD != 0 {
		a = append(a, slog.Int64("spent_micro_usd", ev.SpentMicroUSD))
	}
	if ev.MonthlyBudgetMicro != 0 {
		a = append(a, slog.Int64("monthly_budget_micro", ev.MonthlyBudgetMicro))
	}
	if ev.StatusCode != 0 {
		a = append(a, slog.Int("status_code", ev.StatusCode))
	}
	if ev.FirstBlock {
		a = append(a, slog.Bool("first_block", ev.FirstBlock))
	}
	if ev.Reason != "" {
		a = append(a, slog.String("reason", ev.Reason))
	}
	if ev.RetryAfter != 0 {
		a = append(a, slog.Int("retry_after", ev.RetryAfter))
	}
	if ev.ThresholdPct != 0 {
		a = append(a, slog.Int("threshold_pct", ev.ThresholdPct))
	}
	return a
}

// Compile-time assertion that the slog logger satisfies the notifier seam.
var _ proxy.Notifier = (*SlogLogger)(nil)
