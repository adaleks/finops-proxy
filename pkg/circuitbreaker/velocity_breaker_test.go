package circuitbreaker

import (
	"sync"
	"testing"
	"time"
)

func TestVelocityBurstBlocks(t *testing.T) {
	v := NewVelocityBreaker(VelocityConfig{Window: time.Minute, ShortWindow: 5 * time.Second, BurstTokens: 1000})
	v.now = staticClock(time.Unix(1_000_000, 0))
	agent := AgentID("a")

	v.CheckVelocity(agent, 400)
	v.CheckVelocity(agent, 400)
	if res := v.CheckVelocity(agent, 400); !res.Block {
		t.Fatalf("burst of 1200 > 1000 must block, got %+v", res)
	}
	if res := v.CheckVelocity(agent, 400); res.Block && res.Reason != ReasonVelocity {
		t.Fatalf("reason = %q, want %q", res.Reason, ReasonVelocity)
	}
}

func TestVelocitySteadyRatePasses(t *testing.T) {
	v := NewVelocityBreaker(VelocityConfig{Window: 30 * time.Second, ShortWindow: 5 * time.Second, BurstTokens: 100_000, SpikeFactor: 5.0})
	clock := newFakeClock(time.Unix(1_000_000, 0))
	v.now = clock.now
	agent := AgentID("a")

	// Steady 100 tokens/sec for 60s: short rate == long rate, no burst.
	for i := 0; i < 60; i++ {
		clock.advance(time.Second)
		if res := v.CheckVelocity(agent, 100); res.Block {
			t.Fatalf("steady rate must not block at %ds: %+v", i+1, res)
		}
	}
}

func TestVelocitySpikeBlocks(t *testing.T) {
	v := NewVelocityBreaker(VelocityConfig{Window: 30 * time.Second, ShortWindow: 5 * time.Second, BurstTokens: 1_000_000, SpikeFactor: 5.0})
	clock := newFakeClock(time.Unix(1_000_000, 0))
	v.now = clock.now
	agent := AgentID("a")

	// Warm up: steady 10 tokens/sec for 30s (fills the long window).
	for i := 0; i < 30; i++ {
		clock.advance(time.Second)
		if res := v.CheckVelocity(agent, 10); res.Block {
			t.Fatalf("warm-up must not block at %ds: %+v", i+1, res)
		}
	}
	// A 50_000-token spike in 1s pushes shortRate/longRate > 5.
	clock.advance(time.Second)
	if res := v.CheckVelocity(agent, 50_000); !res.Block {
		t.Fatalf("acceleration spike must block, got %+v", res)
	}
}

func TestVelocityWindowSlideExpires(t *testing.T) {
	v := NewVelocityBreaker(VelocityConfig{Window: 10 * time.Second, ShortWindow: 2 * time.Second, BurstTokens: 1000, SpikeFactor: 1000.0})
	clock := newFakeClock(time.Unix(1_000_000, 0))
	v.now = clock.now
	agent := AgentID("a")

	v.CheckVelocity(agent, 400)
	clock.advance(11 * time.Second) // past the 10s window
	if res := v.CheckVelocity(agent, 400); res.Block {
		t.Fatalf("after window expiry the first event must not count toward burst, got %+v", res)
	}
}

func TestVelocityResetAndIsolation(t *testing.T) {
	v := NewVelocityBreaker(VelocityConfig{Window: time.Minute, ShortWindow: 5 * time.Second, BurstTokens: 1000})
	v.now = staticClock(time.Unix(1_000_000, 0))
	a, b := AgentID("a"), AgentID("b")

	v.CheckVelocity(a, 400)
	v.CheckVelocity(a, 400)
	// b is independent and must not be contaminated by a.
	if res := v.CheckVelocity(b, 400); res.Block {
		t.Fatalf("agent b must not be contaminated by a, got %+v", res)
	}
	// a blocks.
	if res := v.CheckVelocity(a, 400); !res.Block {
		t.Fatalf("agent a must block, got %+v", res)
	}
	// Reset clears a.
	if n := v.Reset(a); n != 1 {
		t.Fatalf("Reset removed %d, want 1", n)
	}
	if res := v.CheckVelocity(a, 400); res.Block {
		t.Fatalf("after reset, a must pass, got %+v", res)
	}
}

func TestVelocityFirstBlockTransition(t *testing.T) {
	v := NewVelocityBreaker(VelocityConfig{Window: 10 * time.Second, ShortWindow: 2 * time.Second, BurstTokens: 1000})
	clock := newFakeClock(time.Unix(1_000_000, 0))
	v.now = clock.now
	agent := AgentID("a")

	v.CheckVelocity(agent, 400)
	v.CheckVelocity(agent, 400)
	res := v.CheckVelocity(agent, 400)
	if !res.Block || !res.FirstBlock {
		t.Fatalf("3rd sighting must be the first block, got %+v", res)
	}
	// Repeat block is not first.
	if res := v.CheckVelocity(agent, 400); res.Block && res.FirstBlock {
		t.Fatalf("repeat block must not carry FirstBlock=true, got %+v", res)
	}
	// Advance past the window: the burst clears, and blockedAt is reset so the
	// next genuine spike would re-fire FirstBlock.
	clock.advance(11 * time.Second)
	if res := v.CheckVelocity(agent, 10); res.Block {
		t.Fatalf("after window expiry must not block, got %+v", res)
	}
}

func TestVelocityConcurrency(t *testing.T) {
	v := NewVelocityBreaker(VelocityConfig{Window: time.Minute, ShortWindow: 5 * time.Second, BurstTokens: 1_000_000, SpikeFactor: 1e9})
	v.now = staticClock(time.Unix(1_000_000, 0))
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 200; j++ {
				v.CheckVelocity(AgentID("a"), 10)
			}
		}()
	}
	wg.Wait()
	if res := v.CheckVelocity(AgentID("fresh"), 1); res.Block {
		t.Fatalf("fresh agent must pass after churn, got %+v", res)
	}
}
