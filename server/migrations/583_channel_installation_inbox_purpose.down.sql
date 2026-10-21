DELETE FROM channel_installation WHERE purpose = 'inbox';

ALTER TABLE channel_installation
    DROP CONSTRAINT IF EXISTS channel_installation_purpose_check;

ALTER TABLE channel_installation
    DROP COLUMN IF EXISTS purpose;

ALTER TABLE channel_installation
    ALTER COLUMN agent_id SET NOT NULL;
