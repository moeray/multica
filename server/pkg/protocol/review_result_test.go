package protocol

import (
	"strings"
	"testing"
)

// TestBuildReviewResultContentRoundTrips pins the layout the two sides of the
// mirror injection depend on: the producer writes it, the daemon's per-turn
// prompt reads the verdict and the issue reference back out of it. A body with
// blank lines of its own must survive the trip, since reviewers write
// multi-paragraph conclusions.
func TestBuildReviewResultContentRoundTrips(t *testing.T) {
	t.Parallel()

	body := "阻塞项：缺少权限测试。\n\n建议：\n\n- 补一个报错路径用例\n- 收敛错误码"
	content := BuildReviewResultContent(false, body, "[MP-68](https://app.example/mp/issues/MP-68)")

	verdict, gotBody, issueRef := SplitReviewResultContent(content)
	if verdict != "评审结果：不通过" {
		t.Errorf("verdict = %q", verdict)
	}
	if gotBody != body {
		t.Errorf("body = %q, want %q", gotBody, body)
	}
	if issueRef != "[MP-68](https://app.example/mp/issues/MP-68)" {
		t.Errorf("issueRef = %q", issueRef)
	}
}

func TestSplitReviewResultContentToleratesShapesWithoutAllSections(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		content string
		verdict string
		body    string
		issue   string
	}{
		{name: "empty", content: "   ", verdict: "", body: "", issue: ""},
		{name: "single section", content: "只有一行", verdict: "只有一行"},
		{name: "two sections", content: "结论\n\nMP-1", verdict: "结论", issue: "MP-1"},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			verdict, body, issue := SplitReviewResultContent(tc.content)
			if verdict != tc.verdict || body != tc.body || issue != tc.issue {
				t.Fatalf("SplitReviewResultContent(%q) = (%q, %q, %q), want (%q, %q, %q)",
					tc.content, verdict, body, issue, tc.verdict, tc.body, tc.issue)
			}
		})
	}
}

// TestBuildReviewResultContentMatchesLegacyLayout guards the message the
// transcript, the UI and the channel already received before this helper
// existed: the refactor must not change a single byte of it.
func TestBuildReviewResultContentMatchesLegacyLayout(t *testing.T) {
	t.Parallel()

	got := BuildReviewResultContent(true, "阻塞项已修复。", "MP-67")
	want := "评审结果：通过\n\n阻塞项已修复。\n\nMP-67"
	if got != want {
		t.Fatalf("content = %q, want %q", got, want)
	}
	if !strings.HasPrefix(got, "评审结果：通过") {
		t.Fatalf("verdict is no longer the first line: %q", got)
	}
}
