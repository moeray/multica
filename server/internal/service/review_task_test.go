package service

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/testutil"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/dbid"
)

type reviewRoundFixture struct {
	f          principalFixture
	developer  pgtype.UUID
	reviewerA  pgtype.UUID
	reviewerB  pgtype.UUID
	issue      db.Issue
	reviewerAA db.Agent
}

func newReviewRoundFixture(t *testing.T) reviewRoundFixture {
	t.Helper()
	f, owner := newPrincipalFixture(t)
	developerID := f.privateAgentOwnedBy(t, owner, "review-dev")
	reviewerAID := f.privateAgentOwnedBy(t, owner, "review-a")
	reviewerBID := f.privateAgentOwnedBy(t, owner, "review-b")
	issueID := f.Issue(t, "review round", testutil.Cols{
		"status":        "in_review",
		"assignee_type": "agent",
		"assignee_id":   developerID,
	})
	issue, err := f.q.GetIssue(context.Background(), parseTestUUID(t, issueID))
	if err != nil {
		t.Fatalf("load issue: %v", err)
	}
	reviewerA, err := f.q.GetAgent(context.Background(), parseTestUUID(t, reviewerAID))
	if err != nil {
		t.Fatalf("load reviewer: %v", err)
	}
	return reviewRoundFixture{
		f:          f,
		developer:  parseTestUUID(t, developerID),
		reviewerA:  parseTestUUID(t, reviewerAID),
		reviewerB:  parseTestUUID(t, reviewerBID),
		issue:      issue,
		reviewerAA: reviewerA,
	}
}

func (fx reviewRoundFixture) mentionCreator(t *testing.T) ReviewTaskCreator {
	t.Helper()
	return func(ctx context.Context, key string) (db.AgentTaskQueue, error) {
		return fx.f.svc.TaskSvc.EnqueueMentionTaskForReview(ctx, fx.issue, fx.reviewerA, pgtype.UUID{}, OriginNamed, key)
	}
}

func (fx reviewRoundFixture) wakeupCreator(t *testing.T) ReviewTaskCreator {
	t.Helper()
	return func(ctx context.Context, key string) (db.AgentTaskQueue, error) {
		return fx.f.q.CreateWakeupTask(ctx, db.CreateWakeupTaskParams{
			ID:                dbid.NewV7(),
			AgentID:           fx.reviewerA,
			RuntimeID:         fx.reviewerAA.RuntimeID,
			IssueID:           fx.issue.ID,
			Priority:          0,
			WakeupContext:     []byte(`{"wakeup_id":"00000000-0000-0000-0000-000000000001"}`),
			OriginatorUserID:  parseTestUUID(t, fx.f.UserID),
			AccountableUserID: parseTestUUID(t, fx.f.UserID),
			DedupeKey:         pgtype.Text{String: key, Valid: true},
		})
	}
}

func (fx reviewRoundFixture) reloadIssue(t *testing.T) db.Issue {
	t.Helper()
	issue, err := fx.f.q.GetIssue(context.Background(), fx.issue.ID)
	if err != nil {
		t.Fatalf("reload issue: %v", err)
	}
	return issue
}

