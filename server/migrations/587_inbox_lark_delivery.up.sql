-- Durable outbox for inbox notifications. payload is the inbox item snapshot
-- taken at enqueue time; delivery never re-reads a mutable agent binding.
-- idempotency_key is (inbox item, recipient, installation, channel user), so a
-- duplicate bus event or replay cannot enqueue a second send.
CREATE TABLE IF NOT EXISTS inbox_lark_delivery (
    id                  UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    workspace_id        UUID NOT NULL,
    inbox_item_id       UUID NOT NULL,
    recipient_member_id UUID NOT NULL,
    installation_id     UUID NOT NULL,
    channel_user_id     TEXT NOT NULL,
    idempotency_key     TEXT NOT NULL,
    payload             JSONB NOT NULL DEFAULT '{}'::jsonb,
    status              TEXT NOT NULL DEFAULT 'pending'
        CHECK (status IN ('pending', 'sending', 'retry', 'delivered', 'failed', 'cancelled')),
    attempts            INTEGER NOT NULL DEFAULT 0,
    next_attempt_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    claim_token         UUID,
    claim_expires_at    TIMESTAMPTZ,
    last_error          TEXT,
    cancelled_reason    TEXT,
    delivered_at        TIMESTAMPTZ,
    created_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at          TIMESTAMPTZ NOT NULL DEFAULT now()
);
