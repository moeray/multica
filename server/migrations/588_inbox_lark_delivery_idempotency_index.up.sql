CREATE UNIQUE INDEX CONCURRENTLY IF NOT EXISTS idx_inbox_lark_delivery_idempotency
    ON inbox_lark_delivery(idempotency_key);
