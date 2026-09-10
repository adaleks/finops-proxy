# CE Core Architecture Rules

## Mandatory Constraints
1. Zero External Dependencies: Use Go standard library or high-performance zero-dep packages.
2. Ultra-Low Overhead: Any middleware or detection logic must execute in < 1ms.
3. Memory Footprint: Use `sync.Pool` for allocations; avoid high GC pressure in the hot-path.
4. Offline First: No external calls to third-party APIs or embedding providers in CE.
5. Interface Isolation: All logic in `pkg/circuitbreaker/` and `pkg/proxy/` must depend strictly on core interfaces.
