package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/multica-ai/multica/server/pkg/protocol"
)

// claimedContextMessages is the injection slice of a claim response — the field
// the daemon renders into the per-turn prompt (MP-68).
type claimedContextMessages struct {
	ChatMessage     string `json:"chat_message"`
	ContextMessages []struct {
		ID           string `json:"id"`
		MessageKind  string `json:"message_kind"`
		ReviewPassed *bool  `json:"review_passed"`
		Content      string `json:"content"`
	} `json:"chat_context_messages"`
}

func claimContextMessages(t *testing.T, runtimeID string) claimedContextMessages {
	t.Helper()
	w := httptest.NewRecorder()
	req := newDaemonTokenRequest("POST", "/api/daemon/runtimes/"+runtimeID+"/tasks/claim", nil,
		testWorkspaceID, "claim-context-messages")
	req = withURLParam(req, "runtimeId", runtimeID)

	testHandler.ClaimTaskByRuntime(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("ClaimTaskByRuntime: expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var resp struct {
		Task *claimedContextMessages `json:"task"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode claim response: %v", err)
	}
	if resp.Task == nil {
		t.Fatal("expected a claimed task")
	}
	return *resp.Task
}

func seedMirroredReviewResult(t *testing.T, ctx context.Context, sessionID, content string) string {
	t.Helper()
	var id string
	if err := testPool.QueryRow(ctx, `
		INSERT INTO chat_message (chat_session_id, role, content, message_kind, review_passed)
		VALUES ($1, 'assistant', $2, $3, true)
		RETURNING id`, sessionID, content, protocol.ChatMessageKindReviewResult).Scan(&id); err != nil {
		t.Fatalf("seed mirrored review result: %v", err)
	}
	return id
}

// TestClaim_ShipsMirroredMessagesForTheTurn is the server half of acceptance 1:
// the mirror stamped for this turn reaches the claim payload, structurally
// (kind + review_passed + body) and separately from the member's own message.
// Without this the daemon has nothing to render, however good the prompt copy.
func TestClaim_ShipsMirroredMessagesForTheTurn(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	ctx := context.Background()
	agentID, sessionID, runtimeID, _ := setupDirectChatSession(t, ctx, "mirror-backed chat")

	mirrorID := seedMirroredReviewResult(t, ctx, sessionID,
		protocol.BuildReviewResultContent(true, "阻塞项已修复。", "MP-67"))
	t.Cleanup(func() {
		testPool.Exec(context.Background(), `DELETE FROM chat_message WHERE chat_session_id = $1`, sessionID)
	})

	// The real send path is what stamps the mirror for the new turn.
	sendDirectChat(t, ctx, agentID, sessionID, "刚才那条评审结果还差什么？")
	requeueTaskForClaim(t, ctx, sessionID)

	claimed := claimContextMessages(t, runtimeID)
	if len(claimed.ContextMessages) != 1 {
		t.Fatalf("claim shipped %d context messages, want 1", len(claimed.ContextMessages))
	}
	got := claimed.ContextMessages[0]
	if got.ID != mirrorID {
		t.Errorf("context message id = %q, want %s", got.ID, mirrorID)
	}
	if got.MessageKind != protocol.ChatMessageKindReviewResult {
		t.Errorf("message_kind = %q", got.MessageKind)
	}
	if got.ReviewPassed == nil || !*got.ReviewPassed {
		t.Errorf("review_passed = %v, want true", got.ReviewPassed)
	}
	if got.Content == "" {
		t.Error("content is empty; the prompt has nothing to render")
	}
	// The mirror must not be folded into the member's message: the agent would
	// read another agent's review as the person's own words.
	if claimed.ChatMessage != "刚才那条评审结果还差什么？" {
		t.Errorf("chat_message = %q, want only the member's text", claimed.ChatMessage)
	}
}
