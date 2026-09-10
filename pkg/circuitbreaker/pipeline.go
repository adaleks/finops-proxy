package circuitbreaker

// Pipeline composes the exact-hash detector with the three dynamic detectors —
// structural hashing, tool-cycle matching, and token-velocity breaking — into a
// single check-and-record entry point.
//
// It satisfies both Detector and PayloadChecker:
//
//   - Check (the legacy contract) delegates to the exact-hash detector only, so
//     a caller that never observes the body gets exactly the old behavior.
//   - CheckPayload (the body-aware capability) runs the full pipeline in
//     cost/precision order, short-circuiting on the first block.
//
// Reset fans out to every sub-detector so clearing an agent's history clears
// all loop classes together.
type Pipeline struct {
	exact      Detector
	structural *StructuralHasher
	tools      *ToolMatcher
	velocity   *VelocityBreaker
	estimate   TokenEstimator
}

// PipelineOption customizes a Pipeline. A nil sub-detector disables that stage.
type PipelineOption func(*Pipeline)

// WithStructural enables structural near-duplicate detection.
func WithStructural(s *StructuralHasher) PipelineOption {
	return func(p *Pipeline) { p.structural = s }
}

// WithTools enables tool-cycle detection.
func WithTools(t *ToolMatcher) PipelineOption {
	return func(p *Pipeline) { p.tools = t }
}

// WithVelocity enables token-velocity breaking.
func WithVelocity(v *VelocityBreaker) PipelineOption {
	return func(p *Pipeline) { p.velocity = v }
}

// WithTokenEstimator sets the prompt-token estimator used by the velocity
// stage. A nil estimator (the default) falls back to a byte-length heuristic.
func WithTokenEstimator(f TokenEstimator) PipelineOption {
	return func(p *Pipeline) { p.estimate = f }
}

// NewPipeline builds a Pipeline around the exact-hash detector. exact must be
// non-nil; the sub-detectors are optional.
func NewPipeline(exact Detector, opts ...PipelineOption) *Pipeline {
	p := &Pipeline{exact: exact}
	for _, opt := range opts {
		opt(p)
	}
	return p
}

// Check implements the Detector contract: exact-hash matching only.
func (p *Pipeline) Check(agent AgentID, fp Fingerprint) CheckResult {
	return p.exact.Check(agent, fp)
}

// CheckPayload implements the PayloadChecker capability: the full pipeline.
func (p *Pipeline) CheckPayload(agent AgentID, fp Fingerprint, body []byte) CheckResult {
	if res := p.exact.Check(agent, fp); res.Block {
		return res
	}
	if p.structural != nil {
		if res := p.structural.CheckPayload(agent, fp, body); res.Block {
			return res
		}
	}
	if p.tools != nil {
		if res := p.tools.CheckPayload(agent, fp, body); res.Block {
			return res
		}
	}
	if p.velocity != nil {
		if res := p.velocity.CheckVelocity(agent, p.estimateTokens(body)); res.Block {
			return res
		}
	}
	return CheckResult{}
}

// Reset clears every sub-detector's state for one agent and returns the total
// number of entries removed.
func (p *Pipeline) Reset(agent AgentID) int {
	n := p.exact.Reset(agent)
	if p.structural != nil {
		n += p.structural.Reset(agent)
	}
	if p.tools != nil {
		n += p.tools.Reset(agent)
	}
	if p.velocity != nil {
		n += p.velocity.Reset(agent)
	}
	return n
}

// estimateTokens maps a body to its prompt-token estimate, falling back to a
// chars-per-token (4) heuristic when no estimator is wired.
func (p *Pipeline) estimateTokens(body []byte) int64 {
	if p.estimate != nil {
		return int64(p.estimate(body))
	}
	return (int64(len(body)) + 3) / 4
}

// Compile-time assertions.
var _ Detector = (*Pipeline)(nil)
var _ PayloadChecker = (*Pipeline)(nil)
