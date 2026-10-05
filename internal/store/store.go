// Package store owns the SQLite file and every write the service performs.
package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	_ "modernc.org/sqlite"
)

// ErrConflict reports that an idempotency key is already taken by an order
// whose content differs from the submitted one.
var ErrConflict = errors.New("idempotency conflict")

// ErrUnavailable reports that storage cannot currently serve the request.
var ErrUnavailable = errors.New("storage unavailable")

// Line is one snapshot order line, kept in submission order.
type Line struct {
	ProductID string `json:"product_id"`
	Quantity  int64  `json:"quantity"`
	UnitPrice int64  `json:"unit_price"`
}

// Order is the persisted order with its lines.
type Order struct {
	ID             string    `json:"id"`
	TenantID       string    `json:"tenant_id"`
	IdempotencyKey string    `json:"idempotency_key"`
	Status         string    `json:"status"`
	TotalAmount    int64     `json:"total_amount"`
	CreatedAt      time.Time `json:"created_at"`
	Lines          []Line    `json:"lines"`
}

// Store wraps the SQLite handle so callers never touch database/sql directly.
type Store struct {
	db *sql.DB
	// A single connection plus this mutex serializes create-order transactions,
	// which makes same-key concurrent submissions resolve to exactly one order.
	createMu chan struct{}
}

// Open prepares the database file and the schema this service needs.
func Open(path string) (*Store, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	// modernc.org/sqlite serializes writes per connection poorly under contention;
	// keep one connection and serialize critical sections ourselves.
	db.SetMaxOpenConns(1)
	for _, pragma := range []string{
		"PRAGMA journal_mode=WAL",
		"PRAGMA busy_timeout=5000",
		"PRAGMA foreign_keys=ON",
	} {
		if _, err := db.Exec(pragma); err != nil {
			db.Close()
			return nil, fmt.Errorf("apply %s: %w", pragma, err)
		}
	}
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("apply schema: %w", err)
	}
	return &Store{db: db, createMu: make(chan struct{}, 1)}, nil
}

// Ping reports whether the storage layer is usable.
func (s *Store) Ping() error { return s.db.Ping() }

// Close releases the database handle.
func (s *Store) Close() error { return s.db.Close() }

// CreateOrderParams carries a validated create request plus its canonical
// content fingerprint. The fingerprint identifies "same content" replays:
// JSON field order and whitespace are irrelevant, line order is significant.
type CreateOrderParams struct {
	TenantID       string
	IdempotencyKey string
	Fingerprint    string
	TotalAmount    int64
	Lines          []Line
}

// CreateOrder inserts an order, or replays the existing one for the tenant's
// idempotency key. Same key plus same fingerprint replays the original order
// (returning replay == true); same key with a different fingerprint returns
// ErrConflict and changes nothing. Concurrent identical submissions share one
// persisted order, id and timestamp.
func (s *Store) CreateOrder(ctx context.Context, p CreateOrderParams) (order *Order, replay bool, err error) {
	select {
	case s.createMu <- struct{}{}:
	case <-ctx.Done():
		return nil, false, fmt.Errorf("%w: %v", ErrUnavailable, ctx.Err())
	}
	defer func() { <-s.createMu }()

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, false, fmt.Errorf("%w: begin: %v", ErrUnavailable, err)
	}
	defer tx.Rollback()

	var existingID, existingFingerprint string
	lookupErr := tx.QueryRowContext(ctx,
		`SELECT order_id, fingerprint FROM idempotency_keys
		 WHERE tenant_id = ? AND idempotency_key = ?`,
		p.TenantID, p.IdempotencyKey).Scan(&existingID, &existingFingerprint)
	switch {
	case lookupErr == nil:
		existing, err := loadOrder(ctx, tx, existingID)
		if err != nil {
			return nil, false, err
		}
		if existingFingerprint != p.Fingerprint {
			return nil, false, ErrConflict
		}
		return existing, true, nil
	case !errors.Is(lookupErr, sql.ErrNoRows):
		return nil, false, fmt.Errorf("%w: lookup idempotency key: %v", ErrUnavailable, lookupErr)
	}

	id, err := newOrderID()
	if err != nil {
		return nil, false, fmt.Errorf("%w: generate id: %v", ErrUnavailable, err)
	}
	createdAt := time.Now().UTC()

	if _, err := tx.ExecContext(ctx,
		`INSERT INTO orders (id, tenant_id, idempotency_key, status, total_amount, created_at)
		 VALUES (?, ?, ?, 'created', ?, ?)`,
		id, p.TenantID, p.IdempotencyKey, p.TotalAmount, createdAt.Format(time.RFC3339Nano)); err != nil {
		return nil, false, fmt.Errorf("%w: insert order: %v", ErrUnavailable, err)
	}
	for i, line := range p.Lines {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO order_lines (order_id, position, product_id, quantity, unit_price)
			 VALUES (?, ?, ?, ?, ?)`,
			id, i, line.ProductID, line.Quantity, line.UnitPrice); err != nil {
			return nil, false, fmt.Errorf("%w: insert order line: %v", ErrUnavailable, err)
		}
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO idempotency_keys (tenant_id, idempotency_key, order_id, fingerprint)
		 VALUES (?, ?, ?, ?)`,
		p.TenantID, p.IdempotencyKey, id, p.Fingerprint); err != nil {
		return nil, false, fmt.Errorf("%w: insert idempotency key: %v", ErrUnavailable, err)
	}
	if err := tx.Commit(); err != nil {
		return nil, false, fmt.Errorf("%w: commit: %v", ErrUnavailable, err)
	}
	return &Order{
		ID:             id,
		TenantID:       p.TenantID,
		IdempotencyKey: p.IdempotencyKey,
		Status:         "created",
		TotalAmount:    p.TotalAmount,
		CreatedAt:      createdAt,
		Lines:          p.Lines,
	}, false, nil
}

