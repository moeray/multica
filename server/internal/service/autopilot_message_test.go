package service

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/multica-ai/multica/server/internal/events"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

// seedMessageAutopilot creates an active message-mode autopilot and a running
// run, mirroring the state DispatchAutopilot leaves before downstream work.
func seedMessageAutopilot(t *testing.T, pool *pgxpool.Pool, workspaceID, agentID, creatorID string) (autopilotID, runID string) {
	t.Helper()
	ctx := context.Background()
	if err := pool.QueryRow(ctx, `
		INSERT INTO autopilot (
			workspace_id, title, description, assignee_type, assignee_id,
			status, execution_mode, created_by_type, created_by_id
		)
		VALUES ($1, 'Daily report', 'Write the daily report.', 'agent', $2, 'active', 'message', 'member', $3)
		RETURNING id`, workspaceID, agentID, creatorID).Scan(&autopilotID); err != nil {
		t.Fatalf("seed message autopilot: %v", err)
	}
	t.Cleanup(func() {
		pool.Exec(context.Background(), `DELETE FROM autopilot WHERE id = $1`, autopilotID)
	})
	if err := pool.QueryRow(ctx, `
		INSERT INTO autopilot_run (autopilot_id, source, status)
		VALUES ($1, 'manual', 'running') RETURNING id`, autopilotID).Scan(&runID); err != nil {
		t.Fatalf("seed message run: %v", err)
	}
	return autopilotID, runID
}

// seedLarkP2PSession creates the agent installation, the member's existing
// conversation, and the p2p binding that message mode must reuse.
func seedLarkP2PSession(t *testing.T, pool *pgxpool.Pool, workspaceID, agentID, memberID string) (installationID, sessionID string) {
	t.Helper()
	ctx := context.Background()
	if err := pool.QueryRow(ctx, `
		INSERT INTO channel_installation (
			workspace_id, agent_id, channel_type, config, status, installer_user_id
		)
		VALUES ($1, $2, 'feishu', '{}'::jsonb, 'active', $3)
		RETURNING id`, workspaceID, agentID, memberID).Scan(&installationID); err != nil {
		t.Fatalf("seed channel installation: %v", err)
	}
	t.Cleanup(func() {
		pool.Exec(context.Background(), `DELETE FROM channel_installation WHERE id = $1`, installationID)
	})
	if err := pool.QueryRow(ctx, `
		INSERT INTO chat_session (workspace_id, agent_id, creator_id, title)
		VALUES ($1, $2, $3, 'Daily report') RETURNING id`, workspaceID, agentID, memberID).Scan(&sessionID); err != nil {
		t.Fatalf("seed chat session: %v", err)
	}
	t.Cleanup(func() {
		pool.Exec(context.Background(), `DELETE FROM chat_session WHERE id = $1`, sessionID)
	})
	chatID := fmt.Sprintf("oc_dm_%d", time.Now().UnixNano())
	if _, err := pool.Exec(ctx, `
		INSERT INTO channel_chat_session_binding (
			chat_session_id, installation_id, channel_type, channel_chat_id, chat_type, config
		)
		VALUES ($1, $2, 'feishu', $3, 'p2p', '{}'::jsonb)`, sessionID, installationID, chatID); err != nil {
		t.Fatalf("seed p2p binding: %v", err)
	}
	t.Cleanup(func() {
		pool.Exec(context.Background(), `DELETE FROM channel_chat_session_binding WHERE chat_session_id = $1`, sessionID)
	})
	return installationID, sessionID
}

func newMessageAutopilotService(pool *pgxpool.Pool) *AutopilotService {
	q := db.New(pool)
	bus := events.New()
	return &AutopilotService{
		Queries:   q,
		TxStarter: pool,
		Bus:       bus,
		TaskSvc:   &TaskService{Queries: q, TxStarter: pool, Bus: bus},
	}
}

