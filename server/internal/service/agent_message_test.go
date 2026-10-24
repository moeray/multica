package service

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/multica-ai/multica/server/internal/events"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

type recordingAgentMessageSender struct {
	calls   int
	binding db.ChannelChatSessionBinding
	message db.ChatMessage
	err     error
}

type fakeIssueChatQueries struct {
	tasks    map[string]db.AgentTaskQueue
	sessions map[string]db.ChatSession
}

func (q fakeIssueChatQueries) GetAgentTask(_ context.Context, id pgtype.UUID) (db.AgentTaskQueue, error) {
	if task, ok := q.tasks[util.UUIDToString(id)]; ok {
		return task, nil
	}
	return db.AgentTaskQueue{}, pgx.ErrNoRows
}

func (q fakeIssueChatQueries) GetChatSession(_ context.Context, id pgtype.UUID) (db.ChatSession, error) {
	if session, ok := q.sessions[util.UUIDToString(id)]; ok {
		return session, nil
	}
	return db.ChatSession{}, pgx.ErrNoRows
}

func (s *recordingAgentMessageSender) Send(_ context.Context, binding db.ChannelChatSessionBinding, msg db.ChatMessage) error {
	s.calls++
	s.binding = binding
	s.message = msg
	return s.err
}

func seedReviewerAgent(t *testing.T, pool *pgxpool.Pool, workspaceID, runtimeID, ownerID, name string) string {
	t.Helper()
	var agentID string
	if err := pool.QueryRow(context.Background(), `
		INSERT INTO agent (workspace_id, name, runtime_mode, runtime_config, runtime_id, visibility,
			max_concurrent_tasks, owner_id, instructions, custom_env, custom_args)
		VALUES ($1, $2, 'cloud', '{}'::jsonb, $3, 'workspace', 1, $4, '', '{}'::jsonb, '[]'::jsonb)
		RETURNING id`, workspaceID, name, runtimeID, ownerID).Scan(&agentID); err != nil {
		t.Fatalf("seed reviewer agent: %v", err)
	}
	t.Cleanup(func() {
		pool.Exec(context.Background(), `DELETE FROM agent WHERE id = $1`, agentID)
	})
	return agentID
}

func seedReviewComment(t *testing.T, pool *pgxpool.Pool, workspaceID, issueID, reviewerID, body string) string {
	t.Helper()
	var commentID string
	if err := pool.QueryRow(context.Background(), `
		INSERT INTO comment (workspace_id, issue_id, author_type, author_id, content)
		VALUES ($1, $2, 'agent', $3, $4)
		RETURNING id`, workspaceID, issueID, reviewerID, body).Scan(&commentID); err != nil {
		t.Fatalf("seed review comment: %v", err)
	}
	t.Cleanup(func() {
		pool.Exec(context.Background(), `DELETE FROM comment WHERE id = $1`, commentID)
	})
	return commentID
}

