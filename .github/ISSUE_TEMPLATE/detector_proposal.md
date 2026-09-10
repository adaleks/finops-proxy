---
name: 🛡️ Loop-detector proposal
about: Propose a new deterministic, non-LLM loop-detection signal
title: "[detector] "
labels: enhancement, circuitbreaker
assignees: ""
---

### Failure mode
What real-world loop does this catch that the existing detectors miss?
(exact hash · structural MinHash · tool-cycle · token velocity)

### Signal & algorithm
Describe the deterministic signal (no embeddings, no external calls) and the math.

### Hot-path budget
Expected worst-case work and its < 1 ms bound. Where are the allocations, and are they pooled?

### False-positive analysis
When would this fire on legitimate traffic? How is that mitigated (thresholds, confirmations)?

### Interface impact
Does it satisfy `Detector` or the optional `PayloadChecker` capability in
`pkg/circuitbreaker/detector.go`? Any new config knobs + defaults?

### References
Links to reports, papers, or observed logs (redacted).
