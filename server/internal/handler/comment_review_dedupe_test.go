package handler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/service"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/dbid"
)

// TestCreateCommentReviewMentionFoldsIntoRunningRound covers the mention half
// of MP-66: while the reviewer run for a round is active, a second delivery
// comment mentioning the same reviewer is registered as a planned follow-up
// instead of starting a second run.
func TestCreateCommentReviewMentionFoldsIntoRunningRound(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	ctx := context.Background()
	developerID := createHandlerTestAgent(t, "Review Dedupe Developer", nil)
	reviewerID := createHandlerTestAgent(t, "Review Dedupe Reviewer", nil)
	issueID := createCommentTriggerPreviewIssue(t, "review dedupe", "agent", developerID)
	if _, err := testPool.Exec(ctx, `UPDATE issue SET status = 'in_review' WHERE id = $1`, issueID); err != nil {
		t.Fatalf("move issue to review: %v", err)
	}

	content := fmt.Sprintf("[@Reviewer](mention://agent/%s) please review", reviewerID)
	postCommentForTriggerPreviewTest(t, issueID, map[string]any{"content": content})

	var taskID, dedupeKey string
	if err := testPool.QueryRow(ctx, `
		SELECT id, COALESCE(dedupe_key, '')
		FROM agent_task_queue
		WHERE issue_id = $1 AND agent_id = $2 AND status = 'queued'
	`, issueID, reviewerID).Scan(&taskID, &dedupeKey); err != nil {
		t.Fatalf("load review task: %v", err)
	}
	if !strings.HasPrefix(dedupeKey, "review:") {
		t.Fatalf("review task dedupe_key = %q, want review: prefix", dedupeKey)
	}
	if _, err := testPool.Exec(ctx, `
		UPDATE agent_task_queue SET status = 'running', started_at = now() WHERE id = $1
	`, taskID); err != nil {
		t.Fatalf("start review task: %v", err)
	}

	w := httptest.NewRecorder()
	r := newRequest(http.MethodPost, "/api/issues/"+issueID+"/comments", map[string]any{"content": content + " with one more note"})
	r = withURLParam(r, "id", issueID)
	testHandler.CreateComment(w, r)
	if w.Code != http.StatusCreated {
		t.Fatalf("second comment: expected 201, got %d: %s", w.Code, w.Body.String())
	}
	var resp CommentResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode second comment: %v", err)
	}
	if len(resp.TriggerOutcomes) != 1 {
		t.Fatalf("trigger outcomes = %d, want 1", len(resp.TriggerOutcomes))
	}
	if got := resp.TriggerOutcomes[0]; got.Status != DispatchDeferred || got.ReasonCode != ReasonDeferred {
		t.Fatalf("second mention outcome = %s/%s, want deferred/deferred", got.Status, got.ReasonCode)
	}

	var taskCount int
	if err := testPool.QueryRow(ctx, `
		SELECT count(*) FROM agent_task_queue
		WHERE issue_id = $1 AND agent_id = $2
		  AND status IN ('queued','deferred','dispatched','running','waiting_local_directory')
	`, issueID, reviewerID).Scan(&taskCount); err != nil {
		t.Fatalf("count review tasks: %v", err)
	}
	if taskCount != 1 {
		t.Fatalf("review tasks = %d, want 1", taskCount)
	}
	var folded int
	if err := testPool.QueryRow(ctx, `
		SELECT count(*) FROM agent_task_queue
		WHERE id = $1 AND $2::uuid = ANY(coalesced_comment_ids)
	`, taskID, resp.ID).Scan(&folded); err != nil {
		t.Fatalf("check folded comment: %v", err)
	}
	if folded != 1 {
		t.Fatal("second comment was not registered as a planned follow-up on the running review")
	}
}

