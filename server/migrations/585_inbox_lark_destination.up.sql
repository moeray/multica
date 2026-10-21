-- One explicit Feishu notification destination per (workspace, member).
-- installation_id and channel_user_id are frozen here so a later agent-bot
-- rebind outside this row cannot silently change where inbox notifications go.
CREATE TABLE IF NOT EXISTS inbox_lark_destination (
    id                  UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    workspace_id        UUID NOT NULL,
    member_id           UUID NOT NULL,
    installation_id     UUID NOT NULL,
    channel_user_id     TEXT NOT NULL,
    status              TEXT NOT NULL DEFAULT 'active'
        CHECK (status IN ('active', 'disabled')),
    last_test_status    TEXT
        CHECK (last_test_status IN ('success', 'failed')),
    last_test_error     TEXT,
    last_test_at        TIMESTAMPTZ,
    created_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at          TIMESTAMPTZ NOT NULL DEFAULT now()
);
