package service

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

// chatContextFixture is the minimum a chat turn needs: a workspace with a
// member, an agent on a runtime, and one chat session.
type chatContextFixture struct {
	workspaceID string
	sessionID   string
	agentID     string
	runtimeID   string
}

func seedChatContextFixture(t *testing.T, pool *pgxpool.Pool) chatContextFixture {
	t.Helper()
	ctx := context.Background()
	suffix := time.Now().UnixNano()

	var userID string
	if err := pool.QueryRow(ctx, `INSERT INTO "user" (name, email) VALUES ('ctx user', $1) RETURNING id`,
		fmt.Sprintf("ctx-%d@multica.test", suffix)).Scan(&userID); err != nil {
		t.Fatalf("seed user: %v", err)
	}
	t.Cleanup(func() { pool.Exec(context.Background(), `DELETE FROM "user" WHERE id = $1`, userID) })

	var workspaceID string
	if err := pool.QueryRow(ctx, `INSERT INTO workspace (name, slug) VALUES ('ctx ws', $1) RETURNING id`,
		fmt.Sprintf("ctx-%d", suffix)).Scan(&workspaceID); err != nil {
		t.Fatalf("seed workspace: %v", err)
	}
	t.Cleanup(func() { pool.Exec(context.Background(), `DELETE FROM workspace WHERE id = $1`, workspaceID) })

	if _, err := pool.Exec(ctx, `INSERT INTO member (workspace_id, user_id, role) VALUES ($1, $2, 'owner')`,
		workspaceID, userID); err != nil {
		t.Fatalf("seed member: %v", err)
	}

	var runtimeID string
	if err := pool.QueryRow(ctx, `
		INSERT INTO agent_runtime (workspace_id, name, runtime_mode, provider, status, device_info, metadata, owner_id)
		VALUES ($1, 'ctx-runtime', 'cloud', 'codex', 'online', '', '{}'::jsonb, $2)
		RETURNING id`, workspaceID, userID).Scan(&runtimeID); err != nil {
		t.Fatalf("seed runtime: %v", err)
	}

	var agentID string
	if err := pool.QueryRow(ctx, `
		INSERT INTO agent (workspace_id, name, runtime_mode, runtime_config, runtime_id, visibility,
			max_concurrent_tasks, owner_id, instructions, custom_env, custom_args)
		VALUES ($1, 'ctx-agent', 'cloud', '{}'::jsonb, $2, 'workspace', 1, $3, '', '{}'::jsonb, '[]'::jsonb)
		RETURNING id`, workspaceID, runtimeID, userID).Scan(&agentID); err != nil {
		t.Fatalf("seed agent: %v", err)
	}

	var sessionID string
	if err := pool.QueryRow(ctx, `
		INSERT INTO chat_session (workspace_id, agent_id, creator_id, title)
		VALUES ($1, $2, $3, 'ctx session') RETURNING id`, workspaceID, agentID, userID).Scan(&sessionID); err != nil {
		t.Fatalf("seed chat session: %v", err)
	}
	t.Cleanup(func() { pool.Exec(context.Background(), `DELETE FROM chat_session WHERE id = $1`, sessionID) })

	return chatContextFixture{workspaceID: workspaceID, sessionID: sessionID, agentID: agentID, runtimeID: runtimeID}
}

// insertChatTask creates a queued chat turn stamped at createdAt, which is what
// the link query compares message rows against.
func insertChatTask(t *testing.T, pool *pgxpool.Pool, fixture chatContextFixture, createdAt time.Time) string {
	t.Helper()
	var taskID string
	if err := pool.QueryRow(context.Background(), `
		INSERT INTO agent_task_queue (agent_id, chat_session_id, runtime_id, status, created_at)
		VALUES ($1, $2, $3, 'queued', $4) RETURNING id`,
		fixture.agentID, fixture.sessionID, fixture.runtimeID, createdAt).Scan(&taskID); err != nil {
		t.Fatalf("seed chat task: %v", err)
	}
	t.Cleanup(func() { pool.Exec(context.Background(), `DELETE FROM agent_task_queue WHERE id = $1`, taskID) })
	return taskID
}

type chatMessageSeed struct {
	role      string
	content   string
	kind      string
	taskID    string
	createdAt time.Time
}

func insertChatMessage(t *testing.T, pool *pgxpool.Pool, sessionID string, seed chatMessageSeed) string {
	t.Helper()
	var taskID any
	if seed.taskID != "" {
		taskID = seed.taskID
	}
	kind := seed.kind
	if kind == "" {
		kind = protocol.ChatMessageKindMessage
	}
	var id string
	if err := pool.QueryRow(context.Background(), `
		INSERT INTO chat_message (chat_session_id, role, content, message_kind, task_id, created_at)
		VALUES ($1, $2, $3, $4, $5, $6) RETURNING id`,
		sessionID, seed.role, seed.content, kind, taskID, seed.createdAt).Scan(&id); err != nil {
		t.Fatalf("seed chat message: %v", err)
	}
	return id
}

