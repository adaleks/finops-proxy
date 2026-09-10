// Package logging implements the core's standardized JSON logging: it writes
// every proxy.Event and every logstream.LogEvent as one JSON object per line to
// stdout (or any io.Writer). It is the standalone-core substitute for the
// enterprise webhook notifier and SSE dashboard — same events, transport-free.
package logging

import (
	"context"
	"encoding/json"
	"io"
	"sync"

	"github.com/adaleks/finops-proxy/pkg/logstream"
	"github.com/adaleks/finops-proxy/pkg/proxy"
)

// Logger writes proxy.Events and logstream.LogEvents as newline-delimited JSON.
// It is safe for concurrent use: a mutex serializes writes so one event never
// interleaves with another.
type Logger struct {
	mu  sync.Mutex
	enc *json.Encoder
}

// New returns a Logger writing to w. w is used directly, so a caller that wants
// trailing-newline-free lines passes a writer that already ends each Write.
func New(w io.Writer) *Logger {
	return &Logger{enc: json.NewEncoder(w)}
}

// Notify implements proxy.Notifier: it writes the alert event as one JSON line.
// It never blocks on I/O beyond a single write and never panics.
func (l *Logger) Notify(ev proxy.Event) {
	l.mu.Lock()
	defer l.mu.Unlock()
	_ = l.enc.Encode(ev)
}

// Close implements proxy.Notifier. There is no worker or queue to drain, so it
// is a no-op.
func (l *Logger) Close() {}

// Log writes one live log event as a JSON line.
func (l *Logger) Log(ev logstream.LogEvent) {
	l.mu.Lock()
	defer l.mu.Unlock()
	_ = l.enc.Encode(ev)
}

// Subscribe attaches the logger to a hub: every published logstream.LogEvent is
// written as a JSON line, so request.complete, loop_blocked, budget_exceeded,
// budget_level, and cost_alert events all reach stdout through one path. It
// returns a stop func that unsubscribes and is safe to call once.
func (l *Logger) Subscribe(hub *logstream.Hub) (stop func()) {
	if hub == nil {
		return func() {}
	}
	sub := hub.Subscribe(context.Background(), 1024)
	go func() {
		for ev := range sub.C {
			l.Log(ev)
		}
	}()
	return func() { sub.Close() }
}

// Compile-time assertion that the JSON logger satisfies the notifier seam.
var _ proxy.Notifier = (*Logger)(nil)
