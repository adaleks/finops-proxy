package circuitbreaker

import (
	"testing"
	"time"
)

// benchmarkBody is a representative multi-KB chat-completions body carrying
// dynamic values (numbers, UUID, timestamp) and a tool surface, so every stage
// of the pipeline does real work.
func benchmarkBody() []byte {
	return []byte(`{"model":"gpt-4o","attempt":123,"request_id":"123e4567-e89b-12d3-a456-426614174000","timestamp":"2026-09-09T12:34:56Z","messages":[{"role":"user","content":"please analyze this document and summarize the key points"},{"role":"assistant","tool_calls":[{"function":{"name":"query_db"}},{"function":{"name":"edit_file"}}]}],"tools":[{"type":"function","function":{"name":"search"}}]}`)
}

// Each benchmark uses a static clock and thresholds high enough that nothing
// blocks, so it measures the full steady-state hot path (normalize + hash +
// compare + ring update), not the block short-circuit.

func BenchmarkStructuralHasher(b *testing.B) {
	s := NewStructuralHasher(StructuralConfig{Limit: 1 << 30})
	s.now = staticClock(time.Unix(1_000_000, 0))
	body := benchmarkBody()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		s.CheckPayload(AgentID("a"), Fingerprint("fp"), body)
	}
}

func BenchmarkNormalizePayload(b *testing.B) {
	body := benchmarkBody()
	buf := make([]byte, 0, 4096)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		buf = buf[:0]
		normalizePayload(&buf, body)
	}
}

func BenchmarkMinHash(b *testing.B) {
	form := norm(string(benchmarkBody()))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		minHash(form)
	}
}

func BenchmarkToolMatcher(b *testing.B) {
	m := NewToolMatcher(ToolConfig{Limit: 1 << 30})
	m.now = staticClock(time.Unix(1_000_000, 0))
	body := benchmarkBody()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		m.CheckPayload(AgentID("a"), Fingerprint("fp"), body)
	}
}

func BenchmarkVelocityBreaker(b *testing.B) {
	v := NewVelocityBreaker(VelocityConfig{BurstTokens: 1 << 62, SpikeFactor: 1e9})
	v.now = staticClock(time.Unix(1_000_000, 0))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		v.CheckVelocity(AgentID("a"), 1000)
	}
}

func BenchmarkPipeline(b *testing.B) {
	p := NewPipeline(
		NewHashDetector(time.Minute, 1<<30),
		WithStructural(NewStructuralHasher(StructuralConfig{Limit: 1 << 30})),
		WithTools(NewToolMatcher(ToolConfig{Limit: 1 << 30})),
		WithVelocity(NewVelocityBreaker(VelocityConfig{BurstTokens: 1 << 62, SpikeFactor: 1e9})),
	)
	body := benchmarkBody()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		p.CheckPayload(AgentID("a"), Fingerprint("fp"), body)
	}
}
