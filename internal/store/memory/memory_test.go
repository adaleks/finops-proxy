package memory

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/adaleks/finops-proxy/pkg/proxy"
)

func TestCallerByKeyHash(t *testing.T) {
	s := New()
	ctx := context.Background()

	hash := proxy.HashAPIKey("secret")
	s.SetCaller(hash, proxy.Caller{ID: "c1", Name: "Acme"})

	c, err := s.CallerByKeyHash(ctx, hash)
	if err != nil || c.ID != "c1" {
		t.Fatalf("CallerByKeyHash: got (%+v, %v), want (c1, nil)", c, err)
	}
	if _, err := s.CallerByKeyHash(ctx, proxy.HashAPIKey("nope")); !errors.Is(err, proxy.ErrInvalidKey) {
		t.Fatalf("unknown hash: got %v, want ErrInvalidKey", err)
	}
}

func TestLogAndSum(t *testing.T) {
	s := New()
	ctx := context.Background()
	now := time.Now().Unix()

	for _, r := range []proxy.RequestRecord{
		{KeyID: "c1", CostUSD: 100, IsLoopBlocked: false, Timestamp: now},
		{KeyID: "c1", CostUSD: 50, IsLoopBlocked: true, Timestamp: now},
		{KeyID: "c2", CostUSD: 200, IsLoopBlocked: false, Timestamp: now},
		{KeyID: "c1", CostUSD: 30, IsLoopBlocked: false, Timestamp: now - 100}, // outside the window
	} {
		if err := s.LogRequest(ctx, r); err != nil {
			t.Fatal(err)
		}
	}

	total, err := s.SumActualCostSince(ctx, now-1)
	if err != nil {
		t.Fatal(err)
	}
	if total != 300 { // 100 + 200 (blocked excluded; the -100 row is outside the window)
		t.Fatalf("SumActualCostSince = %d, want 300", total)
	}

	c1, err := s.SumActualCostForKeySince(ctx, "c1", now-1)
	if err != nil {
		t.Fatal(err)
	}
	if c1 != 100 {
		t.Fatalf("SumActualCostForKeySince(c1) = %d, want 100", c1)
	}
}

func TestSettings(t *testing.T) {
	s := New()
	ctx := context.Background()

	row, err := s.GetSettings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if row.LoopThresholdCount != DefaultLoopThreshold {
		t.Fatalf("default loop threshold = %d, want %d", row.LoopThresholdCount, DefaultLoopThreshold)
	}

	if err := s.UpdateSettings(ctx, proxy.SettingsRow{LoopThresholdCount: 7, CostAlertThresholdMicro: 5}); err != nil {
		t.Fatal(err)
	}
	row, err = s.GetSettings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if row.LoopThresholdCount != 7 || row.CostAlertThresholdMicro != 5 {
		t.Fatalf("updated settings = %+v, want {7 5}", row)
	}
}
