package migrations

import (
	"context"
	"errors"
	"os"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestAutopilotMessageExecutionModeMigration(t *testing.T) {
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

	if _, err := conn.Exec(ctx, `
		CREATE TEMP TABLE autopilot (
			execution_mode TEXT NOT NULL DEFAULT 'create_issue'
				CHECK (execution_mode IN ('create_issue', 'run_only'))
		)
	`); err != nil {
		t.Fatalf("create pre-migration autopilot table: %v", err)
	}
	applyMigrationFile(t, ctx, conn.Conn(), "592_autopilot_execution_mode_message.up.sql")
	if _, err := conn.Exec(ctx, `INSERT INTO autopilot (execution_mode) VALUES ('message')`); err != nil {
		t.Fatalf("insert message mode after up migration: %v", err)
	}

	applyMigrationFile(t, ctx, conn.Conn(), "592_autopilot_execution_mode_message.down.sql")
	var mode string
	if err := conn.QueryRow(ctx, `SELECT execution_mode FROM autopilot`).Scan(&mode); err != nil {
		t.Fatalf("read downgraded mode: %v", err)
	}
	if mode != "create_issue" {
		t.Fatalf("down migration mode = %q, want create_issue", mode)
	}
	if _, err := conn.Exec(ctx, `INSERT INTO autopilot (execution_mode) VALUES ('message')`); !isCheckViolation(err) {
		t.Fatalf("insert message mode after down migration: got %v, want check violation", err)
	}
}

func TestInboxItemAutopilotMessageMigration(t *testing.T) {
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

	const schema = "autopilot_message_migration_test"
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
		CREATE TABLE inbox_item (
			id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
			workspace_id UUID NOT NULL
		)
	`); err != nil {
		t.Fatalf("create pre-migration inbox_item: %v", err)
	}

	applyMigrationFile(t, ctx, conn.Conn(), "593_inbox_item_autopilot_message.up.sql")
	applyMigrationFile(t, ctx, conn.Conn(), "594_inbox_item_dedupe_key_index.up.sql")
	if _, err := conn.Exec(ctx, `
		INSERT INTO inbox_item (workspace_id, dedupe_key)
		VALUES ('00000000-0000-0000-0000-000000000001', 'autopilot_report:run-1')
	`); err != nil {
		t.Fatalf("insert deduped inbox item: %v", err)
	}
	if _, err := conn.Exec(ctx, `
		INSERT INTO inbox_item (workspace_id, dedupe_key)
		VALUES ('00000000-0000-0000-0000-000000000001', 'autopilot_report:run-1')
	`); !isUniqueViolation(err) {
		t.Fatalf("duplicate dedupe key: got %v, want unique violation", err)
	}
	// NULL dedupe keys stay outside the partial unique index.
	if _, err := conn.Exec(ctx, `
		INSERT INTO inbox_item (workspace_id) VALUES
			('00000000-0000-0000-0000-000000000001'),
			('00000000-0000-0000-0000-000000000001')
	`); err != nil {
		t.Fatalf("insert NULL dedupe keys: %v", err)
	}

	applyMigrationFile(t, ctx, conn.Conn(), "594_inbox_item_dedupe_key_index.down.sql")
	applyMigrationFile(t, ctx, conn.Conn(), "593_inbox_item_autopilot_message.down.sql")
	var columns int
	if err := conn.QueryRow(ctx, `
		SELECT count(*) FROM information_schema.columns
		WHERE table_schema = $1 AND table_name = 'inbox_item'
		  AND column_name IN ('chat_session_id', 'chat_message_id', 'dedupe_key')
	`, schema).Scan(&columns); err != nil {
		t.Fatalf("inspect inbox_item columns: %v", err)
	}
	if columns != 0 {
		t.Fatalf("down migration left %d autopilot columns", columns)
	}
}

func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}