func contextTaskIDOf(t *testing.T, pool *pgxpool.Pool, messageID string) pgtype.UUID {
	t.Helper()
	var got pgtype.UUID
	if err := pool.QueryRow(context.Background(),
		`SELECT context_task_id FROM chat_message WHERE id = $1`, messageID).Scan(&got); err != nil {
		t.Fatalf("load context_task_id: %v", err)
	}
	return got
}

// TestLinkPendingAgentChatMessagesToTaskStampsOnlyThisTurnsMirrors is the
// attach rule (acceptance 1) and its two boundaries: a message that arrives
// while the turn is already created belongs to the NEXT turn, and an ordinary
// reply belongs to the turn that produced it.
func TestLinkPendingAgentChatMessagesToTaskStampsOnlyThisTurnsMirrors(t *testing.T) {
	pool := sharedTestPool(t)
	ctx := context.Background()
	q := db.New(pool)
	fixture := seedChatContextFixture(t, pool)

	turnStarted := time.Now().Add(-time.Minute)
	oldMirrorAt := turnStarted.Add(-time.Minute)
	taskID := insertChatTask(t, pool, fixture, turnStarted)

	review := insertChatMessage(t, pool, fixture.sessionID, chatMessageSeed{
		role: "assistant", kind: protocol.ChatMessageKindReviewResult,
		content: protocol.BuildReviewResultContent(true, "通过。", "MP-67"), createdAt: oldMirrorAt,
	})
	otherKind := insertChatMessage(t, pool, fixture.sessionID, chatMessageSeed{
		role: "assistant", kind: "status_note", content: "构建完成。", createdAt: oldMirrorAt.Add(time.Second),
	})
	producedReply := insertChatMessage(t, pool, fixture.sessionID, chatMessageSeed{
		role: "assistant", content: "普通回复", taskID: taskID, createdAt: oldMirrorAt,
	})
	userMessage := insertChatMessage(t, pool, fixture.sessionID, chatMessageSeed{
		role: "user", content: "用户发言", createdAt: oldMirrorAt,
	})
	// The onboarding opening is the one assistant row with no producing turn
	// that is NOT a mirror: it is the member's own opening reply, already read
	// and already quoted by the kickoff. Injecting it would show Mika her own
	// introduction a third time.
	onboardingOpening := insertChatMessage(t, pool, fixture.sessionID, chatMessageSeed{
		role: "assistant", kind: protocol.ChatMessageKindOnboardingOpening,
		content: "你好，我是 Mika。", createdAt: oldMirrorAt,
	})
	arrivedDuringTurn := insertChatMessage(t, pool, fixture.sessionID, chatMessageSeed{
		role: "assistant", kind: protocol.ChatMessageKindReviewResult,
		content:   protocol.BuildReviewResultContent(false, "不通过。", "MP-68"),
		createdAt: turnStarted.Add(time.Second),
	})

	if err := q.LinkPendingAgentChatMessagesToTask(ctx, db.LinkPendingAgentChatMessagesToTaskParams{
		TaskID: util.MustParseUUID(taskID), ChatSessionID: util.MustParseUUID(fixture.sessionID),
	}); err != nil {
		t.Fatalf("link pending agent messages: %v", err)
	}

	turn := util.MustParseUUID(taskID)
	for _, want := range []struct {
		name string
		id   string
	}{{"review_result mirror", review}, {"assistant row of another kind", otherKind}} {
		if got := contextTaskIDOf(t, pool, want.id); got != turn {
			t.Errorf("%s context_task_id = %s, want this turn", want.name, util.UUIDToString(got))
		}
	}
	for _, want := range []struct {
		name string
		id   string
	}{{"reply produced by a turn", producedReply}, {"user message", userMessage}, {"onboarding opening", onboardingOpening}, {"message that arrived after the turn", arrivedDuringTurn}} {
		if got := contextTaskIDOf(t, pool, want.id); got.Valid {
			t.Errorf("%s was stamped with %s, want untouched", want.name, util.UUIDToString(got))
		}
	}

	// The stamped set is what the claim reads back, in transcript order.
	rows, err := q.ListChatContextMessagesForTask(ctx, db.ListChatContextMessagesForTaskParams{
		ChatSessionID: util.MustParseUUID(fixture.sessionID),
		ContextTaskID: turn,
	})
	if err != nil {
		t.Fatalf("list context messages: %v", err)
	}
	if len(rows) != 2 || util.UUIDToString(rows[0].ID) != review || util.UUIDToString(rows[1].ID) != otherKind {
		t.Fatalf("claim read = %d rows, want the two mirrors in transcript order", len(rows))
	}
}

