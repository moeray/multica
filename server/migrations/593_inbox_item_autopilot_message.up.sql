-- Autopilot message-mode reports can fall back to an inbox notification when
-- the recipient has no bound conversation. The chat references are optional:
-- a delivered chat report points at its session/message, while the inbox-only
-- fallback leaves them NULL. dedupe_key makes one autopilot run produce at most
-- one fallback notification.
ALTER TABLE inbox_item ADD COLUMN IF NOT EXISTS chat_session_id UUID;
ALTER TABLE inbox_item ADD COLUMN IF NOT EXISTS chat_message_id UUID;
ALTER TABLE inbox_item ADD COLUMN IF NOT EXISTS dedupe_key TEXT;
