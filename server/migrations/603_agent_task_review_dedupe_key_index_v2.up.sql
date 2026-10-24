CREATE UNIQUE INDEX CONCURRENTLY IF NOT EXISTS idx_agent_task_queue_review_dedupe_key_v2
    ON agent_task_queue (dedupe_key)
    WHERE dedupe_key IS NOT NULL
      AND status IN ('queued', 'deferred', 'dispatched', 'running', 'waiting_local_directory');
