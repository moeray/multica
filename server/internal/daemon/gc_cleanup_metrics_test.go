package daemon

import (
	"context"
	"net/http"
	"path/filepath"
	"testing"
)

func TestRecordArtifactCleanupCountsPerItem(t *testing.T) {
	stats := &gcStats{byPattern: map[string]int{}}
	recordArtifactCleanup(stats, artifactCleanupResult{
		Removed:    2,
		Failed:     1,
		Skipped:    3,
		Bytes:      128,
		PerPattern: map[string]int{"node_modules": 2},
	})

	if stats.cleanupSucceeded != 2 || stats.cleanupFailed != 1 || stats.cleanupSkipped != 3 {
		t.Fatalf("cleanup counts = success:%d failed:%d skipped:%d, want 2/1/3",
			stats.cleanupSucceeded, stats.cleanupFailed, stats.cleanupSkipped)
	}
	if stats.artifactDirs != 1 || stats.artifactRemoved != 2 || stats.bytesReclaimed != 128 {
		t.Fatalf("artifact totals = dirs:%d removed:%d bytes:%d, want 1/2/128",
			stats.artifactDirs, stats.artifactRemoved, stats.bytesReclaimed)
	}
	if stats.byPattern["node_modules"] != 2 {
		t.Fatalf("node_modules count = %d, want 2", stats.byPattern["node_modules"])
	}
}

func TestApplyGCActionCountsPolicySkip(t *testing.T) {
	d := newGCTestDaemon(t, http.NewServeMux())
	stats := &gcStats{byPattern: map[string]int{}}

	d.applyGCAction(context.Background(), t.TempDir(), gcActionSkip, stats)

	if stats.skipped != 1 || stats.cleanupSkipped != 1 {
		t.Fatalf("skip counters = skipped:%d cleanup_skipped:%d, want 1/1",
			stats.skipped, stats.cleanupSkipped)
	}
}

func TestGCWorkspaceCountsActiveTaskSkip(t *testing.T) {
	d := newGCTestDaemon(t, http.NewServeMux())
	taskDir := createTaskDir(t, d.cfg.WorkspacesRoot, "ws1", "active-task", nil)
	d.markActiveEnvRoot(taskDir)
	defer d.unmarkActiveEnvRoot(taskDir)

	stats := &gcStats{byPattern: map[string]int{}}
	d.gcWorkspace(context.Background(), filepath.Dir(taskDir), stats)

	if stats.skipped != 1 || stats.cleanupSkipped != 1 {
		t.Fatalf("active task counters = skipped:%d cleanup_skipped:%d, want 1/1",
			stats.skipped, stats.cleanupSkipped)
	}
}
