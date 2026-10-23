-- A Chat may be bound to a channel more than once over its lifetime. The old
-- UNIQUE (chat_session_id) constraint made the second binding impossible, even
-- though retired bindings are intentionally retained for route history.
ALTER TABLE channel_chat_session_binding
    DROP CONSTRAINT IF EXISTS channel_chat_session_binding_chat_session_id_key;

-- A rollback may have restored the old constraint as a standalone unique index.
DROP INDEX IF EXISTS channel_chat_session_binding_chat_session_id_key;