func TestDispatchMessageUsesBoundP2PChatSession(t *testing.T) {
	pool := sharedTestPool(t)
	ctx := context.Background()
	q := db.New(pool)
	workspaceID, userID, agentID, _ := seedAttributionFixture(t, pool)
	_, sessionID := seedLarkP2PSession(t, pool, workspaceID, agentID, userID)
	// A newer web chat for the same agent/member must not win: message mode
	// only reuses the Feishu p2p binding.
	var webSessionID string
	if err := pool.QueryRow(ctx, `
		INSERT INTO chat_session (workspace_id, agent_id, creator_id, title)
		VALUES ($1, $2, $3, 'Newer web chat') RETURNING id`,
		workspaceID, agentID, userID).Scan(&webSessionID); err != nil {
		t.Fatalf("seed newer web chat: %v", err)
	}
	autopilotID, runID := seedMessageAutopilot(t, pool, workspaceID, agentID, userID)

	var issuesBefore int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM issue WHERE workspace_id = $1`, workspaceID).Scan(&issuesBefore); err != nil {
		t.Fatalf("count issues before dispatch: %v", err)
	}

	svc := newMessageAutopilotService(pool)
	ap, err := q.GetAutopilot(ctx, util.MustParseUUID(autopilotID))
	if err != nil {
		t.Fatalf("load autopilot: %v", err)
	}
	run, err := q.GetAutopilotRun(ctx, util.MustParseUUID(runID))
	if err != nil {
		t.Fatalf("load run: %v", err)
	}
	if err := svc.dispatchMessage(ctx, ap, &run, pgtype.UUID{}); err != nil {
		t.Fatalf("dispatchMessage: %v", err)
	}

	task, err := q.GetAutopilotTaskByRun(ctx, run.ID)
	if err != nil {
		t.Fatalf("load dispatched task: %v", err)
	}
	if task.IssueID.Valid {
		t.Fatalf("message task must not create an issue, got %s", util.UUIDToString(task.IssueID))
	}
	if !task.ChatSessionID.Valid || task.ChatSessionID.Bytes != util.MustParseUUID(sessionID).Bytes {
		t.Fatalf("task chat_session_id = %s, want %s", util.UUIDToString(task.ChatSessionID), sessionID)
	}
	if task.ChatSessionID.Bytes == util.MustParseUUID(webSessionID).Bytes {
		t.Fatalf("task reused the newer web chat %s instead of the p2p binding", webSessionID)
	}
	if !task.ChatInputTaskID.Valid || task.ChatInputTaskID.Bytes != task.ID.Bytes {
		t.Fatalf("task must own its input batch, chat_input_task_id = %s task = %s",
			util.UUIDToString(task.ChatInputTaskID), util.UUIDToString(task.ID))
	}

	updatedRun, err := q.GetAutopilotRun(ctx, run.ID)
	if err != nil {
		t.Fatalf("reload run: %v", err)
	}
	if updatedRun.Status != "running" || !updatedRun.TaskID.Valid || updatedRun.TaskID.Bytes != task.ID.Bytes {
		t.Fatalf("run = status %q task %s, want running with task %s",
			updatedRun.Status, util.UUIDToString(updatedRun.TaskID), util.UUIDToString(task.ID))
	}

	var kickoffKind, kickoffContent string
	var kickoffTaskID pgtype.UUID
	if err := pool.QueryRow(ctx, `
		SELECT message_kind, content, task_id
		FROM chat_message
		WHERE chat_session_id = $1 AND role = 'user'`, util.MustParseUUID(sessionID)).Scan(&kickoffKind, &kickoffContent, &kickoffTaskID); err != nil {
		t.Fatalf("load kickoff: %v", err)
	}
	if kickoffKind != protocol.ChatMessageKindAutopilotKickoff {
		t.Fatalf("kickoff kind = %q, want %q", kickoffKind, protocol.ChatMessageKindAutopilotKickoff)
	}
	if kickoffTaskID.Bytes != task.ID.Bytes {
		t.Fatalf("kickoff task_id = %s, want %s", util.UUIDToString(kickoffTaskID), util.UUIDToString(task.ID))
	}
	if !strings.Contains(kickoffContent, "Write the daily report.") || !strings.Contains(kickoffContent, "Do not create a Multica issue") {
		t.Fatalf("kickoff must carry the autopilot instructions and issue guard: %q", kickoffContent)
	}

	var deliveryChannel, deliveryChatID string
	if err := pool.QueryRow(ctx, `
		SELECT channel_type, channel_chat_id
		FROM channel_task_delivery WHERE task_id = $1`, task.ID).Scan(&deliveryChannel, &deliveryChatID); err != nil {
		t.Fatalf("load task delivery: %v", err)
	}
	if deliveryChannel != "feishu" || !strings.HasPrefix(deliveryChatID, "oc_dm_") {
		t.Fatalf("delivery = %q/%q, want feishu p2p chat", deliveryChannel, deliveryChatID)
	}

	// The hidden kickoff is the agent's input but never a UI turn or preview.
	visible, err := q.ListChatMessages(ctx, util.MustParseUUID(sessionID))
	if err != nil {
		t.Fatalf("list visible messages: %v", err)
	}
	if len(visible) != 0 {
		t.Fatalf("visible chat messages = %d, want 0 (kickoff hidden)", len(visible))
	}
	input, err := q.ListChatInputMessages(ctx, task.ID)
	if err != nil {
		t.Fatalf("list task input: %v", err)
	}
	if len(input) != 1 || input[0].MessageKind != protocol.ChatMessageKindAutopilotKickoff {
		t.Fatalf("task input = %+v, want exactly the hidden kickoff", input)
	}
	sessions, err := q.ListChatSessionsByCreator(ctx, db.ListChatSessionsByCreatorParams{
		WorkspaceID: util.MustParseUUID(workspaceID),
		CreatorID:   util.MustParseUUID(userID),
	})
	if err != nil {
		t.Fatalf("list chat sessions: %v", err)
	}
	if len(sessions) != 0 {
		t.Fatalf("chat list = %d sessions, want 0 before the report is written", len(sessions))
	}

	var issuesAfter int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM issue WHERE workspace_id = $1`, workspaceID).Scan(&issuesAfter); err != nil {
		t.Fatalf("count issues after dispatch: %v", err)
	}
	if issuesAfter != issuesBefore {
		t.Fatalf("issue count changed from %d to %d; message mode must not create issues", issuesBefore, issuesAfter)
	}

	// Completing the task writes the assistant outcome and lets the run point at
	// the delivered message.
	if _, err := pool.Exec(ctx, `UPDATE agent_task_queue SET status = 'running', started_at = now() WHERE id = $1`, task.ID); err != nil {
		t.Fatalf("mark task running: %v", err)
	}
	result := []byte(`{"output":"Daily report body"}`)
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin outcome tx: %v", err)
	}
	msg, err := svc.TaskSvc.writeChatCompletionOutcome(ctx, q.WithTx(tx), task, result)
	if err != nil {
		tx.Rollback(ctx)
		t.Fatalf("write chat outcome: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit outcome tx: %v", err)
	}
	if msg == nil || msg.MessageKind != protocol.ChatMessageKindAgentReport {
		t.Fatalf("assistant message = %+v, want kind %q", msg, protocol.ChatMessageKindAgentReport)
	}
	visible, err = q.ListChatMessages(ctx, util.MustParseUUID(sessionID))
	if err != nil {
		t.Fatalf("list visible messages after report: %v", err)
	}
	if len(visible) != 1 || visible[0].ID.Bytes != msg.ID.Bytes {
		t.Fatalf("visible messages = %+v, want the agent report", visible)
	}

	completed := task
	completed.Status = "completed"
	completed.Result = result
	svc.SyncRunFromTask(ctx, completed)
	completedRun, err := q.GetAutopilotRun(ctx, run.ID)
	if err != nil {
		t.Fatalf("reload completed run: %v", err)
	}
	if completedRun.Status != "completed" {
		t.Fatalf("run status = %q, want completed", completedRun.Status)
	}
	var runResult map[string]string
	if err := json.Unmarshal(completedRun.Result, &runResult); err != nil {
		t.Fatalf("decode run result: %v", err)
	}
	if runResult["chat_session_id"] != sessionID || runResult["message_id"] != util.UUIDToString(msg.ID) {
		t.Fatalf("run result = %v, want chat_session_id/message_id", runResult)
	}
}

