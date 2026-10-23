CREATE UNIQUE INDEX CONCURRENTLY IF NOT EXISTS idx_chat_message_dedupe_key
    ON chat_message (chat_session_id, dedupe_key)
    WHERE dedupe_key IS NOT NULL;
