package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

// reviewDedupeKeyPrefix marks task rows that belong to a review round. The
// suffix is "<issue_id>:<reviewer_agent_id>:<review_generation>".
const reviewDedupeKeyPrefix = "review:"

// ErrNotReviewRequest means the (issue, agent) pair does not describe a review
// request. Callers use ReviewTaskKey first; this is a defensive sentinel for
// code paths that pass the request through unconditionally.
var ErrNotReviewRequest = errors.New("not a review request")

// ReviewTaskKey returns the idempotency key for one reviewer on one review
// round. The generation advances whenever the issue enters in_review, so a new
// handoff always gets a fresh key while the mention and the status wakeup that
// describe the same handoff share one.
//
// The assigned developer is never the reviewer: a run that the issue's own
// assignee triggers is ordinary work, not a review.
func ReviewTaskKey(issue db.Issue, agentID pgtype.UUID) (string, bool) {
	if !issue.ID.Valid || !agentID.Valid || issue.Status != "in_review" {
		return "", false
	}
	if issue.AssigneeID.Valid && issue.AssigneeID == agentID {
		return "", false
	}
	return fmt.Sprintf("%s%s:%s:%d",
		reviewDedupeKeyPrefix,
		util.UUIDToString(issue.ID),
		util.UUIDToString(agentID),
		issue.ReviewGeneration,
	), true
}

// ReviewGenerationFromTask extracts the review round a task was enqueued for.
// It only reports true for rows carrying a review dedupe key.
func ReviewGenerationFromTask(task db.AgentTaskQueue) (int64, bool) {
	if !task.DedupeKey.Valid {
		return 0, false
	}
	key := task.DedupeKey.String
	if !strings.HasPrefix(key, reviewDedupeKeyPrefix) {
		return 0, false
	}
	idx := strings.LastIndexByte(key, ':')
	if idx < 0 || idx == len(key)-1 {
		return 0, false
	}
	generation, err := strconv.ParseInt(key[idx+1:], 10, 64)
	if err != nil {
		return 0, false
	}
	return generation, true
}

// IsReviewTask reports whether the row belongs to a review round.
func IsReviewTask(task db.AgentTaskQueue) bool {
	return task.DedupeKey.Valid && strings.HasPrefix(task.DedupeKey.String, reviewDedupeKeyPrefix)
}

// ReviewTaskSuperseded reports whether a newer review round has marked this
// started task as stale. Such a task may finish its work, but it must not
// publish a review_result for the newer round.
func ReviewTaskSuperseded(task db.AgentTaskQueue) bool {
	if len(task.Context) == 0 {
		return false
	}
	var context struct {
		ReviewSuperseded bool `json:"review_superseded"`
	}
	if err := json.Unmarshal(task.Context, &context); err != nil {
		return false
	}
	return context.ReviewSuperseded
}

// ReviewTaskResult is the one live task for a review round. Existing is true
// when another trigger (mention, wakeup, manual rerun) already created it.
type ReviewTaskResult struct {
	Key      string
	Task     db.AgentTaskQueue
	Existing bool
}

// ReviewTaskCreator inserts the source-specific review task (a mention task or
// a wakeup task). It must persist the supplied key as agent_task_queue.dedupe_key.
type ReviewTaskCreator func(ctx context.Context, key string) (db.AgentTaskQueue, error)

// RequestReview is the single idempotent entry for every review trigger. It
// returns the existing live task instead of creating a second one when the same
// reviewer already has an active task for the round. A terminal task no longer
// holds the partial unique key, so a failed review can be retried.
func (s *TaskService) RequestReview(ctx context.Context, issue db.Issue, agentID pgtype.UUID, create ReviewTaskCreator) (ReviewTaskResult, error) {
	return s.requestReviewWithQueries(ctx, s.Queries, issue, agentID, create)
}

// RequestReviewTx is RequestReview for callers already inside a transaction
// (the wakeup dispatcher). The creator must insert through the same q.
func (s *TaskService) RequestReviewTx(ctx context.Context, q *db.Queries, issue db.Issue, agentID pgtype.UUID, create ReviewTaskCreator) (ReviewTaskResult, error) {
	return s.requestReviewWithQueries(ctx, q, issue, agentID, create)
}

