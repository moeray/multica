-- Serves both readers of context_task_id: the claim-side read of one turn's
-- injected rows (chat_session_id + context_task_id) and the turn-creation
-- UPDATE's scan of a session's rows. Kept whole rather than partial so both
-- predicates — "= @task_id" and "IS NULL" — are covered by one index.
--
-- Separate file from 599 because CREATE INDEX CONCURRENTLY cannot run inside a
-- transaction block, and each migration file is applied as one implicit
-- transaction.
CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_chat_message_context_task
    ON chat_message (chat_session_id, context_task_id);