// TestRequestReviewConcurrentMentionAndWakeup covers MP-66: the status wakeup
// and the delivery mention describe the same handoff and race for the same
// reviewer. Exactly one task may be created; every loser returns the winner.
func TestRequestReviewConcurrentMentionAndWakeup(t *testing.T) {
	fx := newReviewRoundFixture(t)
	ctx := context.Background()
	svc := fx.f.svc.TaskSvc
	key, ok := ReviewTaskKey(fx.issue, fx.reviewerA)
	if !ok {
		t.Fatal("expected a review key for the reviewer on in_review")
	}

	const attempts = 10
	type result struct {
		res ReviewTaskResult
		err error
	}
	results := make(chan result, attempts)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < attempts; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			create := fx.mentionCreator(t)
			if i%2 == 1 {
				create = fx.wakeupCreator(t)
			}
			res, err := svc.RequestReview(ctx, fx.issue, fx.reviewerA, create)
			results <- result{res: res, err: err}
		}(i)
	}
	close(start)
	wg.Wait()
	close(results)

	var winnerID pgtype.UUID
	taskIDs := map[[16]byte]struct{}{}
	created := 0
	for r := range results {
		if r.err != nil {
			t.Fatalf("RequestReview: %v", r.err)
		}
		if r.res.Key != key {
			t.Fatalf("key = %q, want %q", r.res.Key, key)
		}
		if !r.res.Existing {
			created++
			winnerID = r.res.Task.ID
		}
		taskIDs[r.res.Task.ID.Bytes] = struct{}{}
	}
	if created != 1 {
		t.Fatalf("created review tasks = %d, want exactly 1", created)
	}
	if len(taskIDs) != 1 {
		t.Fatalf("review requests returned %d distinct tasks, want 1", len(taskIDs))
	}

	var active int
	if err := fx.f.Pool.QueryRow(ctx, `
		SELECT count(*) FROM agent_task_queue
		WHERE issue_id = $1 AND agent_id = $2 AND status IN ('queued','dispatched','running','waiting_local_directory')
	`, fx.issue.ID, fx.reviewerA).Scan(&active); err != nil {
		t.Fatalf("count active review tasks: %v", err)
	}
	if active != 1 {
		t.Fatalf("active review tasks = %d, want 1", active)
	}
	var keyed int
	if err := fx.f.Pool.QueryRow(ctx, `
		SELECT count(*) FROM agent_task_queue WHERE dedupe_key = $1
	`, key).Scan(&keyed); err != nil {
		t.Fatalf("count keyed review tasks: %v", err)
	}
	if keyed != 1 {
		t.Fatalf("tasks holding the review key = %d, want 1", keyed)
	}
	if winnerID.Valid {
		var winnerStatus string
		if err := fx.f.Pool.QueryRow(ctx, `SELECT status FROM agent_task_queue WHERE id = $1`, winnerID).Scan(&winnerStatus); err != nil {
			t.Fatalf("load winner: %v", err)
		}
		if winnerStatus != "queued" {
			t.Fatalf("winner status = %q, want queued", winnerStatus)
		}
	}
}

// TestRequestReviewDifferentReviewersAreIndependent locks the reviewer part of
// the key: two agents reviewing the same round each get one task.
func TestRequestReviewDifferentReviewersAreIndependent(t *testing.T) {
	fx := newReviewRoundFixture(t)
	ctx := context.Background()
	svc := fx.f.svc.TaskSvc
	create := func(agentID pgtype.UUID) ReviewTaskCreator {
		return func(ctx context.Context, key string) (db.AgentTaskQueue, error) {
			return svc.EnqueueMentionTaskForReview(ctx, fx.issue, agentID, pgtype.UUID{}, OriginNamed, key)
		}
	}
	a, err := svc.RequestReview(ctx, fx.issue, fx.reviewerA, create(fx.reviewerA))
	if err != nil {
		t.Fatalf("reviewer A: %v", err)
	}
	b, err := svc.RequestReview(ctx, fx.issue, fx.reviewerB, create(fx.reviewerB))
	if err != nil {
		t.Fatalf("reviewer B: %v", err)
	}
	if a.Existing || b.Existing {
		t.Fatalf("distinct reviewers must each create a task: a=%+v b=%+v", a, b)
	}
	if a.Task.ID == b.Task.ID {
		t.Fatal("distinct reviewers shared a task")
	}
	if a.Key == b.Key {
		t.Fatal("distinct reviewers shared a review key")
	}
}

// TestRequestReviewTerminalTaskReleasesKey covers failure retry: the partial
// index only holds active rows, and RequestReview must create a fresh task once
// the previous attempt is terminal.
func TestRequestReviewTerminalTaskReleasesKey(t *testing.T) {
	fx := newReviewRoundFixture(t)
	ctx := context.Background()
	svc := fx.f.svc.TaskSvc
	create := fx.mentionCreator(t)

	first, err := svc.RequestReview(ctx, fx.issue, fx.reviewerA, create)
	if err != nil {
		t.Fatalf("first review: %v", err)
	}
	if _, err := fx.f.Pool.Exec(ctx, `
		UPDATE agent_task_queue SET status = 'failed', completed_at = now(), failure_reason = 'agent_error'
		WHERE id = $1
	`, first.Task.ID); err != nil {
		t.Fatalf("fail first review: %v", err)
	}

	second, err := svc.RequestReview(ctx, fx.issue, fx.reviewerA, create)
	if err != nil {
		t.Fatalf("retry review: %v", err)
	}
	if second.Existing {
		t.Fatal("a failed attempt must not satisfy the retry")
	}
	if second.Task.ID == first.Task.ID {
		t.Fatal("retry reused the failed task")
	}
	if second.Key != first.Key {
		t.Fatalf("retry key = %q, want the same round key %q", second.Key, first.Key)
	}
}

