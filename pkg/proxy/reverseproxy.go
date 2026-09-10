package proxy

import (
	"io"
	"net/http"
	"net/http/httputil"
	"net/url"
)

// newReverseProxy builds the ReverseProxy used to forward requests upstream.
// Configuration notes:
//
//   - Rewrite is a pure URL/Host/header transform — it must never read the
//     request body (the handler has already consumed and rewound it). Exactly
//     one of Director/Rewrite may be set, otherwise the ReverseProxy panics.
//   - FlushInterval is -1 so every write to the response is flushed
//     immediately, which is what lets SSE streams through byte-for-byte with no
//     buffering.
//   - DisableCompression is true because the default transport would
//     transparently gzip upstream responses, corrupting SSE byte identity.
//   - ModifyResponse wraps upstream response bodies in a passive usage observer
//     so cost recording can recover usage tokens without buffering the stream
//     (see cost_tracking.go). A nil hook is allowed and disables observation.
//   - ErrorHandler maps upstream dial/transport failures to a 502 JSON body.
func newReverseProxy(upstream *url.URL, modifyResponse func(*http.Response) error) *httputil.ReverseProxy {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.DisableCompression = true

	return &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(upstream)
			pr.SetXForwarded()
			// The agent-id, project, and api-key headers are proxy-internal: they
			// scope loop history, cost accounting, and identity only and must
			// never leak to the upstream LLM API. (The API-key header is already
			// stripped by Middleware; this is a belt-and-braces delete.)
			pr.Out.Header.Del(AgentIDHeader)
			pr.Out.Header.Del(ProjectHeader)
			pr.Out.Header.Del(APIKeyHeader)
		},
		FlushInterval:  -1,
		Transport:      transport,
		ModifyResponse: modifyResponse,
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadGateway)
			io.WriteString(w, `{"error":{"type":"upstream_unreachable","message":"`+err.Error()+`"}}`)
		},
	}
}
