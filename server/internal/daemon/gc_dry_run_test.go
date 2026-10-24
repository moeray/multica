package daemon

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"testing"
)

func TestRunGC_DryRunReportsCandidatesWithoutDeleting(t *testing.T) {
	t.Parallel()

	d := newGCTestDaemon(t, http.NewServeMux())
	d.cfg.GCDryRun = true
	d.cfg.GCOrphanTTL = 0
	taskDir := createTaskDir(t, d.cfg.WorkspacesRoot, "ws1", "would-clean", nil)
	payload := filepath.Join(taskDir, "workdir", "source.go")
	writeFile(t, payload, 128)

	d.runGC(context.Background())

	if _, err := os.Stat(payload); err != nil {
		t.Fatalf("dry run removed task content: %v", err)
	}
	metrics := d.lastGC.Load()
	if metrics == nil {
		t.Fatal("GC cycle did not publish metrics")
	}
	if !metrics.DryRun {
		t.Fatal("metrics.DryRun = false, want true")
	}
	if metrics.DryRunCandidates != 1 {
		t.Fatalf("DryRunCandidates = %d, want 1", metrics.DryRunCandidates)
	}
	if metrics.CleanupSucceeded != 0 || metrics.CleanupFailed != 0 {
		t.Fatalf("dry run reported deletions: succeeded=%d failed=%d",
			metrics.CleanupSucceeded, metrics.CleanupFailed)
	}
}
