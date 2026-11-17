package agent

import (
	"encoding/json"
	"testing"
)

func TestSplitCodexModelSelector(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name      string
		in        string
		wantModel string
		wantProv  string
	}{
		{"bare official id", "gpt-6.1-sol", "gpt-6.1-sol", ""},
		{"empty", "", "", ""},
		// "openai" must be sent explicitly: config.toml may default to wb-proxy,
		// and an empty provider would inherit that default (verified against
		// app-server 0.169).
		{"openai prefix explicit", "openai/gpt-6.1-sol", "gpt-6.1-sol", "openai"},
		{"wb prefix routes locally", "wb-proxy/hy4std", "hy4std", "wb-proxy"},
		{"wb prefix with cn model", "wb-proxy/glm-5.3-flash", "glm-5.3-flash", "wb-proxy"},
		{"unknown prefix untouched", "some-gateway/foo", "some-gateway/foo", ""},
		{"trailing slash untouched", "wb-proxy/", "wb-proxy/", ""},
		{"leading slash untouched", "/hy4std", "/hy4std", ""},
		{"nested slash untouched", "a/b/c", "a/b/c", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gotModel, gotProv := splitCodexModelSelector(tc.in)
			if gotModel != tc.wantModel || gotProv != tc.wantProv {
				t.Fatalf("splitCodexModelSelector(%q) = (%q, %q), want (%q, %q)",
					tc.in, gotModel, gotProv, tc.wantModel, tc.wantProv)
			}
		})
	}
}

func TestCodexCatalogProvider(t *testing.T) {
	t.Parallel()
	cases := []struct{ in, want string }{
		{"gpt-6.1-sol", "openai"},
		{"", "openai"},
		{"openai/gpt-6.1-sol", "openai"},
		{"wb-proxy/hy4std", "wb-proxy"},
		{"wb-proxy/glm-5.3-flash", "wb-proxy"},
		{"wb-proxy/", "openai"},
		{"/hy4std", "openai"},
		// Unknown prefixes must NOT become a picker group: the launcher would
		// not route them, so displaying them as a channel is a lie.
		{"some-gateway/foo", "openai"},
	}
	for _, tc := range cases {
		if got := codexCatalogProvider(tc.in); got != tc.want {
			t.Fatalf("codexCatalogProvider(%q) = %q, want %q",
				tc.in, got, tc.want)
		}
	}
}

func TestCodexProviderPrefixConsistency(t *testing.T) {
	t.Parallel()
	// Every prefix the catalog exposes as a group must also be routed by the
	// launcher, and vice versa. This is the guard against the two helpers
	// drifting apart again.
	ids := []string{"openai/gpt-6.1-sol", "wb-proxy/hy4std",
		"wb-proxy/glm-5.3-flash", "gpt-6.1-sol", "some-gateway/foo"}
	for _, id := range ids {
		prefix, _, known := codexProviderPrefix(id)
		_, routed := splitCodexModelSelector(id)
		group := codexCatalogProvider(id)
		if known && routed != prefix {
			t.Fatalf("%q: group=%q but routed provider=%q", id, prefix, routed)
		}
		if !known && group != "openai" {
			t.Fatalf("%q: unknown prefix grouped as %q", id, group)
		}
	}
}

func TestExtractThreadProvider(t *testing.T) {
	t.Parallel()
	cases := []struct {
		raw  string
		want string
	}{
		{`{"thread":{"id":"t1","modelProvider":"wb-proxy"}}`, "wb-proxy"},
		{`{"thread":{"id":"t1","modelProvider":"openai"}}`, "openai"},
		{`{"thread":{"id":"t1"}}`, ""},
		{`not json`, ""},
	}
	for _, tc := range cases {
		if got := extractThreadProvider(json.RawMessage(tc.raw)); got != tc.want {
			t.Fatalf("extractThreadProvider(%q) = %q, want %q",
				tc.raw, got, tc.want)
		}
	}
}

// A verified (non-fallback) catalog built from the merged model file must
// still validate a thinking level for provider-qualified ids. This is the
// path the daemon uses at task time; a regression here would silently drop
// the agent's effort for every namespaced model.
func TestThinkingLevelForQualifiedCodexModels(t *testing.T) {
	t.Parallel()
	catalog := Catalog{Models: []Model{
		{ID: "openai/gpt-6.1-sol", Provider: "openai",
			Thinking: &ModelThinking{SupportedLevels: []ThinkingLevel{
				{Value: "low"}, {Value: "high"}, {Value: "xhigh"},
			}}},
		{ID: "wb-proxy/hy4std", Provider: "wb-proxy",
			Thinking: &ModelThinking{SupportedLevels: []ThinkingLevel{
				{Value: "low"}, {Value: "high"},
			}}},
	}}
	load := func() (Catalog, error) { return catalog, nil }
	cases := []struct {
		model string
		level string
		want  bool
	}{
		{"openai/gpt-6.1-sol", "high", true},
		{"openai/gpt-6.1-sol", "xhigh", true},
		{"wb-proxy/hy4std", "high", true},
		{"wb-proxy/hy4std", "ultra", false},
		{"openai/gpt-6.1-sol", "ultra", false},
	}
	for _, tc := range cases {
		got, err := ValidateThinkingLevelWith(load, "codex", tc.model, tc.level)
		if err != nil {
			t.Fatalf("%s/%s: unexpected error: %v", tc.model, tc.level, err)
		}
		if got != tc.want {
			t.Fatalf("%s/%s = %v, want %v", tc.model, tc.level, got, tc.want)
		}
	}
}

func TestParseCodexModelCatalogProviderGrouping(t *testing.T) {
	t.Parallel()
	raw := []byte(`{"models":[
		{"slug":"openai/gpt-6.1-sol","display_name":"GPT-6.1 Sol [official]","visibility":"list"},
		{"slug":"wb-proxy/hy4std","display_name":"hy4std [wb]","visibility":"list"},
		{"slug":"gpt-6-astra","display_name":"GPT-6 Astra","visibility":"list"},
		{"slug":"hidden","display_name":"Hidden","visibility":"hide"}
	]}`)
	models, err := parseCodexModelCatalog(raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(models) != 3 {
		t.Fatalf("got %d models, want 3 (hidden skipped)", len(models))
	}
	want := map[string]string{
		"openai/gpt-6.1-sol": "openai",
		"wb-proxy/hy4std":    "wb-proxy",
		"gpt-6-astra":        "openai",
	}
	for _, m := range models {
		if want[m.ID] != m.Provider {
			t.Fatalf("model %q provider = %q, want %q",
				m.ID, m.Provider, want[m.ID])
		}
	}
}
