-- Existing installations: remember that a 人工处理 order was paid while stock was short
-- (status 异常) and still owes its stock. Orders already in 异常 before this column
-- existed stay false, so they are never charged stock a second time.
-- Adding a column with a constant default is metadata-only on PostgreSQL 11+.
ALTER TABLE orders ADD COLUMN IF NOT EXISTS stock_owed boolean NOT NULL DEFAULT false;
