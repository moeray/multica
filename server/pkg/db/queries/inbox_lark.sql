-- =====================
-- Explicit inbox notification destination
-- =====================

-- name: GetInboxLarkDestination :one
SELECT * FROM inbox_lark_destination
WHERE workspace_id = sqlc.arg('workspace_id')
  AND member_id = sqlc.arg('member_id');

-- name: UpsertInboxLarkDestination :one
-- Replacing a destination is atomic with cancelling the old target's queued
-- sends. An in-flight ('sending') row is left to its claim fence: it already
-- carries a frozen target and can never be redirected to the new destination.
WITH previous AS (
    SELECT destination.installation_id, destination.channel_user_id
    FROM inbox_lark_destination destination
    WHERE destination.workspace_id = sqlc.arg('workspace_id')
      AND destination.member_id = sqlc.arg('member_id')
), upserted AS (
    INSERT INTO inbox_lark_destination (
        workspace_id, member_id, installation_id, channel_user_id, status
    ) VALUES (
        sqlc.arg('workspace_id'), sqlc.arg('member_id'),
        sqlc.arg('installation_id'), sqlc.arg('channel_user_id'), 'active'
    )
    ON CONFLICT (workspace_id, member_id) DO UPDATE SET
        installation_id   = EXCLUDED.installation_id,
        channel_user_id   = EXCLUDED.channel_user_id,
        status            = 'active',
        last_test_status  = NULL,
        last_test_error   = NULL,
        last_test_at      = NULL,
        updated_at        = now()
    RETURNING *
), cancelled AS (
    UPDATE inbox_lark_delivery delivery
    SET status = 'cancelled',
        cancelled_reason = 'target_replaced',
        claim_token = NULL,
        claim_expires_at = NULL,
        updated_at = now()
    FROM previous
    WHERE delivery.workspace_id = sqlc.arg('workspace_id')
      AND delivery.recipient_member_id = sqlc.arg('member_id')
      AND delivery.status IN ('pending', 'retry')
      AND (
          previous.installation_id <> (SELECT installation_id FROM upserted)
          OR previous.channel_user_id <> (SELECT channel_user_id FROM upserted)
      )
)
SELECT * FROM upserted;

-- name: DisableInboxLarkDestination :one
WITH previous AS (
    SELECT destination.installation_id, destination.channel_user_id
    FROM inbox_lark_destination destination
    WHERE destination.workspace_id = sqlc.arg('workspace_id')
      AND destination.member_id = sqlc.arg('member_id')
), disabled AS (
    UPDATE inbox_lark_destination
    SET status = 'disabled', updated_at = now()
    WHERE workspace_id = sqlc.arg('workspace_id')
      AND member_id = sqlc.arg('member_id')
    RETURNING *
), cancelled AS (
    UPDATE inbox_lark_delivery delivery
    SET status = 'cancelled',
        cancelled_reason = 'destination_disabled',
        claim_token = NULL,
        claim_expires_at = NULL,
        updated_at = now()
    FROM previous
    WHERE delivery.workspace_id = sqlc.arg('workspace_id')
      AND delivery.recipient_member_id = sqlc.arg('member_id')
      AND delivery.status IN ('pending', 'retry')
)
SELECT * FROM disabled;

-- name: RecordInboxLarkDestinationTest :exec
UPDATE inbox_lark_destination
SET last_test_status = sqlc.arg('status'),
    last_test_error  = sqlc.arg('error'),
    last_test_at     = now(),
    updated_at       = now()
WHERE id = sqlc.arg('id');

-- name: FindChannelBindingForMemberOnInstallation :one
-- Explicit-target binding lookup: unlike FindChannelBindingForMember, this
-- never picks a different bot. A member can hold several identities on one
-- installation; the newest one wins, matching the inbound binding tiebreak.
SELECT binding.* FROM channel_user_binding binding
WHERE binding.workspace_id = sqlc.arg('workspace_id')
  AND binding.multica_user_id = sqlc.arg('multica_user_id')
  AND binding.installation_id = sqlc.arg('installation_id')
  AND binding.channel_type = 'feishu'
ORDER BY binding.bound_at DESC
LIMIT 1;

-- name: ListInboxLarkTargets :many
-- Only installations the caller can explicitly choose: an active agent bot
-- the caller has already bound, or the caller's own dedicated inbox bot.
SELECT ci.id AS installation_id,
       ci.purpose,
       ci.agent_id,
       ci.installer_user_id,
       binding.channel_user_id,
       (ci.config ->> 'bot_open_id')::text AS bot_open_id,
       (ci.config ->> 'region')::text AS region
FROM channel_installation ci
JOIN channel_user_binding binding
  ON binding.installation_id = ci.id
 AND binding.workspace_id = ci.workspace_id
 AND binding.channel_type = ci.channel_type
 AND binding.multica_user_id = sqlc.arg('member_id')
