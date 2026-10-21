-- Dedicated inbox-notification bots are channel installations too, but they
-- are not owned by an agent. Keep the existing row shape and add an explicit
-- purpose discriminator instead of inventing a placeholder agent.
ALTER TABLE channel_installation
    ALTER COLUMN agent_id DROP NOT NULL;

ALTER TABLE channel_installation
    ADD COLUMN IF NOT EXISTS purpose TEXT NOT NULL DEFAULT 'agent';

DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1
        FROM pg_constraint
        WHERE conname = 'channel_installation_purpose_check'
    ) THEN
        ALTER TABLE channel_installation
            ADD CONSTRAINT channel_installation_purpose_check
            CHECK (purpose IN ('agent', 'inbox'));
    END IF;
END $$;
