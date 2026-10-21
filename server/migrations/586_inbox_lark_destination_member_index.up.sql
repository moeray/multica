CREATE UNIQUE INDEX CONCURRENTLY IF NOT EXISTS idx_inbox_lark_destination_member
    ON inbox_lark_destination(workspace_id, member_id);