WHERE ci.workspace_id = sqlc.arg('workspace_id')
  AND ci.channel_type = 'feishu'
  AND ci.status = 'active'
  AND (
      ci.purpose = 'agent'
      OR (ci.purpose = 'inbox' AND ci.installer_user_id = sqlc.arg('member_id'))
  )
ORDER BY ci.purpose ASC, ci.created_at ASC;

-- =====================
-- Durable delivery outbox
-- =====================

-- name: CreateInboxLarkDelivery :one
WITH inserted AS (
    INSERT INTO inbox_lark_delivery (
        workspace_id, inbox_item_id, recipient_member_id,
        installation_id, channel_user_id, idempotency_key, payload
    ) VALUES (
        sqlc.arg('workspace_id'), sqlc.arg('inbox_item_id'),
        sqlc.arg('recipient_member_id'), sqlc.arg('installation_id'),
        sqlc.arg('channel_user_id'), sqlc.arg('idempotency_key'),
        sqlc.arg('payload')
    )
    ON CONFLICT (idempotency_key) DO NOTHING
    RETURNING *
)
SELECT * FROM inserted
UNION ALL
SELECT * FROM inbox_lark_delivery
WHERE idempotency_key = sqlc.arg('idempotency_key')
LIMIT 1;

-- name: ClaimInboxLarkDelivery :one
-- A queue wake claims a specific row. The status predicate makes a second
-- consumer (another replica, or the poller after a restart) a no-op.
UPDATE inbox_lark_delivery
SET status = 'sending',
    claim_token = sqlc.arg('claim_token'),
    claim_expires_at = now() + interval '2 minutes',
    attempts = attempts + 1,
    updated_at = now()
WHERE id = sqlc.arg('id')
  AND status IN ('pending', 'retry')
  AND next_attempt_at <= now()
RETURNING *;

-- name: ClaimNextInboxLarkDelivery :one
-- Multi-instance safe: SKIP LOCKED gives each worker a disjoint row.
WITH candidate AS (
    SELECT id FROM inbox_lark_delivery
    WHERE status IN ('pending', 'retry')
      AND next_attempt_at <= now()
    ORDER BY next_attempt_at ASC, created_at ASC
    FOR UPDATE SKIP LOCKED
    LIMIT 1
)
UPDATE inbox_lark_delivery delivery
SET status = 'sending',
    claim_token = sqlc.arg('claim_token'),
    claim_expires_at = now() + interval '2 minutes',
    attempts = delivery.attempts + 1,
    updated_at = now()
FROM candidate
WHERE delivery.id = candidate.id
RETURNING delivery.*;

-- name: MarkInboxLarkDeliveryDelivered :execrows
UPDATE inbox_lark_delivery
SET status = 'delivered',
    delivered_at = now(),
    claim_token = NULL,
    claim_expires_at = NULL,
    last_error = NULL,
    updated_at = now()
WHERE id = sqlc.arg('id')
  AND status = 'sending'
  AND claim_token = sqlc.arg('claim_token');

-- name: RetryInboxLarkDelivery :execrows
UPDATE inbox_lark_delivery
SET status = 'retry',
    next_attempt_at = sqlc.arg('next_attempt_at'),
    last_error = sqlc.arg('last_error'),
    claim_token = NULL,
    claim_expires_at = NULL,
    updated_at = now()
WHERE id = sqlc.arg('id')
  AND status = 'sending'
  AND claim_token = sqlc.arg('claim_token');

-- name: FailInboxLarkDelivery :execrows
UPDATE inbox_lark_delivery
SET status = 'failed',
    last_error = sqlc.arg('last_error'),
    claim_token = NULL,
    claim_expires_at = NULL,
    updated_at = now()
WHERE id = sqlc.arg('id')
  AND status = 'sending'
  AND claim_token = sqlc.arg('claim_token');

-- name: CancelInboxLarkDelivery :execrows
UPDATE inbox_lark_delivery delivery
SET status = 'cancelled',
    cancelled_reason = sqlc.arg('reason'),
    claim_token = NULL,
    claim_expires_at = NULL,
    updated_at = now()
WHERE id = sqlc.arg('id')
  AND status = 'sending'
  AND claim_token = sqlc.arg('claim_token');

