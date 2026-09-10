package proxy

// PayloadEvent is the raw request payload handed to an async semantic consumer.
//
// Body is the fully-read request body. The handler rewinds a separate copy for
// the upstream, so the receiver may retain Body; the receiver MUST NOT mutate
// it (the handler may still be reading from the same backing array during the
// rewind).
//
// KeyID is the caller/tenant id ("" = anonymous/legacy bucket). Fingerprint is
// the lowercase SHA-256 hex the detector was handed. Timestamp is Unix epoch
// seconds (UTC).
type PayloadEvent struct {
	AgentID     string
	KeyID       string
	Project     string
	Model       string
	Fingerprint string
	Body        []byte
	Timestamp   int64
}

// Emitter is the seam through which the handler publishes raw payloads to an
// async consumer (the EE semantic worker pool). The contract mirrors Notifier:
// Emit MUST return promptly (never block on the worker, the network, or a full
// queue), MUST be safe for concurrent use, and MUST NOT panic. Implementations
// buffer internally and drop on a full queue.
type Emitter interface {
	Emit(PayloadEvent)
}

// WithEmitter wires an Emitter into the handler. A nil emitter (or omitting the
// option) disables emission: the request path is byte-for-byte unchanged.
func WithEmitter(e Emitter) HandlerOption {
	return func(h *handler) { h.emitter = e }
}