// OrderByID returns the order for the tenant. An unknown id or an order owned
// by another tenant both return ErrNotFound so callers cannot probe tenancy.
func (s *Store) OrderByID(ctx context.Context, tenantID, id string) (*Order, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("%w: begin: %v", ErrUnavailable, err)
	}
	defer tx.Rollback()
	return loadOrderForTenant(ctx, tx, tenantID, id)
}

func loadOrderForTenant(ctx context.Context, q queryRower, tenantID, id string) (*Order, error) {
	var o Order
	var idemKey, createdAt string
	err := q.QueryRowContext(ctx,
		`SELECT id, tenant_id, idempotency_key, status, total_amount, created_at
		 FROM orders WHERE id = ? AND tenant_id = ?`,
		id, tenantID).Scan(&o.ID, &o.TenantID, &idemKey, &o.Status, &o.TotalAmount, &createdAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("%w: load order: %v", ErrUnavailable, err)
	}
	o.IdempotencyKey = idemKey
	if o.CreatedAt, err = time.Parse(time.RFC3339Nano, createdAt); err != nil {
		return nil, fmt.Errorf("%w: parse created_at: %v", ErrUnavailable, err)
	}
	lines, err := loadLines(ctx, q, o.ID)
	if err != nil {
		return nil, err
	}
	o.Lines = lines
	return &o, nil
}

func loadOrder(ctx context.Context, q queryRower, id string) (*Order, error) {
	var o Order
	var idemKey, createdAt string
	err := q.QueryRowContext(ctx,
		`SELECT id, tenant_id, idempotency_key, status, total_amount, created_at
		 FROM orders WHERE id = ?`, id).
		Scan(&o.ID, &o.TenantID, &idemKey, &o.Status, &o.TotalAmount, &createdAt)
	if errors.Is(err, sql.ErrNoRows) {
		// Referential integrity should make this unreachable.
		return nil, fmt.Errorf("%w: replayed order missing", ErrUnavailable)
	}
	if err != nil {
		return nil, fmt.Errorf("%w: load order: %v", ErrUnavailable, err)
	}
	o.IdempotencyKey = idemKey
	if o.CreatedAt, err = time.Parse(time.RFC3339Nano, createdAt); err != nil {
		return nil, fmt.Errorf("%w: parse created_at: %v", ErrUnavailable, err)
	}
	lines, err := loadLines(ctx, q, o.ID)
	if err != nil {
		return nil, err
	}
	o.Lines = lines
	return &o, nil
}

func loadLines(ctx context.Context, q queryRower, orderID string) ([]Line, error) {
	rows, err := q.QueryContext(ctx,
		`SELECT product_id, quantity, unit_price FROM order_lines
		 WHERE order_id = ? ORDER BY position`, orderID)
	if err != nil {
		return nil, fmt.Errorf("%w: load lines: %v", ErrUnavailable, err)
	}
	defer rows.Close()
	var lines []Line
	for rows.Next() {
		var l Line
		if err := rows.Scan(&l.ProductID, &l.Quantity, &l.UnitPrice); err != nil {
			return nil, fmt.Errorf("%w: scan line: %v", ErrUnavailable, err)
		}
		lines = append(lines, l)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("%w: iterate lines: %v", ErrUnavailable, err)
	}
	return lines, nil
}

type queryRower interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}

func newOrderID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

// ErrNotFound reports that no order visible to the tenant exists.
var ErrNotFound = errors.New("order not found")

const schema = `
CREATE TABLE IF NOT EXISTS service_metadata (
	key   TEXT PRIMARY KEY,
	value TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS orders (
	id              TEXT PRIMARY KEY,
	tenant_id       TEXT NOT NULL,
	idempotency_key TEXT NOT NULL,
	status          TEXT NOT NULL,
	total_amount    INTEGER NOT NULL,
	created_at      TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS order_lines (
	order_id   TEXT NOT NULL REFERENCES orders(id) ON DELETE CASCADE,
	position   INTEGER NOT NULL,
	product_id TEXT NOT NULL,
	quantity   INTEGER NOT NULL,
	unit_price INTEGER NOT NULL,
	PRIMARY KEY (order_id, position)
);

CREATE TABLE IF NOT EXISTS idempotency_keys (
	tenant_id       TEXT NOT NULL,
	idempotency_key TEXT NOT NULL,
	order_id        TEXT NOT NULL REFERENCES orders(id),
	fingerprint     TEXT NOT NULL,
	PRIMARY KEY (tenant_id, idempotency_key)
);
`
