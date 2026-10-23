package migrations

import (
	"context"
	"os"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

func TestAgentMessageChatMessageMigration(t *testing.T) {
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

	const schema = "agent_message_migration_test"
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
			content TEXT NOT NULL
		)
	`); err != nil {
		t.Fatalf("create pre-migration chat_message: %v", err)
	}

	applyMigrationFile(t, ctx, conn.Conn(), "597_chat_message_agent_message.up.sql")
	applyMigrationFile(t, ctx, conn.Conn(), "598_chat_message_dedupe_key_index.up.sql")
	if _, err := conn.Exec(ctx, `
		INSERT INTO chat_message (chat_session_id, content, dedupe_key)
		VALUES ('00000000-0000-0000-0000-000000000001', 'first', 'review:1')
	`); err != nil {
		t.Fatalf("insert deduped chat message: %v", err)
	}
	if _, err := conn.Exec(ctx, `
		INSERT INTO chat_message (chat_session_id, content, dedupe_key)
		VALUES ('00000000-0000-0000-0000-000000000001', 'duplicate', 'review:1')
	`); !isUniqueViolation(err) {
		t.Fatalf("duplicate dedupe key: got %v, want unique violation", err)
	}
	// NULL dedupe keys stay outside the partial unique index.
	if _, err := conn.Exec(ctx, `
		INSERT INTO chat_message (chat_session_id, content) VALUES
			('00000000-0000-0000-0000-000000000001', 'plain one'),
			('00000000-0000-0000-0000-000000000001', 'plain two')
	`); err != nil {
		t.Fatalf("insert NULL dedupe keys: %v", err)
	}

	applyMigrationFile(t, ctx, conn.Conn(), "598_chat_message_dedupe_key_index.down.sql")
	applyMigrationFile(t, ctx, conn.Conn(), "597_chat_message_agent_message.down.sql")
	var columns int
	if err := conn.QueryRow(ctx, `
		SELECT count(*) FROM information_schema.columns
		WHERE table_schema = $1 AND table_name = 'chat_message'
		  AND column_name IN ('actor_agent_id', 'review_passed', 'dedupe_key')
	`, schema).Scan(&columns); err != nil {
		t.Fatalf("inspect chat_message columns: %v", err)
	}
	if columns != 0 {
		t.Fatalf("down migration left %d agent-message columns", columns)
	}
}