// TestCreateRetryTaskPreservesReviewKey covers the automatic retry path: clone
// must keep the round key so an auto-retry cannot race a mention/wakeup into a
// second active review.
func TestCreateRetryTaskPreservesReviewKey(t *testing.T) {
	fx := newReviewRoundFixture(t)
	ctx := context.Background()
	svc := fx.f.svc.TaskSvc

	first, err := svc.RequestReview(ctx, fx.issue, fx.reviewerA, fx.mentionCreator(t))
	if err != nil {
		t.Fatalf("first review: %v", err)
	}
	if _, err := fx.f.Pool.Exec(ctx, `
		UPDATE agent_task_queue SET status = 'failed', completed_at = now(), failure_reason = 'runtime_offline'
		WHERE id = $1
	`, first.Task.ID); err != nil {
		t.Fatalf("fail review: %v", err)
	}
	retry, err := fx.f.q.CreateRetryTask(ctx, db.CreateRetryTaskParams{
		ID:        first.Task.ID,
		NewTaskID: dbid.NewV7(),
	})
	if err != nil {
		t.Fatalf("CreateRetryTask: %v", err)
	}
	if retry.DedupeKey != first.Task.DedupeKey {
		t.Fatalf("retry dedupe_key = %v, want %v", retry.DedupeKey, first.Task.DedupeKey)
	}
}

// TestSupersedeStaleReviewTasksCancelsQueuedPreviousRound covers a new handoff:
// the queued run from the previous generation must stop and the new round must
// enqueue under its own key.
func TestSupersedeStaleReviewTasksCancelsQueuedPreviousRound(t *testing.T) {
	fx := newReviewRoundFixture(t)
	ctx := context.Background()
	svc := fx.f.svc.TaskSvc

	first, err := svc.RequestReview(ctx, fx.issue, fx.reviewerA, fx.mentionCreator(t))
	if err != nil {
		t.Fatalf("first review: %v", err)
	}
	if _, err := fx.f.Pool.Exec(ctx, `UPDATE issue SET status = 'in_progress' WHERE id = $1`, fx.issue.ID); err != nil {
		t.Fatalf("leave review: %v", err)
	}
	if _, err := fx.f.Pool.Exec(ctx, `UPDATE issue SET status = 'in_review' WHERE id = $1`, fx.issue.ID); err != nil {
		t.Fatalf("re-enter review: %v", err)
	}
	issue := fx.reloadIssue(t)
	if issue.ReviewGeneration != 1 {
		t.Fatalf("review generation = %d, want 1", issue.ReviewGeneration)
	}
	newKey, _ := ReviewTaskKey(issue, fx.reviewerA)
	if newKey == first.Key {
		t.Fatal("new generation reused the previous review key")
	}
	if err := svc.SupersedeStaleReviewTasks(ctx, issue.ID, fx.reviewerA, newKey); err != nil {
		t.Fatalf("supersede: %v", err)
	}
	old, err := fx.f.q.GetAgentTask(ctx, first.Task.ID)
	if err != nil {
		t.Fatalf("reload old task: %v", err)
	}
	if old.Status != "cancelled" || old.FailureReason.String != "review_superseded" {
		t.Fatalf("old task = status %q reason %q, want cancelled/review_superseded", old.Status, old.FailureReason.String)
	}

	second, err := svc.RequestReview(ctx, issue, fx.reviewerA, func(ctx context.Context, key string) (db.AgentTaskQueue, error) {
		return svc.EnqueueMentionTaskForReview(ctx, issue, fx.reviewerA, pgtype.UUID{}, OriginNamed, key)
	})
	if err != nil {
		t.Fatalf("new review: %v", err)
	}
	if second.Existing || second.Key != newKey {
		t.Fatalf("new review = %+v, want a fresh task under %q", second, newKey)
	}
}