func (s *TaskService) requestReviewWithQueries(ctx context.Context, q *db.Queries, issue db.Issue, agentID pgtype.UUID, create ReviewTaskCreator) (ReviewTaskResult, error) {
	key, ok := ReviewTaskKey(issue, agentID)
	if !ok {
		return ReviewTaskResult{}, ErrNotReviewRequest
	}
	keyText := pgtype.Text{String: key, Valid: true}

	existing, err := q.GetActiveReviewTaskByDedupeKey(ctx, keyText)
	if err == nil {
		slog.Info("review request coalesced: round already has an active task",
			"issue_id", util.UUIDToString(issue.ID),
			"agent_id", util.UUIDToString(agentID),
			"review_key", key,
			"task_id", util.UUIDToString(existing.ID),
			"task_status", existing.Status,
		)
		return ReviewTaskResult{Key: key, Task: existing, Existing: true}, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return ReviewTaskResult{}, fmt.Errorf("look up active review task: %w", err)
	}

	task, err := create(ctx, key)
	if err != nil {
		// A concurrent trigger won the key, or an older task still occupies the
		// ordinary pending slot for this thread. In the former case the winner
		// is exactly the task this request should return.
		if errors.Is(err, pgx.ErrNoRows) || errors.Is(err, ErrDuplicatePendingTask) {
			winner, lookupErr := q.GetActiveReviewTaskByDedupeKey(ctx, keyText)
			if lookupErr == nil {
				slog.Info("review request lost the race and merged into the winner",
					"issue_id", util.UUIDToString(issue.ID),
					"agent_id", util.UUIDToString(agentID),
					"review_key", key,
					"task_id", util.UUIDToString(winner.ID),
				)
				return ReviewTaskResult{Key: key, Task: winner, Existing: true}, nil
			}
			if !errors.Is(lookupErr, pgx.ErrNoRows) {
				return ReviewTaskResult{}, fmt.Errorf("look up review task after conflict: %w", lookupErr)
			}
		}
		return ReviewTaskResult{}, err
	}

	slog.Info("review task enqueued",
		"issue_id", util.UUIDToString(issue.ID),
		"agent_id", util.UUIDToString(agentID),
		"review_key", key,
		"review_generation", issue.ReviewGeneration,
		"task_id", util.UUIDToString(task.ID),
	)
	return ReviewTaskResult{Key: key, Task: task}, nil
}

// SupersedeStaleReviewTasks retires work from an older review round before a
// new round enqueues. Unstarted rows are cancelled and settled so no stale run
// can begin; rows already dispatched or running are marked superseded so their
// eventual result is refused without interrupting work in flight.
func (s *TaskService) SupersedeStaleReviewTasks(ctx context.Context, issueID, agentID pgtype.UUID, keepKey string) error {
	var cancelled []db.AgentTaskQueue
	err := s.runInTx(ctx, func(q *db.Queries) error {
		var err error
		cancelled, _, err = s.supersedeStaleReviewTasksWithQueries(ctx, q, issueID, agentID, keepKey)
		if err != nil {
			return err
		}
		return SettleTerminalTaskState(ctx, q, cancelled...)
	})
	if err != nil {
		return err
	}
	for _, task := range cancelled {
		s.captureTaskCancelled(ctx, task)
		if s.Bus != nil {
			s.broadcastTaskEvent(ctx, protocol.EventTaskCancelled, task)
		}
	}
	if s.Bus != nil {
		for _, id := range distinctAgentIDs(cancelled) {
			s.ReconcileAgentStatus(ctx, id)
		}
	}
	s.notifyTasksFinished(cancelled)
	return nil
}

// supersedeStaleReviewTasksWithQueries performs only the row updates; the
// caller owns settlement and post-commit notification. The wakeup dispatcher
// calls it inside its own transaction.
func (s *TaskService) supersedeStaleReviewTasksWithQueries(
	ctx context.Context, q *db.Queries, issueID, agentID pgtype.UUID, keepKey string,
) (cancelled, superseded []db.AgentTaskQueue, err error) {
	cancelled, err = q.CancelStaleReviewTasks(ctx, db.CancelStaleReviewTasksParams{
		IssueID: issueID, AgentID: agentID, KeepKey: pgtype.Text{String: keepKey, Valid: true},
	})
	if err != nil {
		return nil, nil, fmt.Errorf("cancel stale review tasks: %w", err)
	}
	superseded, err = q.MarkStaleReviewTasksSuperseded(ctx, db.MarkStaleReviewTasksSupersededParams{
		IssueID: issueID, AgentID: agentID, KeepKey: keepKey,
	})
	if err != nil {
		return nil, nil, fmt.Errorf("mark stale review tasks superseded: %w", err)
	}
	for _, task := range cancelled {
		slog.Info("review task superseded: cancelled unstarted previous round",
			"issue_id", util.UUIDToString(issueID),
			"agent_id", util.UUIDToString(agentID),
			"task_id", util.UUIDToString(task.ID),
			"review_key", keepKey,
		)
	}
	for _, task := range superseded {
		slog.Info("review task superseded: started task will not publish a result",
			"issue_id", util.UUIDToString(issueID),
			"agent_id", util.UUIDToString(agentID),
			"task_id", util.UUIDToString(task.ID),
			"review_key", keepKey,
		)
	}
	return cancelled, superseded, nil
}
