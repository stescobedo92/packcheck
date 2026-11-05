package sqlite

import (
	"context"
	"errors"
	"testing"
	"time"

	"packcheck/internal/domain"
)

func testOrder() domain.Order {
	return domain.Order{
		ID: "OP-1", SKU: "S1", ProductName: "PRODUCTO DE PRUEBA",
		GTIN: domain.WithCheckDigit("750123450001"), Lot: "L0001A",
		Expiry: time.Date(2027, 3, 15, 0, 0, 0, 0, time.UTC),
		Contents: []domain.ContentItem{
			{Code: "B-2", Description: "bee", Qty: 2},
			{Code: "A-1", Description: "ay", Qty: 1},
		},
		Supplier: "SUP", FontProfile: "font-a",
	}
}

func openMem(t *testing.T) *Store {
	t.Helper()
	s, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func TestUpsertGetRoundTrip(t *testing.T) {
	s := openMem(t)
	ctx := context.Background()
	want := testOrder()
	if err := s.UpsertOrder(ctx, want); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetOrder(ctx, want.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.GTIN != want.GTIN || got.Lot != want.Lot || got.ExpiryDate() != "2027-03-15" {
		t.Fatalf("round-trip mismatch: %+v", got)
	}
	if len(got.Contents) != 2 || got.Contents[0].Code != "A-1" {
		t.Fatalf("contents must come ordered by code: %+v", got.Contents)
	}
	// Upsert again with different contents: replaces, never accumulates.
	want.Contents = []domain.ContentItem{{Code: "A-1", Qty: 5}}
	if err := s.UpsertOrder(ctx, want); err != nil {
		t.Fatal(err)
	}
	got, err = s.GetOrder(ctx, want.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Contents) != 1 || got.Contents[0].Qty != 5 {
		t.Fatalf("upsert did not replace contents: %+v", got.Contents)
	}
}

func TestGetOrderNotFoundWrapsSentinel(t *testing.T) {
	s := openMem(t)
	_, err := s.GetOrder(context.Background(), "OP-NOPE")
	if !errors.Is(err, domain.ErrOrderNotFound) {
		t.Fatalf("err=%v want ErrOrderNotFound", err)
	}
}

func TestGetOrderSQLInjectionProbeFailsClosed(t *testing.T) {
	s := openMem(t)
	ctx := context.Background()
	if err := s.UpsertOrder(ctx, testOrder()); err != nil {
		t.Fatal(err)
	}
	// With parameterized queries this is a literal unknown id, not SQL.
	_, err := s.GetOrder(ctx, `OP-1" OR "1"="1`)
	if !errors.Is(err, domain.ErrOrderNotFound) {
		t.Fatalf("injection probe err=%v; want not-found (parameterized)", err)
	}
	orders, err := s.ListOrders(ctx)
	if err != nil || len(orders) != 1 {
		t.Fatalf("table must remain intact: orders=%v err=%v", orders, err)
	}
}

func TestUpsertRejectsInvalidOrder(t *testing.T) {
	s := openMem(t)
	bad := testOrder()
	bad.GTIN = "123"
	if err := s.UpsertOrder(context.Background(), bad); err == nil {
		t.Fatal("invalid GTIN order must not be persisted")
	}
	_, err := s.GetOrder(context.Background(), bad.ID)
	if !errors.Is(err, domain.ErrOrderNotFound) {
		t.Fatalf("invalid order leaked into DB: %v", err)
	}
}