// TestCreateCommentReviewMentionMergesIntoWakeupTask covers the reverse of
// MP-66: the status wakeup queued the reviewer first, then the delivery mention
// arrives. The mention must fold into that wakeup-created review task, and a
// later mention must be registered as a planned input once it is running.
func TestCreateCommentReviewMentionMergesIntoWakeupTask(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	ctx := context.Background()
	developerID := createHandlerTestAgent(t, "Wakeup Review Developer", nil)
	reviewerID := createHandlerTestAgent(t, "Wakeup Review Reviewer", nil)
	issueID := createCommentTriggerPreviewIssue(t, "wakeup review dedupe", "agent", developerID)
	if _, err := testPool.Exec(ctx, `UPDATE issue SET status = 'in_review' WHERE id = $1`, issueID); err != nil {
		t.Fatalf("move issue to review: %v", err)
	}
	issue, err := testHandler.Queries.GetIssue(ctx, parseUUID(issueID))
	if err != nil {
		t.Fatalf("load issue: %v", err)
	}
	reviewerUUID := parseUUID(reviewerID)
	reviewKey, ok := service.ReviewTaskKey(issue, reviewerUUID)
	if !ok {
		t.Fatal("expected a review key")
	}
	wakeupTask, err := testHandler.Queries.CreateWakeupTask(ctx, db.CreateWakeupTaskParams{
		ID:                dbid.NewV7(),
		AgentID:           reviewerUUID,
		RuntimeID:         parseUUID(handlerTestRuntimeID(t)),
		IssueID:           issue.ID,
		Priority:          0,
		WakeupContext:     []byte(fmt.Sprintf(`{"wakeup_id":%q}`, uuidToString(dbid.NewV7()))),
		OriginatorUserID:  parseUUID(testUserID),
		AccountableUserID: parseUUID(testUserID),
		DedupeKey:         pgtype.Text{String: reviewKey, Valid: true},
	})
	if err != nil {
		t.Fatalf("create wakeup review task: %v", err)
	}

	content := fmt.Sprintf("[@Reviewer](mention://agent/%s) please review", reviewerID)
	first := postCommentForTriggerPreviewTest(t, issueID, map[string]any{"content": content})
	var folded int
	if err := testPool.QueryRow(ctx, `
		SELECT count(*) FROM agent_task_queue
		WHERE id = $1
		  AND (trigger_comment_id = $2 OR $2::uuid = ANY(coalesced_comment_ids))
	`, wakeupTask.ID, first).Scan(&folded); err != nil {
		t.Fatalf("check first folded comment: %v", err)
	}
	if folded != 1 {
		t.Fatal("mention was not folded into the queued wakeup review task")
	}
	if _, err := testPool.Exec(ctx, `
		UPDATE agent_task_queue SET status = 'running', started_at = now() WHERE id = $1
	`, wakeupTask.ID); err != nil {
		t.Fatalf("start wakeup review task: %v", err)
	}

	w := httptest.NewRecorder()
	r := newRequest(http.MethodPost, "/api/issues/"+issueID+"/comments", map[string]any{"content": content + " one more note"})
	r = withURLParam(r, "id", issueID)
	testHandler.CreateComment(w, r)
	if w.Code != http.StatusCreated {
		t.Fatalf("second comment: expected 201, got %d: %s", w.Code, w.Body.String())
	}
	var resp CommentResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode second comment: %v", err)
	}
	if len(resp.TriggerOutcomes) != 1 || resp.TriggerOutcomes[0].Status != DispatchDeferred || resp.TriggerOutcomes[0].ReasonCode != ReasonDeferred {
		t.Fatalf("running wakeup mention outcome = %+v, want deferred/deferred", resp.TriggerOutcomes)
	}
	if err := testPool.QueryRow(ctx, `
		SELECT count(*) FROM agent_task_queue
		WHERE id = $1 AND $2::uuid = ANY(coalesced_comment_ids)
	`, wakeupTask.ID, resp.ID).Scan(&folded); err != nil {
		t.Fatalf("check planned comment: %v", err)
	}
	if folded != 1 {
		t.Fatal("second mention was not registered as planned input on the running wakeup review")
	}
	var taskCount int
	if err := testPool.QueryRow(ctx, `
		SELECT count(*) FROM agent_task_queue
		WHERE issue_id = $1 AND agent_id = $2
		  AND status IN ('queued','deferred','dispatched','running','waiting_local_directory')
	`, issueID, reviewerID).Scan(&taskCount); err != nil {
		t.Fatalf("count review tasks: %v", err)
	}
	if taskCount != 1 {
		t.Fatalf("review tasks = %d, want 1", taskCount)
	}
}

type failingTxStarter struct{ err error }

func (f failingTxStarter) Begin(context.Context) (pgx.Tx, error) { return nil, f.err }

// TestCreateCommentReviewFailsClosedWhenSupersedeFails proves a new generation
// is not enqueued when the old one could not be retired: the unique index only
// covers one key, so continuing would allow two active review tasks.
func TestCreateCommentReviewFailsClosedWhenSupersedeFails(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	ctx := context.Background()
	developerID := createHandlerTestAgent(t, "Supersede Failure Developer", nil)
	reviewerID := createHandlerTestAgent(t, "Supersede Failure Reviewer", nil)
	issueID := createCommentTriggerPreviewIssue(t, "supersede failure", "agent", developerID)
	if _, err := testPool.Exec(ctx, `UPDATE issue SET status = 'in_review' WHERE id = $1`, issueID); err != nil {
		t.Fatalf("move issue to review: %v", err)
	}

	h := *testHandler
	svc := *testHandler.TaskService
	svc.TxStarter = failingTxStarter{err: errors.New("injected supersede failure")}
	h.TaskService = &svc

	w := httptest.NewRecorder()
	r := newRequest(http.MethodPost, "/api/issues/"+issueID+"/comments", map[string]any{
		"content": fmt.Sprintf("[@Reviewer](mention://agent/%s) please review", reviewerID),
	})
	r = withURLParam(r, "id", issueID)
	h.CreateComment(w, r)
	if w.Code != http.StatusCreated {
		t.Fatalf("comment: expected 201 (comment persists), got %d: %s", w.Code, w.Body.String())
	}
	var resp CommentResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode comment: %v", err)
	}
	if len(resp.TriggerOutcomes) != 1 || resp.TriggerOutcomes[0].Status != DispatchBlocked || resp.TriggerOutcomes[0].ReasonCode != ReasonInternalError {
		t.Fatalf("trigger outcomes = %+v, want blocked/internal_error", resp.TriggerOutcomes)
	}
	var taskCount int
	if err := testPool.QueryRow(ctx, `
		SELECT count(*) FROM agent_task_queue WHERE issue_id = $1 AND agent_id = $2
	`, issueID, reviewerID).Scan(&taskCount); err != nil {
		t.Fatalf("count review tasks: %v", err)
	}
	if taskCount != 0 {
		t.Fatalf("review tasks after failed supersede = %d, want 0", taskCount)
	}
}

