ALTER TABLE chat_message DROP COLUMN IF EXISTS dedupe_key;
ALTER TABLE chat_message DROP COLUMN IF EXISTS review_passed;
ALTER TABLE chat_message DROP COLUMN IF EXISTS actor_agent_id;
