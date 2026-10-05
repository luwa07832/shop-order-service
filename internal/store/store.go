// Package store owns the SQLite file and every write the service performs.
package store

import (
	"database/sql"
	"errors"
	"fmt"

	_ "modernc.org/sqlite"
)

// ErrContentConflict reports that an idempotency key was reused with different order content.
var ErrContentConflict = errors.New("store: idempotency key reused with different content")

// Store wraps the SQLite handle so callers never touch database/sql directly.
type Store struct {
	db *sql.DB
}

// Line is one ordered product row of an order. Amounts are cents.
type Line struct {
	ProductID string `json:"product_id"`
	Quantity  int64  `json:"quantity"`
	UnitPrice int64  `json:"unit_price"`
}

// Order is the stored snapshot of a created order.
type Order struct {
	ID             string `json:"id"`
	TenantID       string `json:"tenant_id"`
	IdempotencyKey string `json:"idempotency_key"`
	Lines          []Line `json:"lines"`
	TotalAmount    int64  `json:"total_amount"`
	Status         string `json:"status"`
	CreatedAt      string `json:"created_at"`
}

// Open prepares the database file and the schema this service needs.
func Open(path string) (*Store, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	// A single connection serializes transactions, which keeps the
	// read-then-insert idempotency check in CreateOrder race-free.
	db.SetMaxOpenConns(1)
	if _, err := db.Exec("PRAGMA journal_mode=WAL"); err != nil {
		db.Close()
		return nil, fmt.Errorf("enable wal: %w", err)
	}
	if _, err := db.Exec("PRAGMA busy_timeout=5000"); err != nil {
		db.Close()
		return nil, fmt.Errorf("set busy timeout: %w", err)
	}
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("apply schema: %w", err)
	}
	return &Store{db: db}, nil
}

// Ping reports whether the storage layer is usable.
func (s *Store) Ping() error { return s.db.Ping() }

// Close releases the database handle.
func (s *Store) Close() error { return s.db.Close() }

// CreateOrder persists the order and reports (order, true, nil). When the
// tenant already used the idempotency key it replays the stored order with
// (order, false, nil) if the content hash matches, or ErrContentConflict if
// the key was reused with different content. A failed create persists nothing.
func (s *Store) CreateOrder(order Order, contentHash string) (Order, bool, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return Order{}, false, fmt.Errorf("begin create order: %w", err)
	}
	defer tx.Rollback()

	var existingID, existingHash string
	err = tx.QueryRow(
		`SELECT id, content_hash FROM orders WHERE tenant_id = ? AND idempotency_key = ?`,
		order.TenantID, order.IdempotencyKey,
	).Scan(&existingID, &existingHash)
	switch {
	case err == nil:
		if existingHash != contentHash {
			return Order{}, false, ErrContentConflict
		}
		existing, err := loadOrder(tx, existingID)
		if err != nil {
			return Order{}, false, err
		}
		return existing, false, nil
	case !errors.Is(err, sql.ErrNoRows):
		return Order{}, false, fmt.Errorf("lookup idempotency key: %w", err)
	}

	if _, err := tx.Exec(
		`INSERT INTO orders (id, tenant_id, idempotency_key, content_hash, total_amount, status, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		order.ID, order.TenantID, order.IdempotencyKey, contentHash,
		order.TotalAmount, order.Status, order.CreatedAt,
	); err != nil {
		return Order{}, false, fmt.Errorf("insert order: %w", err)
	}
	for position, line := range order.Lines {
		if _, err := tx.Exec(
			`INSERT INTO order_lines (order_id, position, product_id, quantity, unit_price)
			 VALUES (?, ?, ?, ?, ?)`,
			order.ID, position, line.ProductID, line.Quantity, line.UnitPrice,
		); err != nil {
			return Order{}, false, fmt.Errorf("insert order line: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return Order{}, false, fmt.Errorf("commit order: %w", err)
	}
	return order, true, nil
}

// GetOrder returns the order owned by the tenant, or found=false when no such
// order exists for that tenant.
func (s *Store) GetOrder(tenantID, id string) (Order, bool, error) {
	order, err := loadOrderWhere(s.db, `WHERE tenant_id = ? AND id = ?`, tenantID, id)
	if errors.Is(err, sql.ErrNoRows) {
		return Order{}, false, nil
	}
	if err != nil {
		return Order{}, false, err
	}
	return order, true, nil
}

// querier is satisfied by both *sql.DB and *sql.Tx.
type querier interface {
	Query(query string, args ...any) (*sql.Rows, error)
	QueryRow(query string, args ...any) *sql.Row
}

// loadOrder fetches one order with its lines by primary key.
func loadOrder(q querier, id string) (Order, error) {
	return loadOrderWhere(q, `WHERE id = ?`, id)
}

// loadOrderWhere fetches one order with its lines, lines in submission order.
func loadOrderWhere(q querier, where string, args ...any) (Order, error) {
	var order Order
	err := q.QueryRow(
		`SELECT id, tenant_id, idempotency_key, total_amount, status, created_at FROM orders `+where,
		args...,
	).Scan(&order.ID, &order.TenantID, &order.IdempotencyKey, &order.TotalAmount, &order.Status, &order.CreatedAt)
	if err != nil {
		return Order{}, err
	}

	rows, err := q.Query(
		`SELECT product_id, quantity, unit_price FROM order_lines WHERE order_id = ? ORDER BY position`,
		order.ID,
	)
	if err != nil {
		return Order{}, fmt.Errorf("query order lines: %w", err)
	}
	defer rows.Close()

	order.Lines = []Line{}
	for rows.Next() {
		var line Line
		if err := rows.Scan(&line.ProductID, &line.Quantity, &line.UnitPrice); err != nil {
			return Order{}, fmt.Errorf("scan order line: %w", err)
		}
		order.Lines = append(order.Lines, line)
	}
	if err := rows.Err(); err != nil {
		return Order{}, fmt.Errorf("read order lines: %w", err)
	}
	return order, nil
}

const schema = `
CREATE TABLE IF NOT EXISTS service_metadata (
	key   TEXT PRIMARY KEY,
	value TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS orders (
	id              TEXT PRIMARY KEY,
	tenant_id       TEXT NOT NULL,
	idempotency_key TEXT NOT NULL,
	content_hash    TEXT NOT NULL,
	total_amount    INTEGER NOT NULL,
	status          TEXT NOT NULL,
	created_at      TEXT NOT NULL,
	UNIQUE (tenant_id, idempotency_key)
);

CREATE TABLE IF NOT EXISTS order_lines (
	order_id  TEXT NOT NULL REFERENCES orders (id),
	position  INTEGER NOT NULL,
	product_id TEXT NOT NULL,
	quantity  INTEGER NOT NULL,
	unit_price INTEGER NOT NULL,
	PRIMARY KEY (order_id, position)
);
`