// TestSupersedeStaleReviewTasksMarksRunningPreviousRound protects a review that
// already started before the new handoff: it is not interrupted, but its
// eventual verdict is refused.
func TestSupersedeStaleReviewTasksMarksRunningPreviousRound(t *testing.T) {
	fx := newReviewRoundFixture(t)
	ctx := context.Background()
	svc := fx.f.svc.TaskSvc

	first, err := svc.RequestReview(ctx, fx.issue, fx.reviewerA, fx.mentionCreator(t))
	if err != nil {
		t.Fatalf("first review: %v", err)
	}
	if _, err := fx.f.Pool.Exec(ctx, `UPDATE agent_task_queue SET status = 'running', started_at = now() WHERE id = $1`, first.Task.ID); err != nil {
		t.Fatalf("start review: %v", err)
	}
	if _, err := fx.f.Pool.Exec(ctx, `UPDATE issue SET status = 'in_progress' WHERE id = $1`, fx.issue.ID); err != nil {
		t.Fatalf("leave review: %v", err)
	}
	if _, err := fx.f.Pool.Exec(ctx, `UPDATE issue SET status = 'in_review' WHERE id = $1`, fx.issue.ID); err != nil {
		t.Fatalf("re-enter review: %v", err)
	}
	issue := fx.reloadIssue(t)
	newKey, _ := ReviewTaskKey(issue, fx.reviewerA)
	if err := svc.SupersedeStaleReviewTasks(ctx, issue.ID, fx.reviewerA, newKey); err != nil {
		t.Fatalf("supersede: %v", err)
	}
	old, err := fx.f.q.GetAgentTask(ctx, first.Task.ID)
	if err != nil {
		t.Fatalf("reload old task: %v", err)
	}
	if old.Status != "running" {
		t.Fatalf("running review was interrupted: status = %q", old.Status)
	}
	if !ReviewTaskSuperseded(old) {
		t.Fatal("running review was not marked superseded")
	}
	generation, ok := ReviewGenerationFromTask(old)
	if !ok || generation == issue.ReviewGeneration {
		t.Fatalf("old run generation = %d (ok=%v), issue generation = %d", generation, ok, issue.ReviewGeneration)
	}
}

// TestReviewTaskKeyGates pins the shape of the key: only an in_review issue's
// non-assignee agent is a review request.
func TestReviewTaskKeyGates(t *testing.T) {
	issueID := pgtype.UUID{Bytes: [16]byte{1}, Valid: true}
	developerID := pgtype.UUID{Bytes: [16]byte{2}, Valid: true}
	reviewerID := pgtype.UUID{Bytes: [16]byte{3}, Valid: true}
	base := db.Issue{ID: issueID, Status: "in_review", AssigneeID: developerID, AssigneeType: pgtype.Text{String: "agent", Valid: true}, ReviewGeneration: 4}
	key, ok := ReviewTaskKey(base, reviewerID)
	if !ok || key != "review:01000000-0000-0000-0000-000000000000:03000000-0000-0000-0000-000000000000:4" {
		t.Fatalf("key = %q ok=%v", key, ok)
	}
	if _, ok := ReviewTaskKey(base, developerID); ok {
		t.Fatal("the issue assignee must not be a review request")
	}
	notReview := base
	notReview.Status = "in_progress"
	if _, ok := ReviewTaskKey(notReview, reviewerID); ok {
		t.Fatal("a non-review status must not produce a review key")
	}
	if _, ok := ReviewTaskKey(base, pgtype.UUID{}); ok {
		t.Fatal("an invalid agent id must not produce a review key")
	}
}

func TestRequestReviewRejectsNonReviewRequest(t *testing.T) {
	svc := &TaskService{}
	_, err := svc.RequestReview(context.Background(), db.Issue{}, pgtype.UUID{}, nil)
	if !errors.Is(err, ErrNotReviewRequest) {
		t.Fatalf("err = %v, want ErrNotReviewRequest", err)
	}
}
