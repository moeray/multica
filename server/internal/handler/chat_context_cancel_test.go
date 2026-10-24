package handler

// chat_context_cancel_test.go — MP-73. A turn's mirrored rows are stamped with
// its context_task_id at enqueue and read when the task is claimed. A turn the
// user cancels BEFORE that claim never reads them, so a stamp left behind hides
// those rows from every later turn: the review result MP-68 exists to surface
// is never injected again. Cancellation therefore hands the stamp back to the
// un-injected pool — but only for a turn that never ran. A claimed turn already
// built its prompt from those rows; dropping its stamp would inject them twice.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

// insertStampedMirror writes the server-authored review_result row a chat
// receives when an agent reviews one of its issues, stamped to the turn the
// enqueue path just handed it to. created_at is backdated so the row predates
// that turn and every later one, exactly as a mirror written before the turn
// started would.
func insertStampedMirror(t *testing.T, sessionID, taskID string) string {
	t.Helper()
	var messageID string
	if err := testPool.QueryRow(context.Background(), `
		INSERT INTO chat_message (
			chat_session_id, role, content, message_kind, context_task_id, created_at
		)
		VALUES ($1, 'assistant', $2, $3, $4, now() - interval '1 minute')
		RETURNING id
	`,
		sessionID,
		protocol.BuildReviewResultContent(true, "通过。", "MP-67"),
		protocol.ChatMessageKindReviewResult,
		taskID,
	).Scan(&messageID); err != nil {
		t.Fatalf("insert stamped mirror: %v", err)
	}
	return messageID
}

// contextTaskIDOfMessage reads a message's injection stamp; nil means the row is
// back in the un-injected pool.
func contextTaskIDOfMessage(t *testing.T, messageID string) *string {
	t.Helper()
	var stamp *string
	if err := testPool.QueryRow(context.Background(),
		`SELECT context_task_id::text FROM chat_message WHERE id = $1`, messageID,
	).Scan(&stamp); err != nil {
		t.Fatalf("read context_task_id: %v", err)
	}
	return stamp
}

// nextChatTurn reproduces what the member's next message does to the copied
// context: enqueue a turn and let it seal every un-injected mirror that arrived
// before it. It returns that turn's id and the mirrored rows it would be
// dispatched with.
func nextChatTurn(t *testing.T, agentID, sessionID string) (string, []string) {
	t.Helper()
	taskID := insertPendingChatTask(t, agentID, sessionID, "queued")
	queries := db.New(testPool)
	if err := queries.LinkPendingAgentChatMessagesToTask(context.Background(), db.LinkPendingAgentChatMessagesToTaskParams{
		TaskID:        util.MustParseUUID(taskID),
		ChatSessionID: util.MustParseUUID(sessionID),
	}); err != nil {
		t.Fatalf("link pending agent messages to the next turn: %v", err)
	}
	rows, err := queries.ListChatContextMessagesForTask(context.Background(), db.ListChatContextMessagesForTaskParams{
		ChatSessionID: util.MustParseUUID(sessionID),
		ContextTaskID: util.MustParseUUID(taskID),
	})
	if err != nil {
		t.Fatalf("list the next turn's injected context: %v", err)
	}
	ids := make([]string, 0, len(rows))
	for _, row := range rows {
		ids = append(ids, util.UUIDToString(row.ID))
	}
	return taskID, ids
}

// TestCancelQueuedChatTurnReleasesItsInjectedMirror is acceptance 1: a turn
// cancelled while still queued never read the mirror stamped to it, so the
// stamp must go back to NULL and the member's next message must still carry it.
func TestCancelQueuedChatTurnReleasesItsInjectedMirror(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}

	agentID := createHandlerTestAgent(t, "CancelQueuedContextAgent", []byte("[]"))
	sessionID := createHandlerTestChatSession(t, agentID)
	taskID := insertPendingChatTask(t, agentID, sessionID, "queued")
	mirrorID := insertStampedMirror(t, sessionID, taskID)

	w := httptest.NewRecorder()
	testHandler.CancelTaskByUser(w, cancelTaskByUserRequest(t, testUserID, taskID))
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if got := taskStatus(t, taskID); got != "cancelled" {
		t.Fatalf("queued turn status = %q, want cancelled", got)
	}

	if got := contextTaskIDOfMessage(t, mirrorID); got != nil {
		t.Fatalf(
			"mirror still stamped to the cancelled turn (%s) — it will never be injected again, so the member never sees this review result",
			*got,
		)
	}

	nextTaskID, injected := nextChatTurn(t, agentID, sessionID)
	if len(injected) != 1 || injected[0] != mirrorID {
		t.Fatalf("next turn %s would be dispatched with %d mirrored row(s) %v, want exactly the released mirror %s",
			nextTaskID, len(injected), injected, mirrorID)
	}
}

