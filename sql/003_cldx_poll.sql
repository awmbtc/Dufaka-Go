-- Existing installations: rotate the cldx poll set (see store.WaitingSNs).
--
-- Run this file OUTSIDE a transaction (e.g. psql -f without --single-transaction,
-- and not inside BEGIN/COMMIT): CREATE INDEX CONCURRENTLY refuses to run in a
-- transaction block. CONCURRENTLY builds the indexes without blocking checkout
-- and payment writes on a large orders table. If a concurrent build is
-- interrupted it leaves an INVALID index behind; drop that index and run the
-- file again (IF NOT EXISTS would otherwise keep the invalid one).
--
-- Dufaka-Go also creates these indexes at startup (store.EnsureCldxSchema)
-- when they are missing, but with a plain CREATE INDEX that blocks writes on
-- orders while it builds; on a big table run this file first.
ALTER TABLE orders ADD COLUMN IF NOT EXISTS cldx_minor bigint;
ALTER TABLE orders ADD COLUMN IF NOT EXISTS cldx_expires_at bigint;
ALTER TABLE orders ADD COLUMN IF NOT EXISTS cldx_polled_at bigint;
-- One partial index per WaitingSNs bucket: live (status 1) orders least
-- recently polled first, and late-window (status -1) orders by deadline.
CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_orders_cldx_live ON orders (cldx_polled_at NULLS FIRST, id) WHERE status = 1 AND cldx_minor IS NOT NULL AND deleted_at IS NULL;
CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_orders_cldx_late ON orders (cldx_expires_at) WHERE status = -1 AND cldx_minor IS NOT NULL AND deleted_at IS NULL;
-- The single index of the previous release is replaced by the two above.
DROP INDEX CONCURRENTLY IF EXISTS idx_orders_cldx_wait;