// TestLinkPendingAgentChatMessagesToTaskDoesNotReplay is acceptance 4/6: the
// same mirror is injected exactly once. A repeated link (a reclaimed task) is a
// no-op, and the following turn gets only what arrived after the first one.
func TestLinkPendingAgentChatMessagesToTaskDoesNotReplay(t *testing.T) {
	pool := sharedTestPool(t)
	ctx := context.Background()
	q := db.New(pool)
	fixture := seedChatContextFixture(t, pool)

	firstTurnAt := time.Now().Add(-time.Minute)
	firstTask := insertChatTask(t, pool, fixture, firstTurnAt)
	firstMirror := insertChatMessage(t, pool, fixture.sessionID, chatMessageSeed{
		role: "assistant", kind: protocol.ChatMessageKindReviewResult,
		content:   protocol.BuildReviewResultContent(true, "通过。", "MP-67"),
		createdAt: firstTurnAt.Add(-time.Minute),
	})

	link := func(taskID string) {
		t.Helper()
		if err := q.LinkPendingAgentChatMessagesToTask(ctx, db.LinkPendingAgentChatMessagesToTaskParams{
			TaskID: util.MustParseUUID(taskID), ChatSessionID: util.MustParseUUID(fixture.sessionID),
		}); err != nil {
			t.Fatalf("link pending agent messages: %v", err)
		}
	}

	link(firstTask)
	// A second claim of the same turn (redelivery) must not move or duplicate
	// anything.
	link(firstTask)

	if got := contextTaskIDOf(t, pool, firstMirror); util.UUIDToString(got) != firstTask {
		t.Fatalf("mirror after repeated link = %s, want %s", util.UUIDToString(got), firstTask)
	}

	secondTurnAt := time.Now()
	secondTask := insertChatTask(t, pool, fixture, secondTurnAt)
	secondMirror := insertChatMessage(t, pool, fixture.sessionID, chatMessageSeed{
		role: "assistant", kind: protocol.ChatMessageKindReviewResult,
		content:   protocol.BuildReviewResultContent(false, "不通过。", "MP-68"),
		createdAt: secondTurnAt.Add(-time.Second),
	})
	link(secondTask)

	if got := contextTaskIDOf(t, pool, firstMirror); util.UUIDToString(got) != firstTask {
		t.Errorf("first mirror was replayed onto the second turn: %s", util.UUIDToString(got))
	}
	if got := contextTaskIDOf(t, pool, secondMirror); util.UUIDToString(got) != secondTask {
		t.Errorf("second mirror = %s, want %s", util.UUIDToString(got), secondTask)
	}
}

// TestInheritChatContextMessagesForRetryTaskMovesTheStamp is acceptance 6's
// retry half: an auto-retry clone is the same turn retried, so it reads the
// same mirrors — and the failed parent stops owning them, so nothing can be
// injected twice.
func TestInheritChatContextMessagesForRetryTaskMovesTheStamp(t *testing.T) {
	pool := sharedTestPool(t)
	ctx := context.Background()
	q := db.New(pool)
	fixture := seedChatContextFixture(t, pool)

	parentAt := time.Now().Add(-time.Minute)
	parentTask := insertChatTask(t, pool, fixture, parentAt)
	mirror := insertChatMessage(t, pool, fixture.sessionID, chatMessageSeed{
		role: "assistant", kind: protocol.ChatMessageKindReviewResult,
		content:   protocol.BuildReviewResultContent(true, "通过。", "MP-67"),
		createdAt: parentAt.Add(-time.Minute),
	})
	if err := q.LinkPendingAgentChatMessagesToTask(ctx, db.LinkPendingAgentChatMessagesToTaskParams{
		TaskID: util.MustParseUUID(parentTask), ChatSessionID: util.MustParseUUID(fixture.sessionID),
	}); err != nil {
		t.Fatalf("link pending agent messages: %v", err)
	}

	childTask := insertChatTask(t, pool, fixture, time.Now())
	if err := q.InheritChatContextMessagesForRetryTask(ctx, db.InheritChatContextMessagesForRetryTaskParams{
		ChildTaskID: util.MustParseUUID(childTask), ParentTaskID: util.MustParseUUID(parentTask),
	}); err != nil {
		t.Fatalf("inherit retry chat context messages: %v", err)
	}

	if got := contextTaskIDOf(t, pool, mirror); util.UUIDToString(got) != childTask {
		t.Fatalf("mirror after inheritance = %s, want child %s", util.UUIDToString(got), childTask)
	}
	parentRows, err := q.ListChatContextMessagesForTask(ctx, db.ListChatContextMessagesForTaskParams{
		ChatSessionID: util.MustParseUUID(fixture.sessionID),
		ContextTaskID: util.MustParseUUID(parentTask),
	})
	if err != nil {
		t.Fatalf("list parent context messages: %v", err)
	}
	if len(parentRows) != 0 {
		t.Fatalf("parent still owns %d injected messages after inheritance", len(parentRows))
	}
}
