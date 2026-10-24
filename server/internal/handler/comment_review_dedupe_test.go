package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
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
		SELECT count(*) FROM agent_task_queue WHERE issue_id = $1 AND agent_id = $2
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
