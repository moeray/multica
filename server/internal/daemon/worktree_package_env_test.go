package daemon

import (
	"testing"

	"github.com/multica-ai/multica/server/internal/daemon/execenv"
)

func TestApplyWorktreePackageManagerEnv(t *testing.T) {
	worktreeEnv := &execenv.Environment{LocalWorktree: &execenv.LocalWorktree{}}

	t.Run("worktree copy mode overrides a user setting", func(t *testing.T) {
		dst := map[string]string{pnpmPackageImportMethodEnv: "hardlink"}
		applyWorktreePackageManagerEnv(dst, worktreeEnv, "copy")
		if got := dst[pnpmPackageImportMethodEnv]; got != "copy" {
			t.Fatalf("%s = %q, want copy", pnpmPackageImportMethodEnv, got)
		}
	})

	t.Run("auto leaves pnpm's default alone", func(t *testing.T) {
		dst := map[string]string{}
		applyWorktreePackageManagerEnv(dst, worktreeEnv, "auto")
		if _, ok := dst[pnpmPackageImportMethodEnv]; ok {
			t.Fatalf("auto injected %s", pnpmPackageImportMethodEnv)
		}
	})

	t.Run("non-worktree tasks are unchanged", func(t *testing.T) {
		dst := map[string]string{pnpmPackageImportMethodEnv: "hardlink"}
		applyWorktreePackageManagerEnv(dst, &execenv.Environment{}, "copy")
		if got := dst[pnpmPackageImportMethodEnv]; got != "hardlink" {
			t.Fatalf("non-worktree environment changed to %q", got)
		}
	})
}