func TestResolveIssueChatSessionUsesOnlyKnownOrigins(t *testing.T) {
	ctx := context.Background()
	workspaceID := util.MustParseUUID("11111111-1111-4111-8111-111111111111")
	sessionID := util.MustParseUUID("22222222-2222-4222-8222-222222222222")
	taskID := util.MustParseUUID("33333333-3333-4333-8333-333333333333")
	otherWorkspaceSession := util.MustParseUUID("44444444-4444-4444-8444-444444444444")
	queries := fakeIssueChatQueries{
		sessions: map[string]db.ChatSession{
			util.UUIDToString(sessionID): {
				ID:          sessionID,
				WorkspaceID: workspaceID,
			},
			util.UUIDToString(otherWorkspaceSession): {
				ID:          otherWorkspaceSession,
				WorkspaceID: util.MustParseUUID("55555555-5555-4555-8555-555555555555"),
			},
		},
		tasks: map[string]db.AgentTaskQueue{
			util.UUIDToString(taskID): {
				ID:            taskID,
				ChatSessionID: sessionID,
			},
		},
	}
	issue := db.Issue{WorkspaceID: workspaceID}

	for _, originType := range []string{"lark_chat", "slack_chat", "dingtalk_chat", "wecom_chat", "telegram_chat"} {
		issue.OriginType = pgtype.Text{String: originType, Valid: true}
		issue.OriginID = sessionID
		session, err := ResolveIssueChatSession(ctx, queries, issue)
		if err != nil || session == nil || session.ID != sessionID {
			t.Fatalf("%s = (%+v, %v), want session %s", originType, session, err, util.UUIDToString(sessionID))
		}
	}

	for _, originType := range []string{"agent_create", "quick_create"} {
		issue.OriginType = pgtype.Text{String: originType, Valid: true}
		issue.OriginID = taskID
		session, err := ResolveIssueChatSession(ctx, queries, issue)
		if err != nil || session == nil || session.ID != sessionID {
			t.Fatalf("%s = (%+v, %v), want session %s", originType, session, err, util.UUIDToString(sessionID))
		}
	}

	for _, tc := range []struct {
		name       string
		originType string
		originID   pgtype.UUID
	}{
		{"unknown origin", "autopilot", sessionID},
		{"missing task", "agent_create", util.MustParseUUID("66666666-6666-4666-8666-666666666666")},
		{"wrong workspace", "lark_chat", otherWorkspaceSession},
		{"missing session", "lark_chat", util.MustParseUUID("77777777-7777-4777-8777-777777777777")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			issue.OriginType = pgtype.Text{String: tc.originType, Valid: true}
			issue.OriginID = tc.originID
			session, err := ResolveIssueChatSession(ctx, queries, issue)
			if err != nil || session != nil {
				t.Fatalf("resolve = (%+v, %v), want (nil, nil)", session, err)
			}
		})
	}
}

func TestResolveIssueChatSession(t *testing.T) {
	pool := sharedTestPool(t)
	ctx := context.Background()
	q := db.New(pool)
	workspaceID, userID, agentID, issueID := seedAttributionFixture(t, pool)
	_, sessionID := seedLarkP2PSession(t, pool, workspaceID, agentID, userID)

	issue, err := q.GetIssue(ctx, util.MustParseUUID(issueID))
	if err != nil {
		t.Fatalf("load issue: %v", err)
	}

	for _, originType := range []string{"lark_chat", "slack_chat", "dingtalk_chat", "wecom_chat", "telegram_chat"} {
		t.Run(originType, func(t *testing.T) {
			originIssue := issue
			originIssue.OriginType = pgtype.Text{String: originType, Valid: true}
			originIssue.OriginID = util.MustParseUUID(sessionID)
			session, err := ResolveIssueChatSession(ctx, q, originIssue)
			if err != nil {
				t.Fatalf("resolve: %v", err)
			}
			if session == nil || util.UUIDToString(session.ID) != sessionID {
				t.Fatalf("session = %+v, want %s", session, sessionID)
			}
		})
	}

	t.Run("agent_create task", func(t *testing.T) {
		var taskID string
		if err := pool.QueryRow(ctx, `
			INSERT INTO agent_task_queue (agent_id, chat_session_id, status, priority, completed_at)
			VALUES ($1, $2, 'completed', 0, now()) RETURNING id`, agentID, sessionID).Scan(&taskID); err != nil {
			t.Fatalf("seed origin task: %v", err)
		}
		t.Cleanup(func() {
			pool.Exec(context.Background(), `DELETE FROM agent_task_queue WHERE id = $1`, taskID)
		})
		originIssue := issue
		originIssue.OriginType = pgtype.Text{String: "agent_create", Valid: true}
		originIssue.OriginID = util.MustParseUUID(taskID)
		session, err := ResolveIssueChatSession(ctx, q, originIssue)
		if err != nil {
			t.Fatalf("resolve: %v", err)
		}
		if session == nil || util.UUIDToString(session.ID) != sessionID {
			t.Fatalf("session = %+v, want %s", session, sessionID)
		}
	})

	t.Run("unknown origin", func(t *testing.T) {
		originIssue := issue
		originIssue.OriginType = pgtype.Text{String: "autopilot", Valid: true}
		originIssue.OriginID = util.MustParseUUID(sessionID)
		session, err := ResolveIssueChatSession(ctx, q, originIssue)
		if err != nil || session != nil {
			t.Fatalf("unknown origin = (%+v, %v), want (nil, nil)", session, err)
		}
	})

	t.Run("wrong workspace", func(t *testing.T) {
		originIssue := issue
		originIssue.WorkspaceID = util.MustParseUUID("00000000-0000-4000-8000-000000000001")
		originIssue.OriginType = pgtype.Text{String: "lark_chat", Valid: true}
		originIssue.OriginID = util.MustParseUUID(sessionID)
		session, err := ResolveIssueChatSession(ctx, q, originIssue)
		if err != nil || session != nil {
			t.Fatalf("wrong workspace = (%+v, %v), want (nil, nil)", session, err)
		}
	})
}