func TestDispatchMessageFallsBackToDeduplicatedInbox(t *testing.T) {
	pool := sharedTestPool(t)
	ctx := context.Background()
	q := db.New(pool)
	workspaceID, userID, agentID, _ := seedAttributionFixture(t, pool)
	autopilotID, runID := seedMessageAutopilot(t, pool, workspaceID, agentID, userID)
	dedupeKey := "autopilot_report:" + runID
	t.Cleanup(func() {
		pool.Exec(context.Background(), `DELETE FROM inbox_item WHERE workspace_id = $1 AND dedupe_key = $2`, workspaceID, dedupeKey)
	})

	svc := newMessageAutopilotService(pool)
	ap, err := q.GetAutopilot(ctx, util.MustParseUUID(autopilotID))
	if err != nil {
		t.Fatalf("load autopilot: %v", err)
	}
	run, err := q.GetAutopilotRun(ctx, util.MustParseUUID(runID))
	if err != nil {
		t.Fatalf("load run: %v", err)
	}
	if err := svc.dispatchMessage(ctx, ap, &run, pgtype.UUID{}); err != nil {
		t.Fatalf("dispatchMessage fallback: %v", err)
	}

	task, err := q.GetAutopilotTaskByRun(ctx, run.ID)
	if err != nil {
		t.Fatalf("load fallback task: %v", err)
	}
	if task.ChatSessionID.Valid {
		t.Fatalf("fallback task must not invent a chat session, got %s", util.UUIDToString(task.ChatSessionID))
	}
	if task.IssueID.Valid {
		t.Fatalf("fallback task must not create an issue, got %s", util.UUIDToString(task.IssueID))
	}

	result := []byte(`{"output":"Fallback report body"}`)
	completed := task
	completed.Status = "completed"
	completed.Result = result
	svc.SyncRunFromTask(ctx, completed)
	// A replayed completion must not create a second notification.
	svc.SyncRunFromTask(ctx, completed)

	var count int
	if err := pool.QueryRow(ctx, `
		SELECT count(*) FROM inbox_item
		WHERE workspace_id = $1 AND recipient_id = $2 AND type = 'agent_message'`,
		workspaceID, userID).Scan(&count); err != nil {
		t.Fatalf("count fallback inbox items: %v", err)
	}
	if count != 1 {
		t.Fatalf("fallback inbox items = %d, want 1", count)
	}
	var body, dedupe string
	var issueID pgtype.UUID
	if err := pool.QueryRow(ctx, `
		SELECT body, dedupe_key, issue_id FROM inbox_item
		WHERE workspace_id = $1 AND dedupe_key = $2`, workspaceID, dedupeKey).Scan(&body, &dedupe, &issueID); err != nil {
		t.Fatalf("load fallback inbox item: %v", err)
	}
	if !strings.Contains(body, "Fallback report body") {
		t.Fatalf("fallback body = %q, want task output", body)
	}
	if issueID.Valid {
		t.Fatalf("fallback inbox item must not link an issue, got %s", util.UUIDToString(issueID))
	}
	if dedupe != dedupeKey {
		t.Fatalf("dedupe key = %q, want %q", dedupe, dedupeKey)
	}

	completedRun, err := q.GetAutopilotRun(ctx, run.ID)
	if err != nil {
		t.Fatalf("reload fallback run: %v", err)
	}
	if completedRun.Status != "completed" {
		t.Fatalf("fallback run status = %q, want completed", completedRun.Status)
	}
	var runResult map[string]string
	if err := json.Unmarshal(completedRun.Result, &runResult); err != nil {
		t.Fatalf("decode fallback run result: %v", err)
	}
	if runResult["inbox_item_id"] == "" {
		t.Fatalf("fallback run result = %v, want inbox_item_id", runResult)
	}
}

