package wrapper

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// newFakeProxy records DELETE requests to /v1/agent/*/history and answers 204.
func newFakeProxy(t *testing.T) (*httptest.Server, *atomic.Int32, *atomic.Value) {
	t.Helper()
	var deletes atomic.Int32
	var lastPath atomic.Value
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete && strings.HasPrefix(r.URL.Path, "/v1/agent/") {
			deletes.Add(1)
			lastPath.Store(r.URL.Path)
			w.WriteHeader(http.StatusNoContent)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)
	return srv, &deletes, &lastPath
}

// TestRunKillsLoopingChildAndDeletes proves the kill → DELETE → give-up path:
// a child that prints the loop marker and hangs is killed, its history cleared,
// and with max-restarts=0 the run gives up (exit 42).
func TestRunKillsLoopingChildAndDeletes(t *testing.T) {
	proxy, deletes, lastPath := newFakeProxy(t)

	cfg := DefaultConfig()
	cfg.ProxyURL = proxy.URL
	cfg.AgentID = "agent-test"
	cfg.MaxRestarts = 0
	cfg.KillGrace = 500 * time.Millisecond

	var out, errBuf bytes.Buffer
	done := make(chan int, 1)
	go func() {
		done <- Run(context.Background(), cfg, "sh",
			[]string{"-c", `echo "loop agent_loop_exception" >&2; sleep 30`},
			strings.NewReader(""), &out, &errBuf)
	}()

	select {
	case code := <-done:
		if code != 42 {
			t.Fatalf("Run returned %d, want 42 (give up after loop with max-restarts=0)", code)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return within 5s (child was not killed)")
	}

	if got := deletes.Load(); got != 1 {
		t.Fatalf("DELETE called %d times, want 1", got)
	}
	if p, _ := lastPath.Load().(string); p != "/v1/agent/agent-test/history" {
		t.Errorf("DELETE path = %q, want %q", p, "/v1/agent/agent-test/history")
	}
}

// TestRunRestartsWithRecoveryPrompt proves the full auto-recovery: the first
// child loops and is killed, history is cleared, and the restart sees the
// recovery prompt and exits 0.
func TestRunRestartsWithRecoveryPrompt(t *testing.T) {
	proxy, deletes, _ := newFakeProxy(t)

	cfg := DefaultConfig()
	cfg.ProxyURL = proxy.URL
	cfg.AgentID = "agent-test"
	cfg.MaxRestarts = 2
	cfg.RecoveryPrompt = "try an alternative"
	cfg.KillGrace = 500 * time.Millisecond

	script := `if [ -n "$FINOPS_RECOVERY_PROMPT" ]; then echo "recovered: $FINOPS_RECOVERY_PROMPT"; exit 0; fi; echo "loop agent_loop_exception" >&2; sleep 30`

	var out, errBuf bytes.Buffer
	done := make(chan int, 1)
	go func() {
		done <- Run(context.Background(), cfg, "sh", []string{"-c", script},
			strings.NewReader(""), &out, &errBuf)
	}()

	select {
	case code := <-done:
		if code != 0 {
			t.Fatalf("Run returned %d, want 0 (recovered on restart); stderr=%q", code, errBuf.String())
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return within 5s")
	}

	if got := deletes.Load(); got != 1 {
		t.Fatalf("DELETE called %d times, want 1", got)
	}
	if !strings.Contains(out.String(), "recovered: try an alternative") {
		t.Errorf("recovery output missing, got %q", out.String())
	}
}
