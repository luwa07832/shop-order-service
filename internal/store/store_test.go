package store

import (
	"errors"
	"path/filepath"
	"testing"
)

func TestOpenCreatesUsableStore(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "store.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer st.Close()
	if err := st.Ping(); err != nil {
		t.Fatalf("ping: %v", err)
	}
}

func sampleOrder() Order {
	return Order{
		ID:             "order-1",
		TenantID:       "tenant-a",
		IdempotencyKey: "key-1",
		Lines: []Line{
			{ProductID: "p-1", Quantity: 2, UnitPrice: 500},
			{ProductID: "p-2", Quantity: 1, UnitPrice: 300},
		},
		TotalAmount: 1300,
		Status:      "created",
		CreatedAt:   "2026-10-05T00:00:00Z",
	}
}

func TestCreateOrderPersistsAndReplays(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "store.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer st.Close()

	order := sampleOrder()
	stored, created, err := st.CreateOrder(order, "hash-a")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if !created || stored.ID != order.ID {
		t.Fatalf("created = %v, stored = %+v", created, stored)
	}

	replayed, created, err := st.CreateOrder(Order{
		ID:             "order-2",
		TenantID:       order.TenantID,
		IdempotencyKey: order.IdempotencyKey,
	}, "hash-a")
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	if created {
		t.Fatal("replay must not create a new order")
	}
	if replayed.ID != order.ID || replayed.TotalAmount != order.TotalAmount || len(replayed.Lines) != 2 {
		t.Fatalf("replayed = %+v", replayed)
	}
	if replayed.Lines[0].ProductID != "p-1" || replayed.Lines[1].ProductID != "p-2" {
		t.Fatalf("line order not preserved: %+v", replayed.Lines)
	}
}

func TestCreateOrderDetectsContentConflict(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "store.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer st.Close()

	order := sampleOrder()
	if _, _, err := st.CreateOrder(order, "hash-a"); err != nil {
		t.Fatalf("create: %v", err)
	}
	_, _, err = st.CreateOrder(Order{ID: "order-2", TenantID: order.TenantID, IdempotencyKey: order.IdempotencyKey}, "hash-b")
	if !errors.Is(err, ErrContentConflict) {
		t.Fatalf("err = %v, want ErrContentConflict", err)
	}

	// The original order must be untouched and visible to its tenant only.
	got, found, err := st.GetOrder(order.TenantID, order.ID)
	if err != nil || !found {
		t.Fatalf("get: found=%v err=%v", found, err)
	}
	if got.TotalAmount != 1300 {
		t.Fatalf("total = %d", got.TotalAmount)
	}
	if _, found, err := st.GetOrder("tenant-b", order.ID); err != nil || found {
		t.Fatalf("cross-tenant get: found=%v err=%v", found, err)
	}
	if _, found, err := st.GetOrder(order.TenantID, "missing"); err != nil || found {
		t.Fatalf("missing get: found=%v err=%v", found, err)
	}
}

func TestSameKeyAllowedAcrossTenants(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "store.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer st.Close()

	first := sampleOrder()
	second := sampleOrder()
	second.ID = "order-2"
	second.TenantID = "tenant-b"
	if _, _, err := st.CreateOrder(first, "hash-a"); err != nil {
		t.Fatalf("create first: %v", err)
	}
	if _, created, err := st.CreateOrder(second, "hash-b"); err != nil || !created {
		t.Fatalf("create second: created=%v err=%v", created, err)
	}
}

func TestOrdersSurviveReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "store.db")
	st, err := Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	order := sampleOrder()
	if _, _, err := st.CreateOrder(order, "hash-a"); err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := st.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	reopened, err := Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer reopened.Close()

	got, found, err := reopened.GetOrder(order.TenantID, order.ID)
	if err != nil || !found {
		t.Fatalf("get after reopen: found=%v err=%v", found, err)
	}
	if got.CreatedAt != order.CreatedAt || got.TotalAmount != order.TotalAmount || len(got.Lines) != 2 {
		t.Fatalf("got = %+v", got)
	}
	replayed, created, err := reopened.CreateOrder(Order{ID: "order-3", TenantID: order.TenantID, IdempotencyKey: order.IdempotencyKey}, "hash-a")
	if err != nil || created {
		t.Fatalf("replay after reopen: created=%v err=%v", created, err)
	}
	if replayed.ID != order.ID {
		t.Fatalf("replayed id = %s", replayed.ID)
	}
}
