-- A deferred auto-retry now counts as live review work, so the v2 unique index
-- will include it. Environments that ran the previous release can already hold
-- a deferred retry beside the queued task RequestReview created because it did
-- not see deferred rows. Retire the losers before the index is built.
--
-- A pre-599 runtime_offline retry has no key yet, and 600 skipped deferred
-- rows. Backfill the same review shape 600 used so the lookup and cleanup
-- below can see it.
UPDATE agent_task_queue t
SET dedupe_key = 'review:' || t.issue_id::text || ':' || t.agent_id::text || ':' || i.review_generation::text
FROM issue i
WHERE t.issue_id = i.id
  AND t.status = 'deferred'
  AND t.dedupe_key IS NULL
  AND i.status = 'in_review'
  AND t.workflow_step = 'in_review'
  AND t.agent_id IS NOT NULL
  AND t.agent_id IS DISTINCT FROM i.assignee_id
  AND (
      t.trigger_evidence_kind = 'issue_wakeup'
      OR (t.trigger_comment_id IS NOT NULL AND t.comment_thread_id IS NOT NULL)
  );

-- A started task is never cancelled: clear its key and mark it superseded so
-- the review-result guard refuses its verdict. Unstarted duplicates are
-- cancelled and keep their terminal key for audit.
WITH ranked AS (
    SELECT
        id,
        status,
        ROW_NUMBER() OVER (
            PARTITION BY dedupe_key
            ORDER BY
                CASE status
                    WHEN 'running' THEN 0
                    WHEN 'waiting_local_directory' THEN 1
                    WHEN 'dispatched' THEN 2
                    WHEN 'queued' THEN 3
                    ELSE 4
                END,
                created_at DESC,
                id DESC
        ) AS round_rank
    FROM agent_task_queue
    WHERE dedupe_key IS NOT NULL
      AND status IN ('queued', 'deferred', 'dispatched', 'running', 'waiting_local_directory')
), losers AS (
    SELECT id, status FROM ranked WHERE round_rank > 1
)
UPDATE agent_task_queue t
SET status = CASE
        WHEN l.status IN ('queued', 'deferred') THEN 'cancelled'
        ELSE t.status
    END,
    completed_at = CASE
        WHEN l.status IN ('queued', 'deferred') THEN now()
        ELSE t.completed_at
    END,
    failure_reason = CASE
        WHEN l.status IN ('queued', 'deferred') THEN 'review_superseded'
        ELSE t.failure_reason
    END,
    dedupe_key = CASE
        WHEN l.status IN ('queued', 'deferred') THEN t.dedupe_key
        ELSE NULL
    END,
    context = CASE
        WHEN l.status IN ('dispatched', 'running', 'waiting_local_directory')
        THEN COALESCE(t.context, '{}'::jsonb) || jsonb_build_object('review_superseded', TRUE)
        ELSE t.context
    END
FROM losers l
WHERE t.id = l.id;
