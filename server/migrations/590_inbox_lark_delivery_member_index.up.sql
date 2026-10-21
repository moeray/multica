CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_inbox_lark_delivery_member
    ON inbox_lark_delivery(workspace_id, recipient_member_id, created_at DESC);
