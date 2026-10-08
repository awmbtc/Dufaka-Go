ALTER TABLE orders ADD COLUMN IF NOT EXISTS usdt_payment jsonb, ADD COLUMN IF NOT EXISTS usdt_polled_at bigint;

CREATE INDEX IF NOT EXISTS idx_orders_usdt_poll ON orders (usdt_polled_at NULLS FIRST, id) WHERE status IN (1,-1) AND usdt_payment IS NOT NULL AND deleted_at IS NULL;
