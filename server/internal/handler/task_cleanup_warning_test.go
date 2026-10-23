package handler

import (
	"testing"

	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

func TestTaskToResponse_CleanupWarningOutcome(t *testing.T) {
	t.Parallel()

	task := db.AgentTaskQueue{
		Status: "completed",
		Result: []byte(`{"output":"done","cleanup_warning":"unlinkat esbuild.exe: Access is denied"}`),
	}
	resp := taskToResponse(task, "")
	if resp.Status != "completed" {
		t.Fatalf("status = %q, want completed", resp.Status)
	}
	if resp.CompletionOutcome != "completed_with_cleanup_warning" {
		t.Errorf("completion_outcome = %q, want completed_with_cleanup_warning", resp.CompletionOutcome)
	}
	if resp.CleanupWarning != "unlinkat esbuild.exe: Access is denied" {
		t.Errorf("cleanup_warning = %q", resp.CleanupWarning)
	}
	if resp.FailureReason != "" {
		t.Errorf("failure_reason = %q, want empty", resp.FailureReason)
	}
}

func TestTaskToResponse_PlainCompletionHasNoCleanupWarning(t *testing.T) {
	t.Parallel()

	task := db.AgentTaskQueue{
		Status: "completed",
		Result: []byte(`{"output":"done"}`),
	}
	resp := taskToResponse(task, "")
	if resp.CompletionOutcome != "completed" {
		t.Errorf("completion_outcome = %q, want completed", resp.CompletionOutcome)
	}
	if resp.CleanupWarning != "" {
		t.Errorf("cleanup_warning = %q, want empty", resp.CleanupWarning)
	}
}
