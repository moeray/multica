ALTER TABLE inbox_item DROP COLUMN IF EXISTS dedupe_key;
ALTER TABLE inbox_item DROP COLUMN IF EXISTS chat_message_id;
ALTER TABLE inbox_item DROP COLUMN IF EXISTS chat_session_id;