// TestCreateCommentReviewMentionMergesIntoDeferredRetry covers the
// runtime_offline auto-retry: the deferred child holds the round key, so the
// delivery mention folds into it instead of creating a second review. Promotion
// then keeps the same row and a later mention still coalesces.
func TestCreateCommentReviewMentionMergesIntoDeferredRetry(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	ctx := context.Background()
	developerID := createHandlerTestAgent(t, "Deferred Review Developer", nil)
	reviewerID := createHandlerTestAgent(t, "Deferred Review Reviewer", nil)
	issueID := createCommentTriggerPreviewIssue(t, "deferred review", "agent", developerID)
	if _, err := testPool.Exec(ctx, `UPDATE issue SET status = 'in_review' WHERE id = $1`, issueID); err != nil {
		t.Fatalf("move issue to review: %v", err)
	}
	issue, err := testHandler.Queries.GetIssue(ctx, parseUUID(issueID))
	if err != nil {
		t.Fatalf("load issue: %v", err)
	}
	reviewerUUID := parseUUID(reviewerID)
	first, err := testHandler.TaskService.RequestReview(ctx, issue, reviewerUUID, func(ctx context.Context, key string) (db.AgentTaskQueue, error) {
		return testHandler.TaskService.EnqueueMentionTaskForReview(ctx, issue, reviewerUUID, pgtype.UUID{}, service.OriginNamed, key)
	})
	if err != nil {
		t.Fatalf("initial review: %v", err)
	}
	if _, err := testPool.Exec(ctx, `
		UPDATE agent_task_queue SET status = 'failed', completed_at = now(), failure_reason = 'runtime_offline'
		WHERE id = $1
	`, first.Task.ID); err != nil {
		t.Fatalf("fail review: %v", err)
	}
	retry, err := testHandler.Queries.CreateRetryTask(ctx, db.CreateRetryTaskParams{
		ID:        first.Task.ID,
		NewTaskID: dbid.NewV7(),
		FireAt:    pgtype.Timestamptz{Time: time.Now().Add(time.Hour), Valid: true},
	})
	if err != nil {
		t.Fatalf("create deferred retry: %v", err)
	}

	content := fmt.Sprintf("[@Reviewer](mention://agent/%s) please review", reviewerID)
	commentID := postCommentForTriggerPreviewTest(t, issueID, map[string]any{"content": content})
	var folded int
	if err := testPool.QueryRow(ctx, `
		SELECT count(*) FROM agent_task_queue
		WHERE id = $1
		  AND (trigger_comment_id = $2 OR $2::uuid = ANY(coalesced_comment_ids))
	`, retry.ID, commentID).Scan(&folded); err != nil {
		t.Fatalf("check deferred merge: %v", err)
	}
	if folded != 1 {
		t.Fatal("mention was not folded into the deferred review retry")
	}
	var taskCount int
	if err := testPool.QueryRow(ctx, `
		SELECT count(*) FROM agent_task_queue
		WHERE issue_id = $1 AND agent_id = $2
		  AND status IN ('queued','deferred','dispatched','running','waiting_local_directory')
	`, issueID, reviewerID).Scan(&taskCount); err != nil {
		t.Fatalf("count review tasks: %v", err)
	}
	if taskCount != 1 {
		t.Fatalf("review tasks = %d, want only the deferred retry", taskCount)
	}

	promoted, err := testHandler.Queries.PromoteDeferredChannelIssueTask(ctx, retry.ID)
	if err != nil {
		t.Fatalf("promote deferred retry: %v", err)
	}
	if promoted.Status != "queued" {
		t.Fatalf("promoted retry status = %q, want queued", promoted.Status)
	}
	postCommentForTriggerPreviewTest(t, issueID, map[string]any{"content": content + " after promotion"})
	if err := testPool.QueryRow(ctx, `
		SELECT count(*) FROM agent_task_queue
		WHERE issue_id = $1 AND agent_id = $2
		  AND status IN ('queued','deferred','dispatched','running','waiting_local_directory')
	`, issueID, reviewerID).Scan(&taskCount); err != nil {
		t.Fatalf("count post-promotion review tasks: %v", err)
	}
	if taskCount != 1 {
		t.Fatalf("post-promotion review tasks = %d, want 1", taskCount)
	}
}