-- name: RecoverStaleInboxLarkDeliveries :execrows
-- After a crash the claim lease expires. Re-deliver only while the frozen
-- target is still the active explicit destination; otherwise cancel it so an
-- unbound or replaced target never receives the old item.
UPDATE inbox_lark_delivery delivery
SET status = CASE WHEN EXISTS (
        SELECT 1 FROM inbox_lark_destination destination
        WHERE destination.workspace_id = delivery.workspace_id
          AND destination.member_id = delivery.recipient_member_id
          AND destination.status = 'active'
          AND destination.installation_id = delivery.installation_id
          AND destination.channel_user_id = delivery.channel_user_id
    ) THEN 'retry' ELSE 'cancelled' END,
    next_attempt_at = now(),
    last_error = CASE WHEN EXISTS (
        SELECT 1 FROM inbox_lark_destination destination
        WHERE destination.workspace_id = delivery.workspace_id
          AND destination.member_id = delivery.recipient_member_id
          AND destination.status = 'active'
          AND destination.installation_id = delivery.installation_id
          AND destination.channel_user_id = delivery.channel_user_id
    ) THEN delivery.last_error ELSE 'target unavailable after restart' END,
    cancelled_reason = CASE WHEN EXISTS (
        SELECT 1 FROM inbox_lark_destination destination
        WHERE destination.workspace_id = delivery.workspace_id
          AND destination.member_id = delivery.recipient_member_id
          AND destination.status = 'active'
          AND destination.installation_id = delivery.installation_id
          AND destination.channel_user_id = delivery.channel_user_id
    ) THEN NULL ELSE 'target_changed_or_disabled' END,
    claim_token = NULL,
    claim_expires_at = NULL,
    updated_at = now()
WHERE delivery.status = 'sending'
  AND delivery.claim_expires_at <= now();

-- name: CancelPendingInboxLarkDeliveriesForTarget :execrows
UPDATE inbox_lark_delivery
SET status = 'cancelled',
    cancelled_reason = sqlc.arg('reason'),
    claim_token = NULL,
    claim_expires_at = NULL,
    updated_at = now()
WHERE workspace_id = sqlc.arg('workspace_id')
  AND recipient_member_id = sqlc.arg('member_id')
  AND installation_id = sqlc.arg('installation_id')
  AND channel_user_id = sqlc.arg('channel_user_id')
  AND status IN ('pending', 'retry');

-- name: ListInboxLarkDeliveriesForMember :many
SELECT * FROM inbox_lark_delivery
WHERE workspace_id = sqlc.arg('workspace_id')
  AND recipient_member_id = sqlc.arg('member_id')
ORDER BY created_at DESC
LIMIT sqlc.arg('limit');

-- name: GetInboxLarkDeliveryForMember :one
SELECT * FROM inbox_lark_delivery
WHERE id = sqlc.arg('id')
  AND workspace_id = sqlc.arg('workspace_id')
  AND recipient_member_id = sqlc.arg('member_id');

-- name: RetryFailedInboxLarkDelivery :one
-- Manual retry is allowed only for a terminal failed row. The caller
-- re-validates the current destination before calling this so a retry can
-- never route an item to a different bot.
UPDATE inbox_lark_delivery
SET status = 'pending',
    attempts = 0,
    next_attempt_at = now(),
    last_error = NULL,
    cancelled_reason = NULL,
    claim_token = NULL,
    claim_expires_at = NULL,
    updated_at = now()
WHERE id = sqlc.arg('id')
  AND workspace_id = sqlc.arg('workspace_id')
  AND recipient_member_id = sqlc.arg('member_id')
  AND status = 'failed'
RETURNING *;

-- name: RevokeInboxLarkNotificationsForMember :exec
-- Membership is the access boundary. Removing a member disables their
-- destination and cancels queued sends in the same transaction as the
-- channel_user_binding cleanup, so a removed member cannot keep receiving
-- workspace content through a stale Feishu target.
WITH disabled AS (
    UPDATE inbox_lark_destination
    SET status = 'disabled', updated_at = now()
    WHERE workspace_id = sqlc.arg('workspace_id')
      AND member_id = sqlc.arg('member_id')
)
UPDATE inbox_lark_delivery delivery
SET status = 'cancelled',
    cancelled_reason = 'member_removed',
    claim_token = NULL,
    claim_expires_at = NULL,
    updated_at = now()
WHERE delivery.workspace_id = sqlc.arg('workspace_id')
  AND delivery.recipient_member_id = sqlc.arg('member_id')
  AND delivery.status IN ('pending', 'retry');

-- name: UpsertInboxChannelInstallation :one
-- Dedicated inbox bot: no agent row, no placeholder identity. One bot per
-- (workspace, installer, channel type); reinstalling the same bot reactivates
-- the existing row so its user binding and delivery history survive.
INSERT INTO channel_installation (
    workspace_id, agent_id, purpose, channel_type, config, installer_user_id
) VALUES (
    sqlc.arg('workspace_id'), NULL, 'inbox', sqlc.arg('channel_type'),
    sqlc.arg('config'), sqlc.arg('installer_user_id')
)
ON CONFLICT (workspace_id, installer_user_id, channel_type) WHERE purpose = 'inbox'
DO UPDATE SET
    config = EXCLUDED.config,
    status = 'active',
    installed_at = now(),
    updated_at = now()
RETURNING *;
