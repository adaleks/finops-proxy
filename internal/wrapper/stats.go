package wrapper

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"sync"
	"time"
)

// metricsSnapshot is the local DTO for the proxy's GET /v1/metrics response. It
// is declared here (rather than importing pkg/proxy) to keep the wrapper package
// stdlib-only, matching the rest of finops-run.
type metricsSnapshot struct {
	ActiveRequests    int64   `json:"active_requests"`
	TotalRequests     int64   `json:"total_requests"`
	LoopBlocks        int64   `json:"loop_blocks"`
	BlockedAgents     int     `json:"blocked_agents"`
	SavedCostMicroUSD int64   `json:"saved_cost_micro_usd"`
	AvgLatencyMicro   int64   `json:"avg_latency_micro"`
	TokenVelocity     float64 `json:"token_velocity"`
	UptimeSeconds     int64   `json:"uptime_seconds"`
}

// Metrics fetches the proxy's /v1/metrics snapshot into the local DTO. It is
// best-effort: a non-2xx or network error is returned so the caller can treat it
// fail-open.
func (c *ProxyClient) Metrics(ctx context.Context) (metricsSnapshot, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.metricsURL(), nil)
	if err != nil {
		return metricsSnapshot{}, err
	}
	resp, err := c.httpc.Do(req)
	if err != nil {
		return metricsSnapshot{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return metricsSnapshot{}, fmt.Errorf("proxy returned status %d", resp.StatusCode)
	}
	var snap metricsSnapshot
	if err := json.NewDecoder(resp.Body).Decode(&snap); err != nil {
		return metricsSnapshot{}, err
	}
	return snap, nil
}

// metricsURL returns {base}/v1/metrics.
func (c *ProxyClient) metricsURL() string {
	u, err := url.Parse(c.baseURL)
	if err != nil {
		return c.baseURL + "/v1/metrics"
	}
	u.Path = "/v1/metrics"
	u.RawQuery = ""
	return u.String()
}

// statusBar renders a one-line live status view by polling the proxy's
// /v1/metrics endpoint. On a TTY it redraws the line in place with ANSI escape
// sequences; off a TTY it is silent so piped output stays clean. All writes are
// serialized by a mutex.
type statusBar struct {
	client   *ProxyClient
	out      io.Writer
	interval time.Duration
	tty      bool
	mu       sync.Mutex
}

func newStatusBar(client *ProxyClient, out io.Writer, interval time.Duration, tty bool) *statusBar {
	if interval <= 0 {
		interval = time.Second
	}
	return &statusBar{client: client, out: out, interval: interval, tty: tty}
}

// start launches the polling goroutine. It exits when ctx is done.
func (s *statusBar) start(ctx context.Context) {
	go func() {
		t := time.NewTicker(s.interval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				s.poll(ctx)
			}
		}
	}()
}

func (s *statusBar) poll(ctx context.Context) {
	snap, err := s.client.Metrics(ctx)
	if err != nil {
		return // fail-open: a missed poll is invisible
	}
	s.render(snap)
}

func (s *statusBar) render(snap metricsSnapshot) {
	if !s.tty {
		return
	}
	saved := float64(snap.SavedCostMicroUSD) / 1_000_000
	line := fmt.Sprintf(
		"[finops-run] active=%d tok/s=%.0f blocks=%d blocked=%d saved=$%.4f avg=%dµs",
		snap.ActiveRequests, snap.TokenVelocity, snap.LoopBlocks,
		snap.BlockedAgents, saved, snap.AvgLatencyMicro,
	)
	s.mu.Lock()
	defer s.mu.Unlock()
	fmt.Fprintf(s.out, "\x1b[2K\r%s", line)
}

// loopDetected renders an immediate, non-overwritten loop alert line (TTY only;
// off-TTY the wrapper's own logger already reports the loop).
func (s *statusBar) loopDetected() {
	if !s.tty {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	fmt.Fprintf(s.out, "\x1b[2K\r[finops-run] loop detected — restarting agent\n")
}

// isTerminal reports whether w is a character-device (terminal) writer.
func isTerminal(w io.Writer) bool {
	f, ok := w.(*os.File)
	if !ok {
		return false
	}
	fi, err := f.Stat()
	if err != nil {
		return false
	}
	return fi.Mode()&os.ModeCharDevice != 0
}
