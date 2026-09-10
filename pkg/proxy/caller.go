// Package proxy implements the FinOps circuit-breaker reverse proxy: the lean,
// Apache-2.0 core engine that fingerprints request bodies, rejects looped
// repeats, and enforces token-budget caps.
//
// It depends only on the sibling core packages (circuitbreaker, pricing,
// logstream) and a small set of seams declared in this package. Every
// enterprise concern — multi-tenant hierarchy, SSO/session auth, webhook
// delivery, dashboard persistence — is pushed behind those seams and supplied
// by the enterprise module, never imported here.
package proxy

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
)

// APIKeyHeader is the request header carrying the caller's API key. It is
// stripped before the request is forwarded upstream (it is proxy-internal).
const APIKeyHeader = "X-FinOps-API-Key"

// ErrInvalidKey is returned when a raw key is empty or its hash matches no
// caller. Every store failure surfaces as ErrInvalidKey so the middleware
// always answers 401 (fail-closed) and never leaks whether a caller exists.
var ErrInvalidKey = errors.New("proxy: invalid API key")

// Caller is the identified caller of the proxy. Money is integer micro-dollars
// (µUSD). It is the flat, enterprise-agnostic projection of whatever identity
// system the caller module layers on top (an EE tenant, an API key, a
// service account). A budget of 0 on a dimension means unlimited.
type Caller struct {
	ID   string // caller id; always non-empty for authenticated requests
	Name string // display name; "" for anonymous callers

	// MonthlyBudgetMicro is the per-caller monthly spend cap in µUSD; 0 =
	// unlimited (the monthly gate is skipped).
	MonthlyBudgetMicro int64

	// DailyBudgetMicro is the per-caller daily spend cap in µUSD; 0 = unlimited
	// (the daily gate is skipped).
	DailyBudgetMicro int64
}

// CallerStore is the narrow persistence seam through which the key
// authenticator resolves a raw API key to a Caller. *store/sqlite.DB and
// *store/memory.Store implement it; the enterprise module supplies its own
// (flattening its multi-tenant hierarchy into a flat Caller).
type CallerStore interface {
	// CallerByKeyHash returns the Caller for the given key hash. It MUST return
	// ErrInvalidKey when the hash is unknown; every other failure is returned
	// unwrapped so the caller can decide how to surface it.
	CallerByKeyHash(ctx context.Context, hash string) (Caller, error)
}

// Authenticator verifies a raw API key and returns the matching Caller. It is
// the core-shaped equivalent of the enterprise auth.Authenticator: hash the
// key, look it up, carry the Caller in the request context.
type Authenticator interface {
	Authenticate(rawKey string) (Caller, error)
}

// authenticator is the concrete Authenticator backed by a CallerStore.
type authenticator struct {
	store CallerStore
}

// NewAuthenticator returns an Authenticator that resolves raw keys against
// store.
func NewAuthenticator(store CallerStore) Authenticator {
	return &authenticator{store: store}
}

// Authenticate verifies rawKey against the store. An empty key fails fast
// before hashing; an unknown key runs a dummy constant-time comparison on the
// miss path so hit and miss paths take approximately the same time.
func (a *authenticator) Authenticate(rawKey string) (Caller, error) {
	if rawKey == "" {
		return Caller{}, ErrInvalidKey
	}
	hash := HashAPIKey(rawKey)
	c, err := a.store.CallerByKeyHash(context.Background(), hash)
	if err != nil {
		secureCompare(hash, dummyHash) // equalize hit/miss timing
		return Caller{}, ErrInvalidKey
	}
	return c, nil
}

// Noop returns an Authenticator that never rejects a request and always yields
// the zero Caller — "auth disabled". It is the fail-open counterpart to
// NewAuthenticator.
func Noop() Authenticator {
	return noopAuthenticator{}
}

type noopAuthenticator struct{}

func (noopAuthenticator) Authenticate(rawKey string) (Caller, error) {
	return Caller{}, nil
}

// HashAPIKey returns the lowercase hex SHA-256 of raw, the same fingerprint
// convention as the loop detector. Raw keys are never stored — only this hash.
func HashAPIKey(raw string) string {
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:])
}

// secureCompare compares two strings in constant time, avoiding the early-exit
// timing oracle of a naive byte-by-byte ==.
func secureCompare(a, b string) bool {
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

// dummyHash is a zeroed hash used in the miss path of Authenticate so that a
// failed lookup takes roughly the same time as a successful one. Its exact
// value is irrelevant — it is never stored or compared against real data.
const dummyHash = "0000000000000000000000000000000000000000000000000000000000000000"

// ctxKey is the private context key for the authenticated Caller. A distinct
// unexported type avoids collisions with values set by other packages.
type ctxKey struct{}

// WithCaller returns a copy of ctx carrying the authenticated caller c.
func WithCaller(ctx context.Context, c Caller) context.Context {
	return context.WithValue(ctx, ctxKey{}, c)
}

// CallerFrom extracts the authenticated caller from ctx. The bool is false when
// no caller is present (auth disabled, or the request never passed through
// Middleware).
func CallerFrom(ctx context.Context) (Caller, bool) {
	c, ok := ctx.Value(ctxKey{}).(Caller)
	return c, ok
}

// Middleware authenticates each request via a, strips the X-FinOps-API-Key
// header, and injects the Caller into the request context before calling next.
// A nil authenticator is pass-through (fail-open). An authentication failure
// answers 401 unauthorized and the request never reaches next, so no body is
// read, hashed, or forwarded for an unauthenticated request.
func Middleware(a Authenticator, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw := r.Header.Get(APIKeyHeader)
		r.Header.Del(APIKeyHeader) // strip before forward — never reaches upstream
		if a == nil {
			next.ServeHTTP(w, r)
			return
		}
		c, err := a.Authenticate(raw)
		if err != nil {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			io.WriteString(w, `{"error":{"type":"unauthorized"}}`)
			return
		}
		next.ServeHTTP(w, r.WithContext(WithCaller(r.Context(), c)))
	})
}
