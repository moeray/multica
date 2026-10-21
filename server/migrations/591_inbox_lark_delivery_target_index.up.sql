CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_inbox_lark_delivery_target
    ON inbox_lark_delivery(workspace_id, recipient_member_id, installation_id, channel_user_id)
    WHERE status IN ('pending', 'retry', 'sending');
