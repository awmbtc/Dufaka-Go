-- Existing installations: the unpaid-order quota (store.unpaidQuota). Checkout
-- counts the unexpired unpaid orders of the buyer's address block and email, so
-- one source cannot hold all stock without paying.
--
-- Run this file OUTSIDE a transaction (CREATE INDEX CONCURRENTLY refuses to run
-- in one). Dufaka-Go also adds the column and index at startup
-- (store.EnsureCldxSchema) when they are missing, but with a plain CREATE INDEX
-- that blocks writes on orders while it builds; on a big table run this first.
-- Adding a nullable column is metadata-only. Orders placed before the upgrade
-- have no buy_source and count only by email until they expire.
ALTER TABLE orders ADD COLUMN IF NOT EXISTS buy_source varchar(64);
CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_orders_unpaid ON orders (created_at) WHERE status = 1 AND deleted_at IS NULL;
