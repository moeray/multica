package daemon

import (
	"strings"

	"github.com/multica-ai/multica/server/internal/daemon/execenv"
)

// pnpmPackageImportMethodEnv is pnpm's npm-config environment spelling of
// package-import-method. Keeping it in the agent environment rather than a
// repository .npmrc means the policy applies only to disposable task
// worktrees and never changes the user's own checkout.
const pnpmPackageImportMethodEnv = "npm_config_package_import_method"

// applyWorktreePackageManagerEnv pins pnpm's package import mode for a task
// worktree. "auto" deliberately leaves the environment untouched so pnpm keeps
// its platform default; the value is applied after agent custom_env so a user
// setting cannot reintroduce shared hardlinks on a worktree.
func applyWorktreePackageManagerEnv(dst map[string]string, env *execenv.Environment, method string) {
	if dst == nil || env == nil || env.LocalWorktree == nil {
		return
	}
	method = strings.ToLower(strings.TrimSpace(method))
	if method == "" || method == "auto" {
		return
	}
	dst[pnpmPackageImportMethodEnv] = method
}
