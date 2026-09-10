package wrapper

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestDeleteHistorySuccess(t *testing.T) {
	var hits atomic.Int32
	var lastPath atomic.Value
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		lastPath.Store(r.URL.Path)
		if r.Method != http.MethodDelete {
			t.Errorf("method = %s, want DELETE", r.Method)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(srv.Close)

	c := NewProxyClient(srv.URL, time.Second, 0)
	if err := c.DeleteHistory(context.Background(), "a/b c"); err != nil {
		t.Fatalf("DeleteHistory: %v", err)
	}
	if got := hits.Load(); got != 1 {
		t.Fatalf("DELETE calls = %d, want 1", got)
	}
	// PathEscape keeps the id inside one path segment.
	if p, _ := lastPath.Load().(string); p != "/v1/agent/a%2Fb%20c/history" {
		t.Errorf("path = %q, want %q", p, "/v1/agent/a%2Fb%20c/history")
	}
}

func TestDeleteHistoryNotFoundIsSuccess(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(srv.Close)

	c := NewProxyClient(srv.URL, time.Second, 0)
	if err := c.DeleteHistory(context.Background(), "x"); err != nil {
		t.Fatalf("404 must be treated as success, got %v", err)
	}
}

func TestDeleteHistoryRetriesThenFailsOpen(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)

	c := NewProxyClient(srv.URL, time.Second, 2) // 2 retries → 3 attempts
	c.backoff = time.Millisecond                 // keep the test fast

	if err := c.DeleteHistory(context.Background(), "x"); err == nil {
		t.Fatal("expected a non-nil error after exhausting retries")
	}
	if got := hits.Load(); got != 3 {
		t.Fatalf("attempts = %d, want 3 (1 + 2 retries)", got)
	}
}
