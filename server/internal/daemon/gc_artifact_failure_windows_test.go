//go:build windows

package daemon

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/windows"

	"github.com/multica-ai/multica/server/internal/daemon/execenv"
)

func TestArtifactCleanupCountsRemoveFailures(t *testing.T) {
	t.Run("task directory", func(t *testing.T) {
		d := newGCTestDaemon(t, http.NewServeMux())
		taskDir := createTaskDir(t, d.cfg.WorkspacesRoot, "ws1", "cleanup-failure", nil)
		locked := filepath.Join(taskDir, "workdir", "payload.bin")
		writeFile(t, locked, 64)
		holdWithoutDeleteShare(t, locked)

		stats := &gcStats{byPattern: map[string]int{}}
		if removed := d.applyGCAction(context.Background(), taskDir, gcActionClean, stats); removed != 0 {
			t.Fatalf("applyGCAction removed %d directories despite the sharing violation", removed)
		}
		if stats.cleanupFailed != 1 || stats.cleanupSkipped != 0 {
			t.Fatalf("task cleanup counters = failed:%d skipped:%d, want 1/0",
				stats.cleanupFailed, stats.cleanupSkipped)
		}
	})

	t.Run("pattern artifact", func(t *testing.T) {
		d := newGCTestDaemon(t, http.NewServeMux())
		taskDir := createTaskDir(t, d.cfg.WorkspacesRoot, "ws1", "artifact-failure", nil)
		locked := filepath.Join(taskDir, "workdir", "node_modules", "locked.bin")
		writeFile(t, locked, 64)
		holdWithoutDeleteShare(t, locked)

		result := d.cleanTaskArtifactsResult(context.Background(), taskDir, []string{"node_modules"})
		if result.Failed != 1 || result.Removed != 0 {
			t.Fatalf("artifact failure result = %+v, want failed=1 removed=0", result)
		}
	})

	t.Run("managed artifact", func(t *testing.T) {
		d := newGCTestDaemon(t, http.NewServeMux())
		taskDir := createTaskDir(t, d.cfg.WorkspacesRoot, "ws1", "managed-failure", nil)
		subpath := execenv.ManagedReclaimableArtifactSubpaths()[0]
		locked := filepath.Join(taskDir, subpath, "locked.bin")
		writeFile(t, locked, 64)
		holdWithoutDeleteShare(t, locked)

		result := d.cleanManagedTaskArtifactsResult(taskDir)
		if result.Failed != 1 || result.Removed != 0 {
			t.Fatalf("managed artifact failure result = %+v, want failed=1 removed=0", result)
		}
	})
}

func holdWithoutDeleteShare(t *testing.T, path string) {
	t.Helper()
	pathPtr, err := windows.UTF16PtrFromString(path)
	if err != nil {
		t.Fatalf("UTF16PtrFromString(%q): %v", path, err)
	}
	handle, err := windows.CreateFile(
		pathPtr,
		windows.GENERIC_READ,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE,
		nil,
		windows.OPEN_EXISTING,
		windows.FILE_ATTRIBUTE_NORMAL,
		0,
	)
	if err != nil {
		t.Fatalf("CreateFile(%q): %v", path, err)
	}
	t.Cleanup(func() { _ = windows.CloseHandle(handle) })

	if err := os.Remove(path); err == nil {
		t.Skip("this Windows build allowed deleting an open file; nothing to regress against")
	}
}
