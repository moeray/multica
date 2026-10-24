package daemon

import (
	"strings"
	"testing"

	"github.com/multica-ai/multica/server/internal/daemon/execenv"
)

func TestApplyWorktreePackageManagerEnv(t *testing.T) {
	worktreeEnv := &execenv.Environment{LocalWorktree: &execenv.LocalWorktree{}}

	for name, key := range map[string]string{
		"lowercase": pnpmPackageImportMethodEnv,
		"uppercase": "NPM_CONFIG_PACKAGE_IMPORT_METHOD",
		"mixedcase": "Npm_Config_Package_Import_Method",
	} {
		t.Run("copy overrides "+name, func(t *testing.T) {
			dst := map[string]string{
				key:                                "hardlink",
				"NPM_CONFIG_PACKAGE_IMPORT_METHOD": "clone",
				"Npm_Config_Package_Import_Method": "hardlink",
			}
			applyWorktreePackageManagerEnv(dst, worktreeEnv, "copy")
			if got := dst[pnpmPackageImportMethodEnv]; got != "copy" {
				t.Fatalf("%s = %q, want copy", pnpmPackageImportMethodEnv, got)
			}
			var matches []string
			for candidate := range dst {
				if strings.EqualFold(candidate, pnpmPackageImportMethodEnv) {
					matches = append(matches, candidate)
				}
			}
			if len(matches) != 1 || matches[0] != pnpmPackageImportMethodEnv {
				t.Fatalf("case-insensitive keys after injection = %v, want only %q", matches, pnpmPackageImportMethodEnv)
			}
		})
	}

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
