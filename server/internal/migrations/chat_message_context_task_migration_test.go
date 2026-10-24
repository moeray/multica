package migrations

import (
	"context"
	"os"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// TestChatMessageContextTaskMigration covers the column the mirror injection
// depends on: nullable (every existing row starts unowned), and a
// (chat_session_id, context_task_id) index behind both the turn-creation stamp
// and the claim-side read.
func TestChatMessageContextTaskMigration(t *testing.T) {
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

	const schema = "chat_message_context_task_migration_test"
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
		CREATE TABLE chat_message (
			id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
			chat_session_id UUID NOT NULL,
			role TEXT NOT NULL,
			content TEXT NOT NULL,
			task_id UUID
		)
	`); err != nil {
		t.Fatalf("create pre-migration chat_message: %v", err)
	}

	applyMigrationFile(t, ctx, conn.Conn(), "605_chat_message_context_task_id.up.sql")
	applyMigrationFile(t, ctx, conn.Conn(), "606_chat_message_context_task_index.up.sql")

	var columnType string
	if err := conn.QueryRow(ctx, `
		SELECT data_type FROM information_schema.columns
		WHERE table_schema = $1 AND table_name = 'chat_message' AND column_name = 'context_task_id'
	`, schema).Scan(&columnType); err != nil {
		t.Fatalf("inspecting context_task_id: %v", err)
	}
	if columnType != "uuid" {
		t.Fatalf("context_task_id type = %q, want uuid", columnType)
	}
	if _, err := conn.Exec(ctx, `
		INSERT INTO chat_message (chat_session_id, role, content)
		VALUES ('00000000-0000-0000-0000-000000000001', 'assistant', 'mirror')
	`); err != nil {
		t.Fatalf("insert unowned row: %v", err)
	}
	var stamped int
	if err := conn.QueryRow(ctx, `SELECT count(*) FROM chat_message WHERE context_task_id IS NOT NULL`).Scan(&stamped); err != nil {
		t.Fatalf("count stamped rows: %v", err)
	}
	if stamped != 0 {
		t.Fatalf("existing rows started stamped (%d), want all NULL", stamped)
	}

	var indexDef string
	if err := conn.QueryRow(ctx, `
		SELECT indexdef FROM pg_indexes
		WHERE schemaname = $1 AND indexname = 'idx_chat_message_context_task'
	`, schema).Scan(&indexDef); err != nil {
		t.Fatalf("index missing: %v", err)
	}

	applyMigrationFile(t, ctx, conn.Conn(), "606_chat_message_context_task_index.down.sql")
	applyMigrationFile(t, ctx, conn.Conn(), "605_chat_message_context_task_id.down.sql")
	var columns int
	if err := conn.QueryRow(ctx, `
		SELECT count(*) FROM information_schema.columns
		WHERE table_schema = $1 AND table_name = 'chat_message' AND column_name = 'context_task_id'
	`, schema).Scan(&columns); err != nil {
		t.Fatalf("inspect chat_message columns: %v", err)
	}
	if columns != 0 {
		t.Fatalf("down migration left %d context_task_id columns", columns)
	}
	var indexes int
	if err := conn.QueryRow(ctx, `
		SELECT count(*) FROM pg_indexes
		WHERE schemaname = $1 AND indexname = 'idx_chat_message_context_task'
	`, schema).Scan(&indexes); err != nil {
		t.Fatalf("inspect index: %v", err)
	}
	if indexes != 0 {
		t.Fatalf("down migration left the context_task index in place")
	}
}
