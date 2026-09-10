package proxy

import (
	"github.com/adaleks/finops-proxy/pkg/logstream"
)

// publishLog forwards a live log event to the stream hub. Nil-safe: with no hub
// wired the proxy behaves exactly as it did without the live log stream, so the
// request path never depends on a subscriber being attached.
func (h *handler) publishLog(ev logstream.LogEvent) {
	if h.hub != nil {
		h.hub.Publish(ev)
	}
}
