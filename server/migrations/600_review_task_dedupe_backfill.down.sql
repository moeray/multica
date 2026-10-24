-- The backfill is a data repair. Dropping the keys is enough to undo it; the
-- rows it cancelled stay cancelled because they might already have been read.
UPDATE agent_task_queue SET dedupe_key = NULL WHERE dedupe_key LIKE 'review:%';
