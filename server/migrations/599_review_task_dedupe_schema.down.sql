DROP TRIGGER IF EXISTS trg_issue_review_generation ON issue;
DROP FUNCTION IF EXISTS bump_issue_review_generation();
ALTER TABLE agent_task_queue DROP COLUMN IF EXISTS dedupe_key;
ALTER TABLE issue DROP COLUMN IF EXISTS review_generation;
