-- Server-authored agent messages (for example a review result) need to record
-- who authored the message independently of the chat session's agent, expose a
-- structured pass/fail result, and carry an idempotency key so a retried
-- producer cannot write a second row.
ALTER TABLE chat_message ADD COLUMN IF NOT EXISTS actor_agent_id UUID;
ALTER TABLE chat_message ADD COLUMN IF NOT EXISTS review_passed BOOLEAN;
ALTER TABLE chat_message ADD COLUMN IF NOT EXISTS dedupe_key TEXT;
