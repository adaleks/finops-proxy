// Package memory implements the zero-dependency reference store for the
// open-source core: request logging, the budget ledger, settings, and caller
// resolution all live in process memory. It is the offline runtime behind
// `cmd/proxy` when no SQLite path is configured, and the reference
// implementation for tests. Nothing here imports anything beyond stdlib and the
// core pkg/proxy seams.
package memory

import (
	"context"
	"sync"

	"github.com/adaleks/finops-proxy/pkg/proxy"
)

// DefaultLoopThreshold mirrors the detector's repeat-threshold default (first
// sighting passes, the second repeat blocks). It seeds SettingsRow until an
// UpdateSettings call.
const DefaultLoopThreshold = 2

// KeyStore is an in-memory proxy.CallerStore: a SHA-256 key-hash -> Caller map
// for offline per-key budgets, seeded from a keys file or by tests. It is safe
// for concurrent use.
type KeyStore struct {
	mu sync.RWMutex
	m  map[string]proxy.Caller
}

// NewKeyStore returns an empty KeyStore.
func NewKeyStore() *KeyStore {
	return &KeyStore{m: make(map[string]proxy.Caller)}
}

// Set binds (or replaces) the caller for a key hash. hash is the lowercase hex
// SHA-256 of the raw key (proxy.HashAPIKey).
func (k *KeyStore) Set(hash string, c proxy.Caller) {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.m[hash] = c
}

// CallerByKeyHash implements proxy.CallerStore. An unknown hash surfaces as
// proxy.ErrInvalidKey so the middleware answers 401 (fail-closed).
func (k *KeyStore) CallerByKeyHash(_ context.Context, hash string) (proxy.Caller, error) {
	k.mu.RLock()
	defer k.mu.RUnlock()
	c, ok := k.m[hash]
	if !ok {
		return proxy.Caller{}, proxy.ErrInvalidKey
	}
	return c, nil
}

// Store is the in-memory proxy.Store. It is safe for concurrent use: a single
// mutex serializes every read and write.
type Store struct {
	mu       sync.Mutex
	logs     []proxy.RequestRecord
	settings proxy.SettingsRow
	haveSet  bool
	callers  *KeyStore
}

// New returns an empty in-memory Store. It satisfies proxy.Store and, through
// its embedded caller map, proxy.CallerStore.
func New() *Store {
	return &Store{callers: NewKeyStore()}
}

// Set binds (or replaces) the caller for a key hash. It matches KeyStore.Set, so
// cmd/proxy's loadKeys can seed the in-memory store (the -keys flag) through the
// same seam as KeyStore. hash is the lowercase hex SHA-256 of the raw key.
func (s *Store) Set(hash string, c proxy.Caller) {
	s.callers.Set(hash, c)
}

// SetCaller binds (or replaces) the caller for a key hash, for offline per-key
// budgets. It is an alias of Set kept for existing callers.
func (s *Store) SetCaller(hash string, c proxy.Caller) {
	s.Set(hash, c)
}

// CallerByKeyHash implements proxy.CallerStore via the embedded KeyStore.
func (s *Store) CallerByKeyHash(ctx context.Context, hash string) (proxy.Caller, error) {
	return s.callers.CallerByKeyHash(ctx, hash)
}

// LogRequest implements proxy.RequestLogStore by appending one record.
func (s *Store) LogRequest(_ context.Context, r proxy.RequestRecord) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.logs = append(s.logs, r)
	return nil
}

// SumActualCostSince implements proxy.BudgetLedger: the sum of actual
// (non-loop-blocked) cost at or after since, across every caller.
func (s *Store) SumActualCostSince(_ context.Context, since int64) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var total int64
	for _, r := range s.logs {
		if r.IsLoopBlocked || r.Timestamp < since {
			continue
		}
		total += r.CostUSD
	}
	return total, nil
}

// SumActualCostForKeySince implements proxy.BudgetLedger: the sum of actual
// cost for one caller's requests at or after since. The empty keyID selects the
// anonymous/legacy bucket.
func (s *Store) SumActualCostForKeySince(_ context.Context, keyID string, since int64) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var total int64
	for _, r := range s.logs {
		if r.IsLoopBlocked || r.Timestamp < since || r.KeyID != keyID {
			continue
		}
		total += r.CostUSD
	}
	return total, nil
}

// GetSettings implements proxy.SettingsStore, returning the current snapshot or
// the defaults before the first update.
func (s *Store) GetSettings(_ context.Context) (proxy.SettingsRow, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.haveSet {
		return proxy.SettingsRow{
			LoopThresholdCount:      DefaultLoopThreshold,
			CostAlertThresholdMicro: 0,
		}, nil
	}
	return s.settings, nil
}

// UpdateSettings implements proxy.SettingsStore by replacing the snapshot.
func (s *Store) UpdateSettings(_ context.Context, row proxy.SettingsRow) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.settings = row
	s.haveSet = true
	return nil
}

// Logs returns a copy of every recorded request, oldest first. It is a test and
// diagnostics helper, not part of any seam.
func (s *Store) Logs() []proxy.RequestRecord {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]proxy.RequestRecord, len(s.logs))
	copy(out, s.logs)
	return out
}

// Compile-time assertions that the in-memory store satisfies the seams.
var (
	_ proxy.Store       = (*Store)(nil)
	_ proxy.CallerStore = (*Store)(nil)
	_ proxy.CallerStore = (*KeyStore)(nil)
)
