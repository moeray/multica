package service

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/dbid"
)

// TestIssueWakeupReviewMergesIntoMentionTask is the MP-66 regression at the
// wakeup boundary: the delivery mention already queued the reviewer, then the
// in_review condition wakeup fires. The wakeup must absorb its inputs into that
// run instead of creating a second review task.
func TestIssueWakeupReviewMergesIntoMentionTask(t *testing.T) {
	f, s, issueID, reviewer := wakeFixture(t)
	ctx := context.Background()
	developer := f.privateAgentOwnedBy(t, f.UserID, "wakeup-review-dev")
	f.Exec(t, `UPDATE issue SET status = 'in_review', assignee_type = 'agent', assignee_id = $2 WHERE id = $1`, issueID, developer)
	issue, err := f.q.GetIssue(ctx, issueID)
	if err != nil {
		t.Fatalf("load issue: %v", err)
	}
	reviewerID := parseTestUUID(t, reviewer)
	key, ok := ReviewTaskKey(issue, reviewerID)
	if !ok {
		t.Fatal("expected a review key for the wakeup target")
	}
	mention, err := f.svc.TaskSvc.EnqueueMentionTaskForReview(ctx, issue, reviewerID, pgtype.UUID{}, OriginNamed, key)
	if err != nil {
		t.Fatalf("enqueue mention review: %v", err)
	}
	// MP-66 shape: the mention run was already claimed and running when the
	// status wakeup fired. A queued mention instead joins at claim time through
	// the existing JoinWaitingWakeups path.
	f.Exec(t, `UPDATE agent_task_queue SET status = 'running', started_at = now() WHERE id = $1`, mention.ID)

	w := wakeCreate(t, f, s, issueID, WakeupInput{
		AgentID:     reviewer,
		Kind:        "event",
		Mode:        "once",
		EventTypes:  []string{"issue.status_changed"},
		Instruction: "Review the handoff",
	})
	payload, _ := json.Marshal(map[string]any{"status": "in_review"})
	receipt, err := f.q.RecordWakeupReceipt(ctx, db.RecordWakeupReceiptParams{
		ID:        dbid.NewV7(),
		WakeupID:  w.ID,
		Revision:  w.Revision,
		EventKey:  "status:in_review",
		EventType: "issue.status_changed",
		Payload:   payload,
	})
	if err != nil {
		t.Fatalf("record wakeup receipt: %v", err)
	}

	wakeDispatch(t, s, w)

	if n := f.Count(t, `SELECT count(*) FROM agent_task_queue WHERE issue_id = $1 AND dedupe_key = $2`, issueID, key); n != 1 {
		t.Fatalf("tasks holding the review key = %d, want 1", n)
	}
	if n := f.Count(t, `SELECT count(*) FROM agent_task_queue WHERE issue_id = $1 AND agent_id = $2 AND status IN ('queued','dispatched','running','waiting_local_directory')`, issueID, reviewerID); n != 1 {
		t.Fatalf("active review tasks = %d, want 1", n)
	}
	got, err := f.q.GetIssueWakeup(ctx, db.GetIssueWakeupParams{ID: w.ID, WorkspaceID: w.WorkspaceID})
	if err != nil {
		t.Fatalf("reload wakeup: %v", err)
	}
	if !got.LastTaskID.Valid || got.LastTaskID != mention.ID {
		t.Fatalf("wakeup last task = %v, want mention task %v", got.LastTaskID, mention.ID)
	}
	var receiptTask pgtype.UUID
	if err := f.Pool.QueryRow(ctx, `SELECT task_id FROM issue_wakeup_receipt WHERE id = $1`, receipt.ID).Scan(&receiptTask); err != nil {
		t.Fatalf("load receipt: %v", err)
	}
	if receiptTask != mention.ID {
		t.Fatalf("receipt task = %v, want the merged mention task %v", receiptTask, mention.ID)
	}
}
