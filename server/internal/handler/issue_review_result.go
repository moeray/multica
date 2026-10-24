package handler

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/multica-ai/multica/server/internal/issuestatus"
	"github.com/multica-ai/multica/server/internal/service"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

type reviewResultDispatch struct {
	// Delivered means the review result is now visible in the source chat, so
	// the generic issue status notification would be a duplicate.
	Delivered bool
	// SuppressStatusNotification covers the developer's in_review handoff:
	// the reviewer is notified by the final comment mention, not by a second
	// issue status inbox item.
	SuppressStatusNotification bool
}

// dispatchReviewResult turns the reviewer's status transition out of in_review
// into one chat message. The actor must be the agent the assignee (developer)
// named in their newest handoff comment; this is the workflow's reviewer
// signal.
func (h *Handler) dispatchReviewResult(
	ctx context.Context,
	prevIssue db.Issue,
	issue db.Issue,
	actorType string,
	actorID string,
	callerRun *db.AgentTaskQueue,
) reviewResultDispatch {
	if h.AgentMessages == nil || prevIssue.Status == issue.Status {
		return reviewResultDispatch{}
	}
	// A verdict belongs to the round its run was enqueued for. Once the issue
	// enters a newer review round, an older run may still finish (or a
	// superseded run may still move the status), but its verdict must not be
	// published as if it reviewed the newer handoff.
	if callerRun != nil && service.IsReviewTask(*callerRun) {
		if service.ReviewTaskSuperseded(*callerRun) {
			slog.Info("review result suppressed: run was superseded by a newer round",
				"issue_id", uuidToString(issue.ID),
				"task_id", uuidToString(callerRun.ID))
			return reviewResultDispatch{}
		}
		if generation, ok := service.ReviewGenerationFromTask(*callerRun); ok && generation != issue.ReviewGeneration {
			slog.Info("review result suppressed: run reviewed an older generation",
				"issue_id", uuidToString(issue.ID),
				"task_id", uuidToString(callerRun.ID),
				"run_generation", generation,
				"issue_generation", issue.ReviewGeneration)
			return reviewResultDispatch{}
		}
	}

	session, err := service.ResolveIssueChatSession(ctx, h.Queries, issue)
	if err != nil {
		slog.Warn("review result: resolve source chat failed",
			"issue_id", uuidToString(issue.ID), "error", err)
		return reviewResultDispatch{}
	}
	if session == nil {
		return reviewResultDispatch{}
	}

	if issue.Status == "in_review" {
		// The developer finishing work must only notify the reviewer. The
		// reviewer is already reached by the final comment's mention.
		if actorType == "agent" && issue.AssigneeID.Valid && actorID == uuidToString(issue.AssigneeID) {
			return reviewResultDispatch{SuppressStatusNotification: true}
		}
		return reviewResultDispatch{}
	}
	if prevIssue.Status != "in_review" || actorType != "agent" {
		return reviewResultDispatch{}
	}
	// Only the reviewer the developer handed off to may publish a verdict.
	// The handoff is the assignee agent's newest comment; an agent that was
	// not mentioned there is not a reviewer, so this stays on the existing
	// issue notification path instead of emitting a forged result.
	reviewer, err := service.IsIssueReviewer(ctx, h.Queries, issue, actorID)
	if err != nil {
		slog.Warn("review result: resolve reviewer failed; keeping existing issue notification",
			"issue_id", uuidToString(issue.ID), "error", err)
		return reviewResultDispatch{}
	}
	if !reviewer {
		return reviewResultDispatch{}
	}
	if issue.Status == "cancelled" {
		return reviewResultDispatch{}
	}

	passed := reviewPassed(ctx, h.Queries, issue)
	result, err := h.AgentMessages.SendReviewResult(ctx, service.ReviewResultRequest{
		Issue:     issue,
		ActorType: actorType,
		ActorID:   actorID,
		DedupeKey: fmt.Sprintf("review_result:%s:%d", uuidToString(issue.ID), issue.Revision),
		Passed:    passed,
	})
	if errors.Is(err, service.ErrNoSourceChatSession) {
		return reviewResultDispatch{}
	}
	if err != nil {
		slog.Warn("review result: send failed; keeping existing issue notification",
			"issue_id", uuidToString(issue.ID), "error", err)
		return reviewResultDispatch{}
	}
	if !result.Delivered {
		return reviewResultDispatch{}
	}
	return reviewResultDispatch{Delivered: true, SuppressStatusNotification: true}
}

func reviewPassed(ctx context.Context, q *db.Queries, issue db.Issue) bool {
	category, err := issuestatus.CategoryWithError(ctx, q, issue.WorkspaceID, issue.Status)
	if err != nil {
		return issue.Status == "done"
	}
	return category == issuestatus.CategoryDone || category == issuestatus.CategoryClosed
}
