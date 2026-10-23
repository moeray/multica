package handler

import (
	"context"
	"fmt"
	"testing"

	"github.com/multica-ai/multica/server/internal/events"
	"github.com/multica-ai/multica/server/internal/service"
	"github.com/multica-ai/multica/server/internal/testutil"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

type recordingReviewResultSender struct {
	calls int
}

func (s *recordingReviewResultSender) Send(context.Context, db.ChannelChatSessionBinding, db.ChatMessage) error {
	s.calls++
	return nil
}

// TestDispatchReviewResultRejectsNonReviewerAgent pins the permission boundary:
// only the agent named in the assignee's handoff comment may publish a review
// result. Another agent moving the issue out of in_review must not write one.
func TestDispatchReviewResultRejectsNonReviewerAgent(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	ctx := context.Background()
	developerID := createHandlerTestAgent(t, "Review Result Developer", nil)
	reviewerID := createHandlerTestAgent(t, "Review Result Reviewer", nil)
	intruderID := createHandlerTestAgent(t, "Review Result Intruder", nil)

	issueID := dbfx.Issue(t, "review result permission", testutil.Cols{
		"status":        "done",
		"assignee_type": "agent",
		"assignee_id":   developerID,
	})
	dbfx.Comment(t, issueID,
		fmt.Sprintf("[@Reviewer](mention://agent/%s) please review", reviewerID),
		testutil.Cols{"author_type": "agent", "author_id": developerID})
	sessionID := dbfx.Insert(t, "chat_session", testutil.Cols{
		"workspace_id": testWorkspaceID,
		"agent_id":     developerID,
		"creator_id":   testUserID,
		"title":        "review result",
	})
	dbfx.Exec(t, `UPDATE issue SET origin_type = 'lark_chat', origin_id = $1 WHERE id = $2`, sessionID, issueID)

	issue, err := testHandler.Queries.GetIssue(ctx, util.MustParseUUID(issueID))
	if err != nil {
		t.Fatalf("load issue: %v", err)
	}
	prevIssue := issue
	prevIssue.Status = "in_review"

	sender := &recordingReviewResultSender{}
	h := *testHandler
	h.AgentMessages = service.NewAgentMessageService(testHandler.Queries, events.New(), sender, "")

	got := h.dispatchReviewResult(ctx, prevIssue, issue, "agent", intruderID)
	if got.Delivered || got.SuppressStatusNotification {
		t.Fatalf("non-reviewer dispatch = %+v, want no delivery", got)
	}
	if sender.calls != 0 {
		t.Fatalf("non-reviewer sender calls = %d, want 0", sender.calls)
	}
	if count := dbfx.Count(t, `
		SELECT count(*) FROM chat_message
		WHERE chat_session_id = $1 AND message_kind = 'review_result'`, sessionID); count != 0 {
		t.Fatalf("review_result rows written by a non-reviewer = %d, want 0", count)
	}

	h.dispatchReviewResult(ctx, prevIssue, issue, "agent", reviewerID)
	if count := dbfx.Count(t, `
		SELECT count(*) FROM chat_message
		WHERE chat_session_id = $1 AND message_kind = 'review_result'`, sessionID); count != 1 {
		t.Fatalf("review_result rows written by the reviewer = %d, want 1", count)
	}
}
