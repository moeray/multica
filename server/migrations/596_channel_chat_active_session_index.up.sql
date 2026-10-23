-- Only active bindings must be unique per Chat. Historical bindings remain
-- readable and are distinguished by retired_at.
CREATE UNIQUE INDEX CONCURRENTLY IF NOT EXISTS idx_channel_chat_session_binding_active_session
    ON channel_chat_session_binding (chat_session_id)
    WHERE retired_at IS NULL;
