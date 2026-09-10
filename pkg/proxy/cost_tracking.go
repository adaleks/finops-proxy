package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/adaleks/finops-proxy/pkg/circuitbreaker"
	"github.com/adaleks/finops-proxy/pkg/logstream"
	"github.com/adaleks/finops-proxy/pkg/pricing"
)

// usageBufferCap caps the per-response parse state the usage observer retains:
// enough for a trailing SSE usage fragment or one compact JSON body, never the
// whole stream.
const usageBufferCap = 1 << 20 // 1 MiB

// reqMeta carries cost-tracking metadata from the handler to the ModifyResponse
// hook via the request context. It is populated only for hashed POST bodies
// that reached the forwarding path, and only when the handler was built with a
// pricing table and a store.
type reqMeta struct {
	model       string
	promptTok   int
	agent       circuitbreaker.AgentID
	fp          circuitbreaker.Fingerprint
	projectName string
	callerID    string // "" = legacy bucket; captured while the request is alive
	statusCode  int    // upstream response status; captured in modifyResponse
	trackCost   bool
	start       time.Time // captured just before forwarding; drives avg latency
}

// reqMetaKey is the context key used to stash reqMeta.
type reqMetaKey struct{}

// modifyResponse is the ReverseProxy.ModifyResponse hook. For requests that
// opted into cost tracking it wraps the upstream response body in a passive
// usage observer; for everyone else it is a no-op. It never alters the response
// status or the bytes on the wire.
func (h *handler) modifyResponse(resp *http.Response) error {
	meta, ok := resp.Request.Context().Value(reqMetaKey{}).(reqMeta)
	if !ok || !meta.trackCost {
		return nil
	}
	// Capture the upstream status before wrapping the body so onCost can emit it
	// on request.complete / cost_alert. The upstream response may be a 4xx or
	// 5xx, which the live log surfaces as-is.
	meta.statusCode = resp.StatusCode
	isSSE := strings.Contains(strings.ToLower(resp.Header.Get("Content-Type")), "text/event-stream")
	resp.Body = &usageTrackingBody{
		rc:     resp.Body,
		meta:   meta,
		onCost: h.onCost,
		isSSE:  isSSE,
	}
	return nil
}

// onCost finalizes cost recording for one proxied call. usageTrackingBody calls
// it exactly once per response, on Close or EOF. Recording is skipped when the
// handler was built without pricing/store (nil-guard).
func (h *handler) onCost(meta reqMeta, promptTok, completionTok int) {
	// Observability: record the forwarded request's latency and token volume
	// regardless of whether cost recording is wired (traffic, not money).
	h.metrics.markForwarded(time.Since(meta.start), promptTok, completionTok)

	if h.price == nil || h.store == nil {
		return
	}
	mp, _ := h.price.PriceFor(meta.model)
	cost := pricing.Cost(mp, promptTok, completionTok)
	// Use context.Background(): by the time the response body is closed the
	// request context is often already canceled (or about to be), and this
	// write must still land.
	_ = h.store.LogRequest(context.Background(), RequestRecord{
		AgentID:          string(meta.agent),
		KeyID:            meta.callerID,
		ProjectName:      meta.projectName,
		Model:            meta.model,
		PromptTokens:     int64(promptTok),
		CompletionTokens: int64(completionTok),
		CostUSD:          cost,
		IsLoopBlocked:    false,
		Timestamp:        time.Now().Unix(),
	})
	// Budget alerting runs strictly after the write above, so this request's
	// actual cost is already included in SumActualCostSince. A blocked request
	// never bills, so it never reaches onCost and never triggers a budget check.
	h.checkBudget()
	// Live log: one request.complete event per forwarded call, carrying the
	// captured status, token counts and cost (µUSD). The hub resolves the name
	// from the caller id when a resolver is wired.
	h.publishLog(logstream.LogEvent{
		Type:             logstream.TypeRequestComplete,
		Level:            "info",
		Timestamp:        time.Now().Unix(),
		TenantID:         meta.callerID,
		AgentID:          string(meta.agent),
		Model:            meta.model,
		ProjectName:      meta.projectName,
		PromptTokens:     int64(promptTok),
		CompletionTokens: int64(completionTok),
		CostMicroUSD:     cost,
		StatusCode:       meta.statusCode,
	})
	// Per-request cost alert: a single completed request whose cost reaches the
	// configured threshold emits cost_alert. 0 disables it; the reader is a
	// lock-free atomic read on the settings service.
	if h.costThreshold != nil {
		if thr := h.costThreshold.CostAlertThresholdMicro(); thr > 0 && cost >= thr {
			h.publishLog(logstream.LogEvent{
				Type:         logstream.TypeCostAlert,
				Level:        "warn",
				Timestamp:    time.Now().Unix(),
				TenantID:     meta.callerID,
				AgentID:      string(meta.agent),
				Model:        meta.model,
				ProjectName:  meta.projectName,
				CostMicroUSD: cost,
				StatusCode:   meta.statusCode,
			})
		}
	}
}