func TestSendReviewResultPersistsDedupesAndDelivers(t *testing.T) {
	pool := sharedTestPool(t)
	ctx := context.Background()
	q := db.New(pool)
	workspaceID, userID, developerID, issueID := seedAttributionFixture(t, pool)
	installationID, sessionID := seedLarkP2PSession(t, pool, workspaceID, developerID, userID)

	var runtimeID string
	if err := pool.QueryRow(ctx, `SELECT runtime_id FROM agent WHERE id = $1`, developerID).Scan(&runtimeID); err != nil {
		t.Fatalf("load runtime: %v", err)
	}
	reviewerID := seedReviewerAgent(t, pool, workspaceID, runtimeID, userID, "reviewer")
	seedReviewComment(t, pool, workspaceID, issueID, developerID,
		fmt.Sprintf("[@Reviewer](mention://agent/%s) 请评审。", reviewerID))
	seedReviewComment(t, pool, workspaceID, issueID, reviewerID, "阻塞项：缺少权限测试。")

	if _, err := pool.Exec(ctx, `
		UPDATE issue SET origin_type = 'lark_chat', origin_id = $1, status = 'done', revision = revision + 1
		WHERE id = $2`, sessionID, issueID); err != nil {
		t.Fatalf("stamp issue origin: %v", err)
	}
	issue, err := q.GetIssue(ctx, util.MustParseUUID(issueID))
	if err != nil {
		t.Fatalf("load issue: %v", err)
	}

	sender := &recordingAgentMessageSender{}
	svc := NewAgentMessageService(q, events.New(), sender, "https://app.example")
	req := ReviewResultRequest{
		Issue:     issue,
		ActorType: "agent",
		ActorID:   reviewerID,
		DedupeKey: fmt.Sprintf("review_result:%s:%d", issueID, issue.Revision),
		Passed:    true,
	}
	first, err := svc.SendReviewResult(ctx, req)
	if err != nil {
		t.Fatalf("first send: %v", err)
	}
	if !first.Inserted || !first.Delivered {
		t.Fatalf("first result = %+v, want inserted+delivered", first)
	}
	if sender.calls != 1 {
		t.Fatalf("sender calls = %d, want 1", sender.calls)
	}
	if sender.binding.InstallationID.Bytes != util.MustParseUUID(installationID).Bytes {
		t.Fatalf("delivered binding installation = %s, want %s",
			util.UUIDToString(sender.binding.InstallationID), installationID)
	}

	second, err := svc.SendReviewResult(ctx, req)
	if err != nil {
		t.Fatalf("second send: %v", err)
	}
	if second.Inserted || !second.Delivered {
		t.Fatalf("second result = %+v, want existing+delivered", second)
	}
	if sender.calls != 1 {
		t.Fatalf("sender calls after retry = %d, want 1", sender.calls)
	}

	var count int
	if err := pool.QueryRow(ctx, `
		SELECT count(*) FROM chat_message
		WHERE chat_session_id = $1 AND message_kind = 'review_result'`, sessionID).Scan(&count); err != nil {
		t.Fatalf("count review messages: %v", err)
	}
	if count != 1 {
		t.Fatalf("review_result rows = %d, want 1", count)
	}
	var kind, content string
	var actorID pgtype.UUID
	var passed pgtype.Bool
	if err := pool.QueryRow(ctx, `
		SELECT message_kind, content, actor_agent_id, review_passed
		FROM chat_message WHERE chat_session_id = $1 AND message_kind = 'review_result'`,
		sessionID).Scan(&kind, &content, &actorID, &passed); err != nil {
		t.Fatalf("load review message: %v", err)
	}
	if kind != protocol.ChatMessageKindReviewResult || !actorID.Valid || util.UUIDToString(actorID) != reviewerID {
		t.Fatalf("message = kind %q actor %s, want review_result/%s", kind, util.UUIDToString(actorID), reviewerID)
	}
	if !passed.Valid || !passed.Bool {
		t.Fatalf("review_passed = %+v, want true", passed)
	}
	if !strings.Contains(content, "评审结果：通过") ||
		!strings.Contains(content, "阻塞项：缺少权限测试。") ||
		!strings.Contains(content, "/issues/") {
		t.Fatalf("content = %q, want verdict, blockers and issue link", content)
	}
}