func TestBuildMessageKickoffCarriesInstructions(t *testing.T) {
	svc := &AutopilotService{}
	ap := db.Autopilot{
		Title:       "Daily report",
		Description: pgtype.Text{String: "Write the daily report.", Valid: true},
	}
	run := db.AutopilotRun{
		Source:      "schedule",
		TriggeredAt: pgtype.Timestamptz{Time: time.Date(2026, 5, 26, 0, 0, 0, 0, time.UTC), Valid: true},
	}
	got := svc.buildMessageKickoff(ap, run, "Asia/Tokyo")
	for _, want := range []string{"Daily report", "Write the daily report.", "2026-05-26 09:00 Asia/Tokyo", "Do not create a Multica issue"} {
		if !strings.Contains(got, want) {
			t.Fatalf("kickoff missing %q:\n%s", want, got)
		}
	}
}

func TestFallbackDeliveryFailureMarksRunFailed(t *testing.T) {
	pool := sharedTestPool(t)
	ctx := context.Background()
	q := db.New(pool)
	workspaceID, userID, agentID, _ := seedAttributionFixture(t, pool)
	autopilotID, runID := seedMessageAutopilot(t, pool, workspaceID, agentID, userID)

	svc := newMessageAutopilotService(pool)
	ap, err := q.GetAutopilot(ctx, util.MustParseUUID(autopilotID))
	if err != nil {
		t.Fatalf("load autopilot: %v", err)
	}
	run, err := q.GetAutopilotRun(ctx, util.MustParseUUID(runID))
	if err != nil {
		t.Fatalf("load run: %v", err)
	}
	if err := svc.dispatchMessage(ctx, ap, &run, pgtype.UUID{}); err != nil {
		t.Fatalf("dispatchMessage fallback: %v", err)
	}
	task, err := q.GetAutopilotTaskByRun(ctx, run.ID)
	if err != nil {
		t.Fatalf("load fallback task: %v", err)
	}

	// Remove the recipient so the fallback has nowhere to deliver. The run
	// must fail visibly instead of completing without a report.
	if _, err := pool.Exec(ctx, `DELETE FROM member WHERE workspace_id = $1 AND user_id = $2`, workspaceID, userID); err != nil {
		t.Fatalf("remove fallback recipient: %v", err)
	}
	completed := task
	completed.Status = "completed"
	completed.Result = []byte(`{"output":"undeliverable report"}`)
	svc.SyncRunFromTask(ctx, completed)

	updated, err := q.GetAutopilotRun(ctx, run.ID)
	if err != nil {
		t.Fatalf("reload failed run: %v", err)
	}
	if updated.Status != "failed" {
		t.Fatalf("run status = %q, want failed", updated.Status)
	}
	if !updated.FailureReason.Valid || !strings.Contains(updated.FailureReason.String, "message delivery failed") {
		t.Fatalf("failure_reason = %q, want message delivery failure", updated.FailureReason.String)
	}
}

