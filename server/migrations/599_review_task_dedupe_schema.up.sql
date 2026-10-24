-- Review-round idempotency (MP-67).
--
-- Every transition into in_review starts a new review round. A review task is
-- keyed by (issue, reviewer agent, generation), so a mention and a wakeup that
-- describe the same handoff resolve to one active task. The trigger lives in
-- the database because status writes reach issue through several statements and
-- every one of them must advance the round identically.
ALTER TABLE issue ADD COLUMN IF NOT EXISTS review_generation BIGINT NOT NULL DEFAULT 0;

ALTER TABLE agent_task_queue ADD COLUMN IF NOT EXISTS dedupe_key TEXT;

CREATE OR REPLACE FUNCTION bump_issue_review_generation() RETURNS trigger AS $$
BEGIN
    IF NEW.status = 'in_review' AND NEW.status IS DISTINCT FROM OLD.status THEN
        NEW.review_generation := OLD.review_generation + 1;
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS trg_issue_review_generation ON issue;
CREATE TRIGGER trg_issue_review_generation
    BEFORE UPDATE OF status ON issue
    FOR EACH ROW
    EXECUTE FUNCTION bump_issue_review_generation();
