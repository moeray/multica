CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_inbox_lark_delivery_due
    ON inbox_lark_delivery(next_attempt_at, created_at)
    WHERE status IN ('pending', 'retry');