func TestSettleQueuedAutopilotKickoffDoesNotRestoreDraft(t *testing.T) {
	pool := sharedTestPool(t)
	ctx := context.Background()
	q := db.New(pool)
	workspaceID, userID, agentID, _ := seedAttributionFixture(t, pool)

	var sessionID string
	if err := pool.QueryRow(ctx, `
		INSERT INTO chat_session (workspace_id, agent_id, creator_id, title)
		VALUES ($1, $2, $3, 'Autopilot queue') RETURNING id`,
		workspaceID, agentID, userID).Scan(&sessionID); err != nil {
		t.Fatalf("seed chat session: %v", err)
	}
	t.Cleanup(func() {
		pool.Exec(context.Background(), `DELETE FROM chat_draft_restore WHERE chat_session_id = $1`, sessionID)
		pool.Exec(context.Background(), `DELETE FROM chat_message WHERE chat_session_id = $1`, sessionID)
		pool.Exec(context.Background(), `DELETE FROM agent_task_queue WHERE chat_session_id = $1`, sessionID)
		pool.Exec(context.Background(), `DELETE FROM chat_session WHERE id = $1`, sessionID)
	})

	var taskID string
	if err := pool.QueryRow(ctx, `
		INSERT INTO agent_task_queue (agent_id, runtime_id, status, priority, chat_session_id)
		SELECT $1, runtime_id, 'queued', 0, $2 FROM agent WHERE id = $1
		RETURNING id`, agentID, sessionID).Scan(&taskID); err != nil {
		t.Fatalf("seed queued task: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE agent_task_queue SET chat_input_task_id = id WHERE id = $1`, taskID); err != nil {
		t.Fatalf("stamp task input owner: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO chat_message (chat_session_id, role, content, task_id, message_kind)
		VALUES ($1, 'user', 'INTERNAL AUTOPILOT PROMPT', $2, 'autopilot_kickoff')`,
		sessionID, taskID); err != nil {
		t.Fatalf("seed kickoff: %v", err)
	}

	task, err := q.GetAgentTask(ctx, util.MustParseUUID(taskID))
	if err != nil {
		t.Fatalf("load queued task: %v", err)
	}
	svc := newMessageAutopilotService(pool)
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin settle tx: %v", err)
	}
	if _, err := svc.TaskSvc.settleQueuedChatInput(ctx, q.WithTx(tx), task, "edit"); err != nil {
		tx.Rollback(ctx)
		t.Fatalf("settle queued kickoff: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit settle tx: %v", err)
	}

	var kickoffs, restores int
	if err := pool.QueryRow(ctx, `
		SELECT count(*) FROM chat_message
		WHERE task_id = $1 AND message_kind = 'autopilot_kickoff'`, taskID).Scan(&kickoffs); err != nil {
		t.Fatalf("count kickoff: %v", err)
	}
	if err := pool.QueryRow(ctx, `
		SELECT count(*) FROM chat_draft_restore WHERE chat_session_id = $1`, sessionID).Scan(&restores); err != nil {
		t.Fatalf("count draft restores: %v", err)
	}
	if kickoffs != 1 {
		t.Fatalf("kickoff rows = %d, want 1 (internal prompt must stay hidden)", kickoffs)
	}
	if restores != 0 {
		t.Fatalf("draft restores = %d, want 0 (kickoff must not become a member draft)", restores)
	}
}
