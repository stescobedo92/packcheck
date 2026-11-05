// Package sqlite adapts ports.OrderRepository to embedded SQLite.
//
// Security: every query is parameterized; no external value is ever
// interpolated into SQL. The DSN is built only from the given path and fixed
// known pragmas, never from free-form input fragments.
package sqlite

import (
	"context"
	"database/sql"
	_ "embed"
	"errors"
	"fmt"
	"net/url"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite" // pure-Go driver: no CGO, auditable and portable

	"packcheck/internal/domain"
)

//go:embed schema.sql
var schemaSQL string

// Store is the SQLite adapter. One *sql.DB pool handle per process.
type Store struct {
	db *sql.DB
}

// Open opens (or creates) the database at path. Use ":memory:" for ephemeral
// databases. It applies safe pragmas: foreign_keys, WAL on files, busy timeout.
func Open(path string) (*Store, error) {
	dsn, err := buildDSN(path)
	if err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("sqlite: open driver: %w", err)
	}
	// SQLite serializes writes; a single writer avoids "database is locked"
	// under concurrent read-only validation plus occasional seeding.
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	db.SetConnMaxLifetime(time.Hour)
	s := &Store{db: db}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := s.Migrate(ctx); err != nil {
		_ = db.Close()
		return nil, err
	}
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("sqlite: ping: %w", err)
	}
	return s, nil
}

func buildDSN(path string) (string, error) {
	if strings.TrimSpace(path) == "" {
		return "", errors.New("sqlite: database path is required")
	}
	if path == ":memory:" {
		return "file::memory:?cache=shared&_pragma=foreign_keys(1)&_pragma=busy_timeout(5000)", nil
	}
	// filepath.Clean neutralizes "." and redundancies; the path is still the
	// one the operator explicitly passed to the CLI (local trusted surface).
	clean := filepath.Clean(path)
	q := url.Values{}
	q.Add("_pragma", "foreign_keys(1)")
	q.Add("_pragma", "journal_mode(WAL)")
	q.Add("_pragma", "busy_timeout(5000)")
	q.Add("_pragma", "synchronous(NORMAL)")
	return "file:" + filepath.ToSlash(clean) + "?" + q.Encode(), nil
}

// Close releases the pool.
func (s *Store) Close() error { return s.db.Close() }

// DB exposes the handle only for in-module test diagnostics. Application
// code must use the Store methods.
func (s *Store) DB() *sql.DB { return s.db }

// Migrate applies the embedded schema. It is idempotent (IF NOT EXISTS).
func (s *Store) Migrate(ctx context.Context) error {
	if _, err := s.db.ExecContext(ctx, schemaSQL); err != nil {
		return fmt.Errorf("sqlite: migrate: %w", err)
	}
	return nil
}

// UpsertOrder inserts or replaces an order and its contents in one transaction.
func (s *Store) UpsertOrder(ctx context.Context, order domain.Order) error {
	if err := order.Validate(); err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("sqlite: begin upsert: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	const upsert = `
INSERT INTO production_orders (id, sku, product_name, gtin, lot, expiry, supplier, font_profile)
VALUES (?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(id) DO UPDATE SET
	sku=excluded.sku, product_name=excluded.product_name, gtin=excluded.gtin,
	lot=excluded.lot, expiry=excluded.expiry, supplier=excluded.supplier,
	font_profile=excluded.font_profile`
	if _, err := tx.ExecContext(ctx, upsert,
		order.ID, order.SKU, order.ProductName, domain.OnlyDigits(order.GTIN),
		domain.NormalizeCode(order.Lot), order.ExpiryDate(), order.Supplier, order.FontProfile,
	); err != nil {
		return fmt.Errorf("sqlite: upsert order: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM order_contents WHERE order_id = ?`, order.ID,
	); err != nil {
		return fmt.Errorf("sqlite: replace contents: %w", err)
	}
	stmt, err := tx.PrepareContext(ctx,
		`INSERT INTO order_contents (order_id, code, description, qty) VALUES (?, ?, ?, ?)`)
	if err != nil {
		return fmt.Errorf("sqlite: prepare contents: %w", err)
	}
	defer stmt.Close()
	for _, c := range order.Contents {
		if _, err := stmt.ExecContext(ctx, order.ID, domain.NormalizeCode(c.Code), c.Description, c.Qty); err != nil {
			return fmt.Errorf("sqlite: insert content: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("sqlite: commit upsert: %w", err)
	}
	return nil
}

// GetOrder retrieves an order with its contents ordered by code.
func (s *Store) GetOrder(ctx context.Context, id string) (domain.Order, error) {
	const q = `SELECT id, sku, product_name, gtin, lot, expiry, supplier, font_profile
	             FROM production_orders WHERE id = ?`
	var o domain.Order
	var expiry string
	err := s.db.QueryRowContext(ctx, q, id).Scan(
		&o.ID, &o.SKU, &o.ProductName, &o.GTIN, &o.Lot, &expiry, &o.Supplier, &o.FontProfile,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.Order{}, fmt.Errorf("%w: %s", domain.ErrOrderNotFound, id)
	}
	if err != nil {
		return domain.Order{}, fmt.Errorf("sqlite: get order: %w", err)
	}
	t, ok := domain.ParseExpiry(expiry)
	if !ok {
		return domain.Order{}, fmt.Errorf("sqlite: order %s has corrupt expiry %q", id, expiry)
	}
	o.Expiry = t
	contents, err := s.listContents(ctx, id)
	if err != nil {
		return domain.Order{}, err
	}
	o.Contents = contents
	return o, nil
}

func (s *Store) listContents(ctx context.Context, orderID string) ([]domain.ContentItem, error) {
	const q = `SELECT code, description, qty FROM order_contents WHERE order_id = ? ORDER BY code`
	rows, err := s.db.QueryContext(ctx, q, orderID)
	if err != nil {
		return nil, fmt.Errorf("sqlite: list contents: %w", err)
	}
	defer rows.Close()
	items := []domain.ContentItem{}
	for rows.Next() {
		var c domain.ContentItem
		if err := rows.Scan(&c.Code, &c.Description, &c.Qty); err != nil {
			return nil, fmt.Errorf("sqlite: scan content: %w", err)
		}
		items = append(items, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("sqlite: iterate contents: %w", err)
	}
	return items, nil
}

// ListOrders returns every order ordered by id, with its contents attached.
func (s *Store) ListOrders(ctx context.Context) ([]domain.Order, error) {
	const q = `SELECT id, sku, product_name, gtin, lot, expiry, supplier, font_profile
	             FROM production_orders ORDER BY id`
	rows, err := s.db.QueryContext(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("sqlite: list orders: %w", err)
	}
	defer rows.Close()
	orders := []domain.Order{}
	for rows.Next() {
		var o domain.Order
		var expiry string
		if err := rows.Scan(&o.ID, &o.SKU, &o.ProductName, &o.GTIN, &o.Lot, &expiry, &o.Supplier, &o.FontProfile); err != nil {
			return nil, fmt.Errorf("sqlite: scan order: %w", err)
		}
		t, ok := domain.ParseExpiry(expiry)
		if !ok {
			return nil, fmt.Errorf("sqlite: order %s has corrupt expiry %q", o.ID, expiry)
		}
		o.Expiry = t
		orders = append(orders, o)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("sqlite: iterate orders: %w", err)
	}
	// Attach contents outside the orders cursor (one connection at a time).
	for i := range orders {
		contents, err := s.listContents(ctx, orders[i].ID)
		if err != nil {
			return nil, err
		}
		orders[i].Contents = contents
	}
	return orders, nil
}
