-- Existing installations: lock both the cldx amount and payment deadline per order.
ALTER TABLE orders ADD COLUMN IF NOT EXISTS cldx_minor bigint;
ALTER TABLE orders ADD COLUMN IF NOT EXISTS cldx_expires_at bigint;
