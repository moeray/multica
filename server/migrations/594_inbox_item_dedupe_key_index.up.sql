CREATE UNIQUE INDEX CONCURRENTLY IF NOT EXISTS idx_inbox_item_dedupe_key
    ON inbox_item (workspace_id, dedupe_key)
    WHERE dedupe_key IS NOT NULL;
