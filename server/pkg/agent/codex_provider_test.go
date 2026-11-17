package agent

import "testing"

func TestSplitCodexModelSelector(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name       string
		in         string
		wantModel  string
		wantProv   string
	}{
		{"bare official id", "gpt-6.1-sol", "gpt-6.1-sol", ""},
		{"empty", "", "", ""},
		{"openai prefix folds to default", "openai/gpt-6.1-sol", "gpt-6.1-sol", ""},
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
		{"wb-proxy/", "wb-proxy"},
		{"/hy4std", "openai"},
	}
	for _, tc := range cases {
		if got := codexCatalogProvider(tc.in); got != tc.want {
			t.Fatalf("codexCatalogProvider(%q) = %q, want %q",
				tc.in, got, tc.want)
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
