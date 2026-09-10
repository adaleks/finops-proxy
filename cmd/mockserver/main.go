// Command mockserver runs a standalone mock LLM upstream for manual smoke
// tests of the FinOps circuit-breaker proxy (PLAN.md §7).
//
// It answers non-streaming JSON on /v1/chat/completions, a Server-Sent Events
// stream on /sse, a liveness check on /healthz, and can be forced to fail
// (?status=500) or hang (?hang=1) on any path.
package main

import (
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"strconv"
	"sync/atomic"
)

func main() {
	addr := flag.String("addr", ":8081", "listen address for the mock upstream")
	flag.Parse()

	var counter atomic.Int64

	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		n := counter.Add(1)
		log.Printf("[%d] %s %s contentLength=%d", n, r.Method, r.URL.Path, r.ContentLength)

		// ?hang=1 → block until the client (or proxy) goes away.
		if r.URL.Query().Get("hang") == "1" {
			<-r.Context().Done()
			return
		}

		// ?status=500 (or any code) on any path → plain-text error.
		if status := r.URL.Query().Get("status"); status != "" {
			code, err := strconv.Atoi(status)
			if err != nil {
				code = http.StatusInternalServerError
			}
			w.WriteHeader(code)
			fmt.Fprintf(w, "mock error %d\n", code)
			return
		}

		switch r.URL.Path {
		case "/v1/chat/completions":
			if r.Method != http.MethodPost {
				w.WriteHeader(http.StatusMethodNotAllowed)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, `{"id":"mock-1","ok":true}`)

		case "/sse":
			serveSSE(w, r)

		case "/healthz":
			w.WriteHeader(http.StatusOK)
			io.WriteString(w, "OK\n")

		default:
			http.NotFound(w, r)
		}
	})

	log.Printf("mock LLM upstream listening on %s", *addr)
	log.Fatal(http.ListenAndServe(*addr, nil))
}

// serveSSE writes a small Server-Sent Events stream: several data events, each
// flushed immediately (double-newline framing), then a terminal data: [DONE].
// The request context is checked between events so a cancelled client stops the
// stream promptly.
func serveSSE(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")

	events := []string{
		`data: {"id":"1","role":"assistant","content":"first chunk"}`,
		`data: {"id":"2","role":"assistant","content":"second chunk"}`,
		`data: {"id":"3","role":"assistant","content":"third chunk"}`,
	}
	for _, ev := range events {
		if r.Context().Err() != nil {
			return
		}
		fmt.Fprintf(w, "%s\n\n", ev)
		flusher.Flush()
	}

	if r.Context().Err() != nil {
		return
	}
	io.WriteString(w, "data: [DONE]\n\n")
	flusher.Flush()
}