// TestCancelRunningChatTurnKeepsItsInjectedMirror is acceptance 2: a turn
// cancelled after it was claimed already read the mirror, so the stamp must
// stay put — releasing it would inject the same row into the next turn a second
// time.
func TestCancelRunningChatTurnKeepsItsInjectedMirror(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}

	agentID := createHandlerTestAgent(t, "CancelRunningContextAgent", []byte("[]"))
	sessionID := createHandlerTestChatSession(t, agentID)
	taskID := insertPendingChatTask(t, agentID, sessionID, "running")
	// The claim is what makes the mirror READ: dispatched_at and started_at are
	// set by ClaimAgentTask / StartAgentTask, never by enqueue.
	if _, err := testPool.Exec(context.Background(), `
		UPDATE agent_task_queue
		SET dispatched_at = now() - interval '1 minute', started_at = now() - interval '1 minute'
		WHERE id = $1
	`, taskID); err != nil {
		t.Fatalf("mark task claimed: %v", err)
	}
	mirrorID := insertStampedMirror(t, sessionID, taskID)

	w := httptest.NewRecorder()
	testHandler.CancelTaskByUser(w, cancelTaskByUserRequest(t, testUserID, taskID))
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if got := taskStatus(t, taskID); got != "cancelled" {
		t.Fatalf("running turn status = %q, want cancelled", got)
	}

	if got := contextTaskIDOfMessage(t, mirrorID); got == nil || *got != taskID {
		t.Fatalf("claimed turn's stamp = %v, want it kept on %s — the run already read this mirror, releasing it would inject it twice",
			got, taskID)
	}

	nextTaskID, injected := nextChatTurn(t, agentID, sessionID)
	if len(injected) != 0 {
		t.Fatalf("next turn %s would re-inject %v, already read by the cancelled run", nextTaskID, injected)
	}
}

// TestQueuedEditReleasesItsInjectedMirror is acceptance 3: editing a queued
// turn cancels it and the member re-sends. The released mirror must land on the
// replacement turn exactly once — not still on the dead one, not twice.
func TestQueuedEditReleasesItsInjectedMirror(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}

	agentID := createHandlerTestAgent(t, "QueuedEditContextAgent", []byte("[]"))
	sessionID := createHandlerTestChatSession(t, agentID)
	taskID, _, _ := insertQueuedChatInputWithAttachment(t, agentID, sessionID, "rewrite this prompt")
	mirrorID := insertStampedMirror(t, sessionID, taskID)

	w := httptest.NewRecorder()
	testHandler.CancelTaskByUser(w, cancelQueuedTaskByUserRequest(t, testUserID, taskID, sessionID, "edit"))
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	if got := contextTaskIDOfMessage(t, mirrorID); got != nil {
		t.Fatalf("mirror still stamped to the edited-away turn (%s), want released", *got)
	}

	nextTaskID, injected := nextChatTurn(t, agentID, sessionID)
	if len(injected) != 1 || injected[0] != mirrorID {
		t.Fatalf("replacement turn %s would be dispatched with %v, want exactly the released mirror %s",
			nextTaskID, injected, mirrorID)
	}

	stale, err := db.New(testPool).ListChatContextMessagesForTask(context.Background(), db.ListChatContextMessagesForTaskParams{
		ChatSessionID: util.MustParseUUID(sessionID),
		ContextTaskID: util.MustParseUUID(taskID),
	})
	if err != nil {
		t.Fatalf("list the edited-away turn's injected context: %v", err)
	}
	if len(stale) != 0 {
		t.Fatalf("edited-away turn still owns %d injected row(s)", len(stale))
	}
}

// TestClearQueuedChatTasksReleasesItsInjectedMirror covers the other queued
// cancel: clearing the queue cancels every follow-up without restoring drafts,
// so each one has to hand its stamp back too. The positional head is not
// cancelled by that path and must keep its own stamp.
func TestClearQueuedChatTasksReleasesItsInjectedMirror(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}

	agentID := createHandlerTestAgent(t, "ClearQueueContextAgent", []byte("[]"))
	sessionID := createHandlerTestChatSession(t, agentID)
	headTaskID := insertPendingChatTask(t, agentID, sessionID, "queued")
	followUpTaskID := insertPendingChatTask(t, agentID, sessionID, "queued")
	if _, err := testPool.Exec(context.Background(), `
		UPDATE agent_task_queue
		SET created_at = now() - interval '2 minutes'
		WHERE id = $1
	`, headTaskID); err != nil {
		t.Fatalf("order queued tasks: %v", err)
	}
	headMirrorID := insertStampedMirror(t, sessionID, headTaskID)
	followUpMirrorID := insertStampedMirror(t, sessionID, followUpTaskID)

	req := withURLParam(
		newRequestAs(
			testUserID,
			http.MethodDelete,
			"/api/chat/sessions/"+sessionID+"/queued-tasks",
			nil,
		),
		"sessionId",
		sessionID,
	)
	w := httptest.NewRecorder()
	testHandler.ClearQueuedChatTasks(w, chatPendingCtxAs(t, req, testUserID))
	if w.Code != http.StatusNoContent {
		t.Fatalf("expected 204, got %d: %s", w.Code, w.Body.String())
	}

	if got := taskStatus(t, followUpTaskID); got != "cancelled" {
		t.Fatalf("queued follow-up status = %q, want cancelled", got)
	}
	if got := contextTaskIDOfMessage(t, followUpMirrorID); got != nil {
		t.Fatalf("follow-up mirror still stamped to the cleared turn (%s), want released", *got)
	}
	if got := taskStatus(t, headTaskID); got != "queued" {
		t.Fatalf("positional head status = %q, want queued", got)
	}
	if got := contextTaskIDOfMessage(t, headMirrorID); got == nil || *got != headTaskID {
		t.Fatalf("head mirror stamp = %v, want it kept on the untouched head %s", got, headTaskID)
	}
}
