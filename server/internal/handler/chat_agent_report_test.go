package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/multica-ai/multica/server/pkg/protocol"
)

// TestChatAPIsExposeAgentReportKind pins the REST projection for the new
// message-mode autopilot outcome. The WebSocket chat:done event already carries
// the raw kind; both read paths must agree or clients lose the report kind after
// a refetch.
func TestChatAPIsExposeAgentReportKind(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	agentID := createHandlerTestAgent(t, "ChatAgentReportAgent", []byte("[]"))
	sessionID := createHandlerTestChatSession(t, agentID)
	if _, err := testPool.Exec(context.Background(), `
		INSERT INTO chat_message (chat_session_id, role, content, message_kind)
		VALUES ($1, 'assistant', 'Daily report', 'agent_report')
	`, sessionID); err != nil {
		t.Fatalf("insert agent report: %v", err)
	}

	page := fetchChatMessagesPageForTest(t, sessionID, nil)
	if len(page.Messages) != 1 || page.Messages[0].MessageKind != protocol.ChatMessageKindAgentReport {
		t.Fatalf("page messages = %+v, want one agent_report", page.Messages)
	}

	req := newRequest("GET", "/api/chat/sessions", nil)
	req = withChatTestWorkspaceCtx(t, req)
	w := httptest.NewRecorder()
	testHandler.ListChatSessions(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("ListChatSessions: expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var sessions []ChatSessionResponse
	if err := json.Unmarshal(w.Body.Bytes(), &sessions); err != nil {
		t.Fatalf("decode sessions: %v", err)
	}
	for _, session := range sessions {
		if session.ID != sessionID {
			continue
		}
		if session.LastMessage == nil || session.LastMessage.MessageKind != protocol.ChatMessageKindAgentReport {
			t.Fatalf("last_message = %+v, want agent_report", session.LastMessage)
		}
		return
	}
	t.Fatalf("session %s missing from list", sessionID)
}
