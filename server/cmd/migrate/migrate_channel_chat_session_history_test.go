package main

import (
	"context"
	"math/rand/v2"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

func TestChannelChatSessionHistoryMigrationsUpDownUp(t *testing.T) {
	adminPool := openTestPool(t)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	schema := createScratchSchema(t, ctx, adminPool, "migrate_channel_session_history_")
	pool := openTestPoolWithSearchPath(t, schema)
	if _, err := pool.Exec(ctx, `
		CREATE TABLE channel_chat_session_binding (
			id UUID PRIMARY KEY,
			chat_session_id UUID NOT NULL,
			installation_id UUID NOT NULL,
			channel_chat_id TEXT NOT NULL,
			retired_at TIMESTAMPTZ,
			CONSTRAINT channel_chat_session_binding_chat_session_id_key
				UNIQUE (chat_session_id)
		)
	`); err != nil {
		t.Fatalf("create binding fixture: %v", err)
	}

	versions := []string{
		"595_channel_chat_session_history",
		"596_channel_chat_active_session_index",
	}
	if err := runMigrations(ctx, pool, migrationOptions(t, schema, "up", versions)); err != nil {
		t.Fatalf("apply channel binding history migrations: %v", err)
	}
	assertIndexExists(t, pool, schema, "channel_chat_session_binding_chat_session_id_key", false)
	assertPartialActiveSessionIndex(t, ctx, pool, schema)
	insertBinding(t, ctx, pool, "00000000-0000-0000-0000-000000000001", "chat-1", false)
	insertBinding(t, ctx, pool, "00000000-0000-0000-0000-000000000001", "chat-1", true)

	reverseStrings(versions)
	if err := runMigrations(ctx, pool, migrationOptions(t, schema, "down", versions)); err == nil ||
		!strings.Contains(err.Error(), "multiple bindings") {
		t.Fatalf("rollback with history error = %v, want multiple-bindings refusal", err)
	}
	assertPartialActiveSessionIndex(t, ctx, pool, schema)

	if _, err := pool.Exec(ctx, `
		DELETE FROM channel_chat_session_binding
		WHERE chat_session_id = '00000000-0000-0000-0000-000000000001'
		  AND retired_at IS NOT NULL
	`); err != nil {
		t.Fatalf("remove historical fixture row: %v", err)
	}

	if err := runMigrations(ctx, pool, migrationOptions(t, schema, "down", versions)); err != nil {
		t.Fatalf("roll back channel binding history migrations: %v", err)
	}
	assertIndexValidity(t, pool, schema, "channel_chat_session_binding_chat_session_id_key", true)
	assertIndexExists(t, pool, schema, "idx_channel_chat_session_binding_active_session", false)

	if _, err := pool.Exec(ctx, `
		INSERT INTO channel_chat_session_binding (
			id, chat_session_id, installation_id, channel_chat_id, retired_at
		) VALUES (
			gen_random_uuid(), '00000000-0000-0000-0000-000000000001',
			'00000000-0000-0000-0000-000000000002', 'chat-1', now()
		)
	`); err == nil {
		t.Fatal("old schema accepted a second binding for the same Chat")
	}

	reverseStrings(versions)
	if err := runMigrations(ctx, pool, migrationOptions(t, schema, "up", versions)); err != nil {
		t.Fatalf("reapply channel binding history migrations: %v", err)
	}
	insertBinding(t, ctx, pool, "00000000-0000-0000-0000-000000000001", "chat-1", true)
	assertPartialActiveSessionIndex(t, ctx, pool, schema)
}

func migrationOptions(t *testing.T, schema, direction string, versions []string) runOptions {
	t.Helper()
	return runOptions{
		Direction:             direction,
		Files:                 realMigrationFiles(t, versions, direction),
		SchemaMigrationsTable: schema + ".schema_migrations",
		AdvisoryLockKey:       int64(rand.Uint64()&0x7fffffffffffffff) | 1,
		Hooks:                 hooksForDirection(direction),
	}
}

func insertBinding(t *testing.T, ctx context.Context, pool *pgxpool.Pool, sessionID, chatID string, retired bool) {
	t.Helper()
	var retiredAt any
	if retired {
		retiredAt = time.Now()
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO channel_chat_session_binding (
			id, chat_session_id, installation_id, channel_chat_id, retired_at
		) VALUES (
			gen_random_uuid(), $1, gen_random_uuid(), $2, $3
		)
	`, sessionID, chatID, retiredAt); err != nil {
		t.Fatalf("insert channel binding: %v", err)
	}
}

func assertPartialActiveSessionIndex(t *testing.T, ctx context.Context, pool *pgxpool.Pool, schema string) {
	t.Helper()
	var predicate string
	var valid bool
	if err := pool.QueryRow(ctx, `
		SELECT pg_get_expr(i.indpred, i.indrelid), i.indisvalid
		FROM pg_index AS i
		JOIN pg_class AS c ON c.oid = i.indexrelid
		JOIN pg_namespace AS n ON n.oid = c.relnamespace
		WHERE n.nspname = $1
		  AND c.relname = 'idx_channel_chat_session_binding_active_session'
	`, schema).Scan(&predicate, &valid); err != nil {
		t.Fatalf("read active-session index: %v", err)
	}
	if !valid || !strings.Contains(predicate, "retired_at IS NULL") {
		t.Fatalf("active-session index valid=%v predicate=%q", valid, predicate)
	}
}

func reverseStrings(values []string) {
	for left, right := 0, len(values)-1; left < right; left, right = left+1, right-1 {
		values[left], values[right] = values[right], values[left]
	}
}
