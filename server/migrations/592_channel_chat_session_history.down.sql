-- Restoring the pre-history schema is only possible when every Chat has at most
-- one binding. The pre-rollback hook refuses to reach this statement while
-- historical bindings exist, so a failed rollback cannot silently merge routes.
CREATE UNIQUE INDEX CONCURRENTLY IF NOT EXISTS channel_chat_session_binding_chat_session_id_key
    ON channel_chat_session_binding (chat_session_id);
