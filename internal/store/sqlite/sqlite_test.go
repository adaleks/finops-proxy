package sqlite

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/adaleks/finops-proxy/pkg/proxy"
)

func TestStore(t *testing.T) {
	dir := t.TempDir()
	db, err := Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()

	ctx := context.Background()
	now := time.Now().Unix()

	if err := db.LogRequest(ctx, proxy.RequestRecord{
		KeyID: "c1", CostUSD: 100, IsLoopBlocked: false, Timestamp: now,
	}); err != nil {
		t.Fatal(err)
	}
	if err := db.LogRequest(ctx, proxy.RequestRecord{
		KeyID: "c1", CostUSD: 50, IsLoopBlocked: true, Timestamp: now,
	}); err != nil {
		t.Fatal(err)
	}

	total, err := db.SumActualCostSince(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	if total != 100 {
		t.Fatalf("SumActualCostSince = %d, want 100", total)
	}
	c1, err := db.SumActualCostForKeySince(ctx, "c1", 0)
	if err != nil {
		t.Fatal(err)
	}
	if c1 != 100 {
		t.Fatalf("SumActualCostForKeySince(c1) = %d, want 100", c1)
	}

	// Settings seed-on-first-access, then update.
	row, err := db.GetSettings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if row.LoopThresholdCount != DefaultLoopThreshold {
		t.Fatalf("default loop threshold = %d, want %d", row.LoopThresholdCount, DefaultLoopThreshold)
	}
	if err := db.UpdateSettings(ctx, proxy.SettingsRow{LoopThresholdCount: 9, CostAlertThresholdMicro: 3}); err != nil {
		t.Fatal(err)
	}
	row, err = db.GetSettings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if row.LoopThresholdCount != 9 || row.CostAlertThresholdMicro != 3 {
		t.Fatalf("updated settings = %+v, want {9 3}", row)
	}

	// Model price seed (INSERT OR IGNORE) + list.
	if err := db.SeedModelPrices(ctx, []PriceRow{
		{Model: "gpt-4o", InputMicroUSD: 100, OutputMicroUSD: 200, Source: PriceSourceSeed},
	}); err != nil {
		t.Fatal(err)
	}
	rows, err := db.ListModelPrices(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Model != "gpt-4o" || rows[0].InputMicroUSD != 100 {
		t.Fatalf("ListModelPrices = %+v, want one gpt-4o row", rows)
	}
}
