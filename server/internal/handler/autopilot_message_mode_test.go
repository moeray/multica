package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestCreateAutopilotAcceptsMessageMode(t *testing.T) {
	agentID := createHandlerTestAgent(t, "message-mode-agent", nil)
	body := map[string]any{
		"title":          "message-mode",
		"assignee_id":    agentID,
		"execution_mode": "message",
	}
	w := httptest.NewRecorder()
	testHandler.CreateAutopilot(w, newRequest("POST", "/api/autopilots?workspace_id="+testWorkspaceID, body))
	if w.Code != http.StatusCreated {
		t.Fatalf("CreateAutopilot(message): expected 201, got %d: %s", w.Code, w.Body.String())
	}
	var ap AutopilotResponse
	if err := json.NewDecoder(w.Body).Decode(&ap); err != nil {
		t.Fatalf("decode autopilot: %v", err)
	}
	if ap.ExecutionMode != "message" {
		t.Fatalf("execution_mode = %q, want message", ap.ExecutionMode)
	}
	t.Cleanup(func() {
		testPool.Exec(context.Background(), `DELETE FROM autopilot_run WHERE autopilot_id = $1`, ap.ID)
		testPool.Exec(context.Background(), `DELETE FROM autopilot_trigger WHERE autopilot_id = $1`, ap.ID)
		testPool.Exec(context.Background(), `DELETE FROM autopilot WHERE id = $1`, ap.ID)
	})
}
