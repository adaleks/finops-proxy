package logging

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"

	"github.com/adaleks/finops-proxy/pkg/logstream"
	"github.com/adaleks/finops-proxy/pkg/proxy"
)

func TestSlogLoggerJSONLines(t *testing.T) {
	var buf bytes.Buffer
	l := NewSlogLogger(&buf, slog.LevelInfo)

	l.Log(logstream.LogEvent{
		Type:         logstream.TypeRequestComplete,
		Level:        "info",
		Timestamp:    1,
		AgentID:      "agent-1",
		Model:        "gpt-4o",
		CostMicroUSD: 1234,
		StatusCode:   200,
	})

	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) != 1 {
		t.Fatalf("got %d lines, want 1: %q", len(lines), buf.String())
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(lines[0]), &m); err != nil {
		t.Fatalf("line is not valid JSON: %v\n%s", err, lines[0])
	}
	for _, key := range []string{"type", "level", "timestamp", "agent_id", "model", "cost_micro_usd", "status_code"} {
		if _, ok := m[key]; !ok {
			t.Fatalf("missing key %q in %s", key, lines[0])
		}
	}
	if m["level"] != "info" {
		t.Fatalf("level = %v, want lowercase \"info\"", m["level"])
	}
	if m["type"] != "request.complete" {
		t.Fatalf("type = %v, want request.complete", m["type"])
	}
}

func TestSlogLoggerNotify(t *testing.T) {
	var buf bytes.Buffer
	l := NewSlogLogger(&buf, slog.LevelInfo)
	l.Notify(proxy.Event{
		Type:           proxy.EventLoopBlocked,
		Timestamp:      2,
		AgentID:        "a",
		SavedCostMicro: 42,
		Reason:         "loop",
	})

	var m map[string]any
	if err := json.Unmarshal(buf.Bytes(), &m); err != nil {
		t.Fatalf("notify output not valid JSON: %v", err)
	}
	if m["type"] != "loop_blocked" {
		t.Fatalf("type = %v, want loop_blocked", m["type"])
	}
	if m["saved_cost_micro_usd"] != float64(42) {
		t.Fatalf("saved_cost_micro_usd = %v, want 42", m["saved_cost_micro_usd"])
	}
}
