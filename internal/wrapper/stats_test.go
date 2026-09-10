package wrapper

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestProxyClientMetrics(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/metrics" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(metricsSnapshot{
			ActiveRequests:    3,
			TotalRequests:     10,
			LoopBlocks:        2,
			BlockedAgents:     1,
			SavedCostMicroUSD: 4200,
			AvgLatencyMicro:   1500,
			TokenVelocity:     100.5,
			UptimeSeconds:     7,
		})
	}))
	defer srv.Close()

	c := NewProxyClient(srv.URL, time.Second, 0)
	snap, err := c.Metrics(context.Background())
	if err != nil {
		t.Fatalf("Metrics: %v", err)
	}
	if snap.ActiveRequests != 3 || snap.SavedCostMicroUSD != 4200 || snap.TokenVelocity != 100.5 {
		t.Fatalf("snapshot = %+v", snap)
	}
}

func TestProxyClientMetricsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	c := NewProxyClient(srv.URL, time.Second, 0)
	if _, err := c.Metrics(context.Background()); err == nil {
		t.Fatalf("expected error on non-200")
	}
}

func TestStatusBarRenderOffTTY(t *testing.T) {
	var buf bytes.Buffer
	bar := newStatusBar(nil, &buf, time.Second, false)
	bar.render(metricsSnapshot{ActiveRequests: 1, LoopBlocks: 2})
	if buf.Len() != 0 {
		t.Fatalf("off-TTY render wrote %q, want nothing", buf.String())
	}
}

func TestStatusBarRenderOnTTY(t *testing.T) {
	var buf bytes.Buffer
	bar := newStatusBar(nil, &buf, time.Second, true)
	bar.render(metricsSnapshot{
		ActiveRequests:    2,
		TokenVelocity:     120,
		LoopBlocks:        3,
		SavedCostMicroUSD: 12345,
	})
	out := buf.String()
	if !bytes.Contains([]byte(out), []byte("\x1b[2K\r")) {
		t.Fatalf("missing ANSI redraw prefix: %q", out)
	}
	if !bytes.Contains([]byte(out), []byte("active=2")) {
		t.Fatalf("missing active gauge: %q", out)
	}
	if !bytes.Contains([]byte(out), []byte("saved=$0.0123")) {
		t.Fatalf("missing formatted saved cost: %q", out)
	}
}

func TestStatusBarLoopDetected(t *testing.T) {
	var buf bytes.Buffer
	bar := newStatusBar(nil, &buf, time.Second, true)
	bar.loopDetected()
	if !bytes.Contains(buf.Bytes(), []byte("loop detected")) {
		t.Fatalf("missing loop alert: %q", buf.String())
	}
}

func TestIsTerminal(t *testing.T) {
	var buf bytes.Buffer
	if isTerminal(&buf) {
		t.Fatalf("bytes.Buffer reported as terminal")
	}
}
