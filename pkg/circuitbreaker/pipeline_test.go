package circuitbreaker

import (
	"strconv"
	"testing"
	"time"
)

func TestPipelineCheckIsExactHashOnly(t *testing.T) {
	hd := NewHashDetector(time.Minute, 2)
	hd.now = staticClock(time.Unix(1_000_000, 0))
	p := NewPipeline(hd)
	fp := Fingerprint("x")

	if res := p.Check(testAgent, fp); res.Block {
		t.Fatalf("first sighting must pass, got %+v", res)
	}
	if res := p.Check(testAgent, fp); !res.Block {
		t.Fatalf("second sighting must block, got %+v", res)
	}
	if res := p.Check(testAgent, fp); res.Block && res.Reason != ReasonExactHash {
		t.Fatalf("reason = %q, want %q", res.Reason, ReasonExactHash)
	}
}

func TestPipelineExactHashWinsOverStructural(t *testing.T) {
	hd := NewHashDetector(time.Minute, 2)
	hd.now = staticClock(time.Unix(1_000_000, 0))
	p := NewPipeline(hd, WithStructural(NewStructuralHasher(StructuralConfig{})))
	body := []byte(`{"q":"hello world"}`)
	fp := Fingerprint("same")

	p.CheckPayload(testAgent, fp, body) // exact count=1, structural 1st sighting
	res := p.CheckPayload(testAgent, fp, body)
	if !res.Block || res.Reason != ReasonExactHash {
		t.Fatalf("exact hash must short-circuit and win, got %+v", res)
	}
}

func TestPipelineStructuralStage(t *testing.T) {
	hd := NewHashDetector(time.Minute, 1<<30) // exact never blocks
	hd.now = staticClock(time.Unix(1_000_000, 0))
	p := NewPipeline(hd, WithStructural(NewStructuralHasher(StructuralConfig{Limit: 2})))
	agent := AgentID("a")

	body := func(n int) []byte {
		return []byte(`{"model":"gpt-4o","n":` + strconv.Itoa(n) + `,"content":"same prompt please retry"}`)
	}
	for i := 0; i < 2; i++ {
		if res := p.CheckPayload(agent, Fingerprint("fp"), body(i)); res.Block {
			t.Fatalf("sighting %d must pass, got %+v", i+1, res)
		}
	}
	if res := p.CheckPayload(agent, Fingerprint("fp"), body(2)); !res.Block || res.Reason != ReasonStructural {
		t.Fatalf("3rd structural sighting must block with structural reason, got %+v", res)
	}
}

func TestPipelineToolStage(t *testing.T) {
	hd := NewHashDetector(time.Minute, 1<<30)
	hd.now = staticClock(time.Unix(1_000_000, 0))
	p := NewPipeline(hd, WithTools(NewToolMatcher(ToolConfig{})))
	agent := AgentID("a")
	body := []byte(`{"messages":[{"role":"assistant","tool_calls":[{"function":{"name":"A"}},{"function":{"name":"B"}}]}]}`)

	p.CheckPayload(agent, Fingerprint("fp"), body)
	p.CheckPayload(agent, Fingerprint("fp"), body)
	if res := p.CheckPayload(agent, Fingerprint("fp"), body); !res.Block || res.Reason != ReasonToolCycle {
		t.Fatalf("3rd request must block with tool-cycle reason, got %+v", res)
	}
}

func TestPipelineVelocityStage(t *testing.T) {
	hd := NewHashDetector(time.Minute, 1<<30)
	hd.now = staticClock(time.Unix(1_000_000, 0))
	p := NewPipeline(hd, WithVelocity(NewVelocityBreaker(VelocityConfig{Window: time.Minute, ShortWindow: 5 * time.Second, BurstTokens: 1000})))
	agent := AgentID("a")

	// estimateTokens uses len(body)/4; a 2000-byte body ≈ 500 tokens.
	body := make([]byte, 2000)
	for i := range body {
		body[i] = 'x'
	}
	p.CheckPayload(agent, Fingerprint("f1"), body)
	p.CheckPayload(agent, Fingerprint("f2"), body)
	if res := p.CheckPayload(agent, Fingerprint("f3"), body); !res.Block || res.Reason != ReasonVelocity {
		t.Fatalf("3rd velocity sighting must block with velocity reason, got %+v", res)
	}
}

func TestPipelineResetFansOut(t *testing.T) {
	hd := NewHashDetector(time.Minute, 2)
	hd.now = staticClock(time.Unix(1_000_000, 0))
	p := NewPipeline(
		hd,
		WithStructural(NewStructuralHasher(StructuralConfig{})),
		WithTools(NewToolMatcher(ToolConfig{})),
		WithVelocity(NewVelocityBreaker(VelocityConfig{})),
	)
	agent := AgentID("a")
	body := []byte(`{"messages":[{"role":"assistant","tool_calls":[{"function":{"name":"A"}},{"function":{"name":"B"}}]}]}`)

	p.CheckPayload(agent, Fingerprint("fp"), body)
	if n := p.Reset(agent); n == 0 {
		t.Fatalf("Reset should remove entries from at least one detector")
	}
	// After reset, exact hash's first sighting passes again.
	if res := p.CheckPayload(agent, Fingerprint("fp"), body); res.Block {
		t.Fatalf("after reset must pass, got %+v", res)
	}
}

func TestPipelineNilSubDetectors(t *testing.T) {
	hd := NewHashDetector(time.Minute, 2)
	hd.now = staticClock(time.Unix(1_000_000, 0))
	p := NewPipeline(hd) // no sub-detectors

	body := []byte(`{"q":"hello"}`)
	for i := 0; i < 5; i++ {
		_ = p.CheckPayload(testAgent, Fingerprint("x"), body) // only exact hash runs
	}
	if res := p.Check(testAgent, Fingerprint("x")); !res.Block {
		t.Fatalf("plain Check must still block via exact hash, got %+v", res)
	}
}

func TestPipelineEstimateTokensFallback(t *testing.T) {
	hd := NewHashDetector(time.Minute, 2)
	p := NewPipeline(hd)
	if got := p.estimateTokens([]byte("abcd")); got != 1 {
		t.Fatalf("estimateTokens(4 bytes) = %d, want 1 (ceil(4/4))", got)
	}
	if got := p.estimateTokens([]byte("abcde")); got != 2 {
		t.Fatalf("estimateTokens(5 bytes) = %d, want 2", got)
	}
}

// Compile-time assertion that Pipeline satisfies both contracts (guards against
// accidental interface drift).
var (
	_ Detector       = (*Pipeline)(nil)
	_ PayloadChecker = (*Pipeline)(nil)
)
