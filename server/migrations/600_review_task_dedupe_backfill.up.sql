-- Backfill the review key onto tasks that are already active for a review
-- round, and retire the duplicate rows the old trigger paths could create
-- (MP-66: one wakeup run plus one mention run for the same handoff).
--
-- The predicate is deliberately conservative: only issues that are in_review
-- right now, tasks queued while that status was current, and tasks that carry
-- one of the review trigger shapes (an issue wakeup or a comment trigger).
-- A running task wins the round so an already-executing review is never
-- silently replaced by a queued sibling.
WITH active AS (
    SELECT
        t.id,
        t.status,
        t.created_at,
        'review:' || t.issue_id::text || ':' || t.agent_id::text || ':' || i.review_generation::text AS review_key,
        ROW_NUMBER() OVER (
            PARTITION BY t.issue_id, t.agent_id
            ORDER BY
                CASE WHEN t.status IN ('dispatched', 'running', 'waiting_local_directory') THEN 0 ELSE 1 END,
                t.created_at DESC,
                t.id DESC
        ) AS round_rank
    FROM agent_task_queue t
    JOIN issue i ON i.id = t.issue_id
    WHERE t.status IN ('queued', 'dispatched', 'running', 'waiting_local_directory')
      AND t.dedupe_key IS NULL
      AND i.status = 'in_review'
      AND t.workflow_step = 'in_review'
      AND t.agent_id IS DISTINCT FROM i.assignee_id
      AND (
          t.trigger_evidence_kind = 'issue_wakeup'
          OR (t.trigger_comment_id IS NOT NULL AND t.comment_thread_id IS NOT NULL)
      )
)
UPDATE agent_task_queue t
SET dedupe_key = CASE WHEN a.round_rank = 1 THEN a.review_key ELSE NULL END,
    status = CASE
        WHEN a.round_rank > 1 AND t.status = 'queued' THEN 'cancelled'
        ELSE t.status
    END,
    completed_at = CASE
        WHEN a.round_rank > 1 AND t.status = 'queued' THEN now()
        ELSE t.completed_at
    END,
    failure_reason = CASE
        WHEN a.round_rank > 1 AND t.status = 'queued' THEN 'review_superseded'
        ELSE t.failure_reason
    END,
    context = CASE
        WHEN a.round_rank > 1 AND t.status IN ('dispatched', 'running', 'waiting_local_directory')
        THEN COALESCE(t.context, '{}'::jsonb) || jsonb_build_object('review_superseded', TRUE)
        ELSE t.context
    END
FROM active a
WHERE t.id = a.id;
