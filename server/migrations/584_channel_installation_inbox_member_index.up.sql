CREATE UNIQUE INDEX CONCURRENTLY IF NOT EXISTS idx_channel_installation_inbox_member
    ON channel_installation(workspace_id, installer_user_id, channel_type)
    WHERE purpose = 'inbox';
