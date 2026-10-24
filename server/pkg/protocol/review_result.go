package protocol

import "strings"

// The review_result transcript body has three sections, separated by blank
// lines: the conclusion, the reviewer's body, and the trailing issue reference.
// Both sides of the mirror injection read that layout — the producer
// (internal/service/agent_message.go) builds it, and the daemon's per-turn
// prompt renders the conclusion and the reference structurally instead of
// copying the body — so the shape lives here rather than in either caller
// (MP-68).
const reviewResultSectionSeparator = "\n\n"

// ReviewResultVerdict renders the one-line conclusion of a review.
func ReviewResultVerdict(passed bool) string {
	if passed {
		return "评审结果：通过"
	}
	return "评审结果：不通过"
}

// BuildReviewResultContent assembles the transcript body of one review_result
// message. issueRef is the trailing reference (a bare identifier or a Markdown
// link) and stays on its own line so SplitReviewResultContent can find it.
func BuildReviewResultContent(passed bool, body, issueRef string) string {
	return ReviewResultVerdict(passed) +
		reviewResultSectionSeparator + strings.TrimSpace(body) +
		reviewResultSectionSeparator + strings.TrimSpace(issueRef)
}

// SplitReviewResultContent is BuildReviewResultContent's inverse. The first
// section is the conclusion and the last one the issue reference; everything
// between them is the reviewer's body (which may itself contain blank lines).
// Content that does not carry all three sections returns what it does have:
// the whole text as the conclusion, with empty body and reference.
func SplitReviewResultContent(content string) (conclusion, body, issueRef string) {
	sections := strings.Split(strings.TrimSpace(content), reviewResultSectionSeparator)
	switch len(sections) {
	case 0:
		return "", "", ""
	case 1:
		return strings.TrimSpace(sections[0]), "", ""
	case 2:
		return strings.TrimSpace(sections[0]), "", strings.TrimSpace(sections[1])
	default:
		return strings.TrimSpace(sections[0]),
			strings.TrimSpace(strings.Join(sections[1:len(sections)-1], reviewResultSectionSeparator)),
			strings.TrimSpace(sections[len(sections)-1])
	}
}