// usageTrackingBody is a bounded, non-buffering observer over an upstream
// response body. Bytes pass through unchanged and immediately (the proxy's
// FlushInterval=-1 is preserved); meanwhile it inspects a copy of the bytes to
// recover usage.prompt_tokens / usage.completion_tokens for cost recording. It
// retains at most usageBufferCap bytes of parse state — a trailing usage
// fragment or one JSON body — never the whole stream.
type usageTrackingBody struct {
	rc     io.ReadCloser
	meta   reqMeta
	onCost func(meta reqMeta, promptTok, completionTok int)
	isSSE  bool

	once sync.Once

	// SSE parse state.
	pending []byte // current incomplete line
	overCap bool   // parse buffers exceeded the cap → stop parsing
	haveUse bool   // a usage object was seen in the stream (authoritative)
	useP    int    // usage.prompt_tokens
	useC    int    // usage.completion_tokens
	deltaCh int    // accumulated choices[0].delta.content length

	// Non-streaming JSON parse state (the whole body, capped).
	jsonBuf []byte
}

func (b *usageTrackingBody) Read(p []byte) (int, error) {
	n, err := b.rc.Read(p)
	if n > 0 {
		b.parseChunk(p[:n])
	}
	if err == io.EOF {
		b.finalize()
	}
	return n, err
}

func (b *usageTrackingBody) Close() error {
	b.finalize()
	return b.rc.Close()
}

func (b *usageTrackingBody) parseChunk(chunk []byte) {
	if b.isSSE {
		b.feedSSE(chunk)
	} else {
		b.feedJSON(chunk)
	}
}

func (b *usageTrackingBody) feedJSON(chunk []byte) {
	if b.overCap {
		return
	}
	if len(b.jsonBuf)+len(chunk) > usageBufferCap {
		b.overCap = true
		b.jsonBuf = nil
		return
	}
	b.jsonBuf = append(b.jsonBuf, chunk...)
}

func (b *usageTrackingBody) feedSSE(chunk []byte) {
	if b.overCap {
		return
	}
	b.pending = append(b.pending, chunk...)
	if len(b.pending) > usageBufferCap {
		b.overCap = true
		b.pending = nil
		if !b.haveUse {
			b.deltaCh = 0 // partial delta accumulation is untrustworthy
		}
		return
	}
	for {
		idx := bytes.IndexByte(b.pending, '\n')
		if idx < 0 {
			return
		}
		line := b.pending[:idx]
		b.pending = b.pending[idx+1:]
		b.processLine(line)
	}
}

func (b *usageTrackingBody) processLine(line []byte) {
	line = bytes.TrimSuffix(line, []byte("\r"))
	if !bytes.HasPrefix(line, []byte("data:")) {
		return
	}
	payload := bytes.TrimSpace(line[len("data:"):])
	if len(payload) == 0 {
		return
	}
	if string(payload) == "[DONE]" {
		return
	}
	b.parseSSEPayload(payload)
}

func (b *usageTrackingBody) parseSSEPayload(payload []byte) {
	if b.overCap || b.haveUse {
		return
	}
	var chunk struct {
		Choices []struct {
			Delta struct {
				Content string `json:"content"`
			} `json:"delta"`
		} `json:"choices"`
		Usage *struct {
			PromptTokens     int64 `json:"prompt_tokens"`
			CompletionTokens int64 `json:"completion_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(payload, &chunk); err != nil {
		return // not a JSON data line; ignore
	}
	if chunk.Usage != nil {
		b.useP = int(chunk.Usage.PromptTokens)
		b.useC = int(chunk.Usage.CompletionTokens)
		b.haveUse = true
		return
	}
	for _, c := range chunk.Choices {
		b.deltaCh += len(c.Delta.Content)
	}
}

// finalize computes the token counts and invokes onCost exactly once, even for
// aborted connections. Any parse failure falls back to estimates/zero and can
// never turn a 200 into an error.
func (b *usageTrackingBody) finalize() {
	b.once.Do(func() {
		if b.onCost == nil {
			return
		}
		promptTok, completionTok := b.tokens()
		b.onCost(b.meta, promptTok, completionTok)
	})
}

func (b *usageTrackingBody) tokens() (int, int) {
	if b.isSSE {
		if b.haveUse {
			return b.useP, b.useC
		}
		// Fall back: prompt from the request-body estimate, completion from the
		// accumulated delta content length / 4.0.
		return b.meta.promptTok, estimateCompletionTokens(b.deltaCh)
	}
	// Non-streaming JSON: the body was buffered (capped); parse usage.
	if !b.overCap {
		if pt, ct, ok := parseJSONUsage(b.jsonBuf); ok {
			return pt, ct
		}
	}
	return b.meta.promptTok, 0
}

// parseJSONUsage extracts usage.prompt_tokens / usage.completion_tokens from a
// complete non-streaming JSON response body. ok=false when absent/unparseable.
func parseJSONUsage(body []byte) (int, int, bool) {
	var doc struct {
		Usage *struct {
			PromptTokens     int64 `json:"prompt_tokens"`
			CompletionTokens int64 `json:"completion_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(body, &doc); err != nil {
		return 0, 0, false
	}
	if doc.Usage == nil {
		return 0, 0, false
	}
	return int(doc.Usage.PromptTokens), int(doc.Usage.CompletionTokens), true
}

// estimateCompletionTokens estimates completion tokens from accumulated delta
// content length using the 4.0 chars-per-token heuristic.
func estimateCompletionTokens(chars int) int {
	if chars <= 0 {
		return 0
	}
	return (chars + 3) / 4 // ceil(len/4.0)
}