func TestSendReviewResultWithoutSourceChatFallsBack(t *testing.T) {
	pool := sharedTestPool(t)
	ctx := context.Background()
	q := db.New(pool)
	workspaceID, userID, developerID, issueID := seedAttributionFixture(t, pool)
	var runtimeID string
	if err := pool.QueryRow(ctx, `SELECT runtime_id FROM agent WHERE id = $1`, developerID).Scan(&runtimeID); err != nil {
		t.Fatalf("load runtime: %v", err)
	}
	reviewerID := seedReviewerAgent(t, pool, workspaceID, runtimeID, userID, "reviewer")
	seedReviewComment(t, pool, workspaceID, issueID, developerID,
		fmt.Sprintf("[@Reviewer](mention://agent/%s) 请评审。", reviewerID))
	issue, err := q.GetIssue(ctx, util.MustParseUUID(issueID))
	if err != nil {
		t.Fatalf("load issue: %v", err)
	}
	svc := NewAgentMessageService(q, events.New(), nil, "")
	_, err = svc.SendReviewResult(ctx, ReviewResultRequest{
		Issue:     issue,
		ActorType: "agent",
		ActorID:   reviewerID,
		Passed:    true,
	})
	if !errors.Is(err, ErrNoSourceChatSession) {
		t.Fatalf("err = %v, want ErrNoSourceChatSession", err)
	}
	var count int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM chat_message WHERE content LIKE '%评审结果%' AND chat_session_id IN (
		SELECT id FROM chat_session WHERE workspace_id = $1)`, workspaceID).Scan(&count); err != nil {
		t.Fatalf("count messages: %v", err)
	}
	if count != 0 {
		t.Fatalf("messages written without a source chat = %d, want 0", count)
	}
}

func TestSendReviewResultRejectsNonAgent(t *testing.T) {
	svc := NewAgentMessageService(nil, events.New(), nil, "")
	_, err := svc.SendReviewResult(context.Background(), ReviewResultRequest{
		ActorType: "member",
		ActorID:   "00000000-0000-4000-8000-000000000001",
	})
	if !errors.Is(err, ErrReviewResultForbidden) {
		t.Fatalf("err = %v, want ErrReviewResultForbidden", err)
	}
}

func TestSendReviewResultRejectsNonReviewerAgent(t *testing.T) {
	pool := sharedTestPool(t)
	ctx := context.Background()
	q := db.New(pool)
	workspaceID, userID, developerID, issueID := seedAttributionFixture(t, pool)
	var runtimeID string
	if err := pool.QueryRow(ctx, `SELECT runtime_id FROM agent WHERE id = $1`, developerID).Scan(&runtimeID); err != nil {
		t.Fatalf("load runtime: %v", err)
	}
	reviewerID := seedReviewerAgent(t, pool, workspaceID, runtimeID, userID, "reviewer")
	intruderID := seedReviewerAgent(t, pool, workspaceID, runtimeID, userID, "intruder")
	seedReviewComment(t, pool, workspaceID, issueID, developerID,
		fmt.Sprintf("[@Reviewer](mention://agent/%s) 请评审。", reviewerID))

	_, sessionID := seedLarkP2PSession(t, pool, workspaceID, developerID, userID)
	if _, err := pool.Exec(ctx, `
		UPDATE issue SET origin_type = 'lark_chat', origin_id = $1, status = 'done', revision = revision + 1
		WHERE id = $2`, sessionID, issueID); err != nil {
		t.Fatalf("stamp issue origin: %v", err)
	}
	issue, err := q.GetIssue(ctx, util.MustParseUUID(issueID))
	if err != nil {
		t.Fatalf("load issue: %v", err)
	}

	svc := NewAgentMessageService(q, events.New(), nil, "")
	_, err = svc.SendReviewResult(ctx, ReviewResultRequest{
		Issue:     issue,
		ActorType: "agent",
		ActorID:   intruderID,
		Passed:    true,
	})
	if !errors.Is(err, ErrReviewResultForbidden) {
		t.Fatalf("err = %v, want ErrReviewResultForbidden", err)
	}
	var count int
	if err := pool.QueryRow(ctx, `
		SELECT count(*) FROM chat_message
		WHERE chat_session_id = $1 AND message_kind = 'review_result'`, sessionID).Scan(&count); err != nil {
		t.Fatalf("count review messages: %v", err)
	}
	if count != 0 {
		t.Fatalf("review_result rows written by a non-reviewer = %d, want 0", count)
	}
}

func TestIsIssueReviewerRequiresHandoffMention(t *testing.T) {
	pool := sharedTestPool(t)
	ctx := context.Background()
	q := db.New(pool)
	workspaceID, userID, developerID, issueID := seedAttributionFixture(t, pool)
	var runtimeID string
	if err := pool.QueryRow(ctx, `SELECT runtime_id FROM agent WHERE id = $1`, developerID).Scan(&runtimeID); err != nil {
		t.Fatalf("load runtime: %v", err)
	}
	reviewerID := seedReviewerAgent(t, pool, workspaceID, runtimeID, userID, "reviewer")
	otherAgentID := seedReviewerAgent(t, pool, workspaceID, runtimeID, userID, "other")
	issue, err := q.GetIssue(ctx, util.MustParseUUID(issueID))
	if err != nil {
		t.Fatalf("load issue: %v", err)
	}

	reviewer, err := IsIssueReviewer(ctx, q, issue, reviewerID)
	if err != nil || reviewer {
		t.Fatalf("without a handoff comment = (%v, %v), want false", reviewer, err)
	}

	seedReviewComment(t, pool, workspaceID, issueID, developerID,
		fmt.Sprintf("[@Reviewer](mention://agent/%s) 请评审。", reviewerID))

	reviewer, err = IsIssueReviewer(ctx, q, issue, reviewerID)
	if err != nil || !reviewer {
		t.Fatalf("mentioned reviewer = (%v, %v), want true", reviewer, err)
	}
	reviewer, err = IsIssueReviewer(ctx, q, issue, otherAgentID)
	if err != nil || reviewer {
		t.Fatalf("unmentioned agent = (%v, %v), want false", reviewer, err)
	}
	reviewer, err = IsIssueReviewer(ctx, q, issue, util.UUIDToString(issue.AssigneeID))
	if err != nil || reviewer {
		t.Fatalf("assignee = (%v, %v), want false", reviewer, err)
	}
	reviewer, err = IsIssueReviewer(ctx, q, issue, userID)
	if err != nil || reviewer {
		t.Fatalf("member = (%v, %v), want false", reviewer, err)
	}
}
