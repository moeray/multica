CREATE UNIQUE INDEX CONCURRENTLY IF NOT EXISTS idx_agent_task_queue_review_dedupe_key
    ON agent_task_queue (dedupe_key)
    WHERE dedupe_key IS NOT NULL
      AND status IN ('queued', 'dispatched', 'running', 'waiting_local_directory');
