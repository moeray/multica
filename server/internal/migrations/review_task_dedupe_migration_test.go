package migrations

import (
	"context"
	"os"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// TestReviewTaskDedupeMigration covers MP-67's schema, backfill, and partial
// unique index on an isolated schema: duplicate active review rows from the old
// trigger paths are retired, the key is released when the task is terminal, and
// a new review round can take it again.
func TestReviewTaskDedupeMigration(t *testing.T) {
	dbURL := os.Getenv("DATABASE_URL")
	if dbURL == "" {
		t.Skip("integration test requires Postgres at DATABASE_URL")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dbURL)
	if err != nil {
		t.Fatalf("connect to Postgres: %v", err)
	}
	defer pool.Close()
	conn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire connection: %v", err)
	}
	defer conn.Release()

	const schema = "review_task_dedupe_migration_test"
	cleanup := func() {
		_, _ = pool.Exec(context.Background(), "DROP SCHEMA IF EXISTS "+schema+" CASCADE")
	}
	cleanup()
	t.Cleanup(cleanup)
	if _, err := conn.Exec(ctx, "CREATE SCHEMA "+schema); err != nil {
		t.Fatalf("create isolated schema: %v", err)
	}
	if _, err := conn.Exec(ctx, `SELECT set_config('search_path', $1, false)`, schema); err != nil {
		t.Fatalf("set search_path: %v", err)
	}
	if _, err := conn.Exec(ctx, `
		CREATE TABLE issue (
			id UUID PRIMARY KEY,
			status TEXT NOT NULL,
			assignee_id UUID,
			review_generation BIGINT NOT NULL DEFAULT 0
		);
		CREATE TABLE agent_task_queue (
			id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
			agent_id UUID NOT NULL,
			issue_id UUID NOT NULL,
			status TEXT NOT NULL,
			created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
			completed_at TIMESTAMPTZ,
			fire_at TIMESTAMPTZ,
			failure_reason TEXT,
			context JSONB,
			dedupe_key TEXT,
			workflow_step TEXT,
			trigger_evidence_kind TEXT,
			trigger_comment_id UUID,
			comment_thread_id UUID
		);
	`); err != nil {
		t.Fatalf("create pre-migration tables: %v", err)
	}

	applyMigrationFile(t, ctx, conn.Conn(), "599_review_task_dedupe_schema.up.sql")
	const issueID = "11111111-1111-1111-1111-111111111111"
	const agentID = "22222222-2222-2222-2222-222222222222"
	if _, err := conn.Exec(ctx, `INSERT INTO issue (id, status) VALUES ($1, 'todo')`, issueID); err != nil {
		t.Fatalf("insert issue: %v", err)
	}
	if _, err := conn.Exec(ctx, `UPDATE issue SET status = 'in_review' WHERE id = $1`, issueID); err != nil {
		t.Fatalf("enter review: %v", err)
	}
	var generation int64
	if err := conn.QueryRow(ctx, `SELECT review_generation FROM issue WHERE id = $1`, issueID).Scan(&generation); err != nil || generation != 1 {
		t.Fatalf("generation after entering review = %d (err %v), want 1", generation, err)
	}
	if _, err := conn.Exec(ctx, `UPDATE issue SET status = 'in_review' WHERE id = $1`, issueID); err != nil {
		t.Fatalf("repeat review status: %v", err)
	}
	if err := conn.QueryRow(ctx, `SELECT review_generation FROM issue WHERE id = $1`, issueID).Scan(&generation); err != nil || generation != 1 {
		t.Fatalf("generation after same-status write = %d (err %v), want 1", generation, err)
	}
	// A second handoff advances the round.
	if _, err := conn.Exec(ctx, `UPDATE issue SET status = 'in_progress' WHERE id = $1`, issueID); err != nil {
		t.Fatalf("leave review: %v", err)
	}
	if _, err := conn.Exec(ctx, `UPDATE issue SET status = 'in_review' WHERE id = $1`, issueID); err != nil {
		t.Fatalf("re-enter review: %v", err)
	}
	if err := conn.QueryRow(ctx, `SELECT review_generation FROM issue WHERE id = $1`, issueID).Scan(&generation); err != nil || generation != 2 {
		t.Fatalf("generation after second handoff = %d (err %v), want 2", generation, err)
	}

	// Three active rows for the same reviewer/round, as the old mention +
	// wakeup paths could produce: two already running and one still queued.
	if _, err := conn.Exec(ctx, `
		INSERT INTO agent_task_queue (agent_id, issue_id, status, created_at, workflow_step, trigger_evidence_kind)
		VALUES ($1, $2, 'running', now() - interval '3 minutes', 'in_review', 'issue_wakeup'),
		       ($1, $2, 'running', now() - interval '2 minutes', 'in_review', 'issue_wakeup'),
		       ($1, $2, 'queued',  now() - interval '1 minute', 'in_review', 'issue_wakeup')
	`, agentID, issueID); err != nil {
		t.Fatalf("seed duplicate review tasks: %v", err)
	}

	applyMigrationFile(t, ctx, conn.Conn(), "600_review_task_dedupe_backfill.up.sql")
	const expectedKey = "review:" + issueID + ":" + agentID + ":2"
	var keyed, cancelled, marked int
	if err := conn.QueryRow(ctx, `SELECT count(*) FROM agent_task_queue WHERE dedupe_key = $1`, expectedKey).Scan(&keyed); err != nil {
		t.Fatalf("count keyed tasks: %v", err)
	}
	if err := conn.QueryRow(ctx, `
		SELECT count(*) FROM agent_task_queue
		WHERE status = 'cancelled' AND failure_reason = 'review_superseded'
	`).Scan(&cancelled); err != nil {
		t.Fatalf("count cancelled duplicates: %v", err)
	}
	if err := conn.QueryRow(ctx, `
		SELECT count(*) FROM agent_task_queue
		WHERE status = 'running' AND dedupe_key IS NULL
		  AND context->>'review_superseded' = 'true'
	`).Scan(&marked); err != nil {
		t.Fatalf("count superseded running duplicates: %v", err)
	}
	if keyed != 1 || cancelled != 1 || marked != 1 {
		t.Fatalf("backfill keyed=%d cancelled=%d marked=%d, want 1/1/1", keyed, cancelled, marked)
	}

	applyMigrationFile(t, ctx, conn.Conn(), "601_agent_task_review_dedupe_key_index.up.sql")
	var keyedTaskID string
	if err := conn.QueryRow(ctx, `SELECT id::text FROM agent_task_queue WHERE dedupe_key = $1`, expectedKey).Scan(&keyedTaskID); err != nil {
		t.Fatalf("load keyed review task: %v", err)
	}
	// Previous release shape: a deferred retry and a queued review task share
	// one key because RequestReview could not see the deferred row. v1 does not
	// cover deferred, so both are allowed until the cleanup runs.
	if _, err := conn.Exec(ctx, `
		UPDATE agent_task_queue
		SET status = 'deferred', fire_at = now() + interval '10 minutes'
		WHERE id = $1::uuid
	`, keyedTaskID); err != nil {
		t.Fatalf("defer old review task: %v", err)
	}
	var queuedTaskID string
	if err := conn.QueryRow(ctx, `
		INSERT INTO agent_task_queue (agent_id, issue_id, status, dedupe_key)
		VALUES ($1, $2, 'queued', $3)
		RETURNING id::text
	`, agentID, issueID, expectedKey).Scan(&queuedTaskID); err != nil {
		t.Fatalf("seed dirty deferred/queued pair: %v", err)
	}
	applyMigrationFile(t, ctx, conn.Conn(), "602_review_task_deferred_retry_cleanup.up.sql")
	var live, cancelledDuplicates int
	if err := conn.QueryRow(ctx, `
		SELECT count(*) FROM agent_task_queue
		WHERE dedupe_key = $1
		  AND status IN ('queued', 'deferred', 'dispatched', 'running', 'waiting_local_directory')
	`, expectedKey).Scan(&live); err != nil {
		t.Fatalf("count live keyed tasks after cleanup: %v", err)
	}
	if err := conn.QueryRow(ctx, `
		SELECT count(*) FROM agent_task_queue
		WHERE id = $1::uuid AND status = 'cancelled' AND failure_reason = 'review_superseded'
	`, keyedTaskID).Scan(&cancelledDuplicates); err != nil {
		t.Fatalf("check retired deferred retry: %v", err)
	}
	if live != 1 || cancelledDuplicates != 1 {
		t.Fatalf("cleanup live=%d retired=%d, want 1/1", live, cancelledDuplicates)
	}

	applyMigrationFile(t, ctx, conn.Conn(), "603_agent_task_review_dedupe_key_index_v2.up.sql")
	applyMigrationFile(t, ctx, conn.Conn(), "604_drop_agent_task_review_dedupe_key_v1.up.sql")
	var indexes int
	if err := conn.QueryRow(ctx, `
		SELECT count(*) FROM pg_indexes
		WHERE schemaname = $1
		  AND indexname IN ('idx_agent_task_queue_review_dedupe_key', 'idx_agent_task_queue_review_dedupe_key_v2')
	`, schema).Scan(&indexes); err != nil {
		t.Fatalf("count review indexes: %v", err)
	}
	if indexes != 1 {
		t.Fatalf("review indexes = %d, want only the v2 index", indexes)
	}
	// The review is retried after a runtime failure: the child is deferred and
	// still holds the round key.
	if _, err := conn.Exec(ctx, `
		UPDATE agent_task_queue
		SET status = 'deferred', fire_at = now() + interval '10 minutes'
		WHERE id = $1::uuid
	`, queuedTaskID); err != nil {
		t.Fatalf("defer review task: %v", err)
	}
	if _, err := conn.Exec(ctx, `
		INSERT INTO agent_task_queue (agent_id, issue_id, status, dedupe_key)
		VALUES ($1, $2, 'deferred', $3)
	`, agentID, issueID, expectedKey); !isUniqueViolation(err) {
		t.Fatalf("duplicate deferred review key: got %v, want unique violation", err)
	}
	// Promoting the retry keeps the same row and key.
	if _, err := conn.Exec(ctx, `
		UPDATE agent_task_queue SET status = 'queued', fire_at = NULL WHERE id = $1::uuid
	`, queuedTaskID); err != nil {
		t.Fatalf("promote deferred review task: %v", err)
	}
	if _, err := conn.Exec(ctx, `
		INSERT INTO agent_task_queue (agent_id, issue_id, status, dedupe_key)
		VALUES ($1, $2, 'queued', $3)
	`, agentID, issueID, expectedKey); !isUniqueViolation(err) {
		t.Fatalf("duplicate active review key: got %v, want unique violation", err)
	}
	// Terminal rows release the partial key.
	if _, err := conn.Exec(ctx, `
		UPDATE agent_task_queue SET status = 'completed', completed_at = now() WHERE id = $1::uuid
	`, queuedTaskID); err != nil {
		t.Fatalf("complete review task: %v", err)
	}
	if _, err := conn.Exec(ctx, `
		INSERT INTO agent_task_queue (agent_id, issue_id, status, dedupe_key)
		VALUES ($1, $2, 'queued', $3)
	`, agentID, issueID, expectedKey); err != nil {
		t.Fatalf("reuse key after terminal: %v", err)
	}

	applyMigrationFile(t, ctx, conn.Conn(), "604_drop_agent_task_review_dedupe_key_v1.down.sql")
	applyMigrationFile(t, ctx, conn.Conn(), "603_agent_task_review_dedupe_key_index_v2.down.sql")
	applyMigrationFile(t, ctx, conn.Conn(), "601_agent_task_review_dedupe_key_index.down.sql")
	applyMigrationFile(t, ctx, conn.Conn(), "600_review_task_dedupe_backfill.down.sql")
	applyMigrationFile(t, ctx, conn.Conn(), "599_review_task_dedupe_schema.down.sql")
	var columns int
	if err := conn.QueryRow(ctx, `
		SELECT count(*) FROM information_schema.columns
		WHERE table_schema = $1 AND table_name = 'agent_task_queue' AND column_name = 'dedupe_key'
	`, schema).Scan(&columns); err != nil {
		t.Fatalf("check dedupe_key column: %v", err)
	}
	if columns != 0 {
		t.Fatalf("dedupe_key column survived rollback")
	}
}
