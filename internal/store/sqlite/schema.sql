-- PackCheck schema v1. Executed only from code via ExecContext;
-- no user input is ever interpolated into this text.
CREATE TABLE IF NOT EXISTS production_orders (
    id           TEXT PRIMARY KEY,
    sku          TEXT NOT NULL,
    product_name TEXT NOT NULL,
    gtin         TEXT NOT NULL,
    lot          TEXT NOT NULL,
    expiry       TEXT NOT NULL, -- YYYY-MM-DD
    supplier     TEXT NOT NULL DEFAULT '',
    font_profile TEXT NOT NULL DEFAULT 'font-a'
);

CREATE TABLE IF NOT EXISTS order_contents (
    order_id    TEXT NOT NULL REFERENCES production_orders(id) ON DELETE CASCADE,
    code        TEXT NOT NULL,
    description TEXT NOT NULL DEFAULT '',
    qty         INTEGER NOT NULL CHECK (qty > 0),
    PRIMARY KEY (order_id, code)
);

CREATE INDEX IF NOT EXISTS idx_order_contents_order ON order_contents(order_id);
CREATE INDEX IF NOT EXISTS idx_orders_gtin ON production_orders(gtin);
CREATE INDEX IF NOT EXISTS idx_orders_lot ON production_orders(lot);
