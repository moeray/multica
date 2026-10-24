package daemon

import (
	"strings"
	"testing"

	"github.com/multica-ai/multica/server/pkg/protocol"
)

// mirroredPromptMarker is the heading buildMirroredChatMessagesBlock opens
// with. Its presence is how every test below tells "injected" from "absent".
const mirroredPromptMarker = "[Messages mirrored into this chat since your last turn]"

func reviewResultMessage(passed bool, body, issueRef string) ChatContextMessage {
	return ChatContextMessage{
		ID:           "019ec09d-0000-7000-8000-000000000001",
		MessageKind:  protocol.ChatMessageKindReviewResult,
		ReviewPassed: &passed,
		Content:      protocol.BuildReviewResultContent(passed, body, issueRef),
	}
}

// TestChatPromptInjectsMirroredReviewResult is acceptance criterion 1: after a
// review result lands in the transcript, the next turn's prompt carries it —
// structurally (kind, verdict, issue) and above the member's own words, so the
// agent can resolve a reference to it without a history read.
func TestChatPromptInjectsMirroredReviewResult(t *testing.T) {
	t.Parallel()

	out := BuildPrompt(Task{
		ChatSessionID: "sess-1",
		ChatMessage:   "刚才那条评审结果里还差什么？",
		ChatContextMessages: []ChatContextMessage{
			reviewResultMessage(true, "阻塞项已修复；剩余两条是建议。", "MP-67"),
		},
	}, "claude")

	for _, want := range []string{
		mirroredPromptMarker,
		"- review_result · passed=true · issue=MP-67",
		"  评审结果：通过",
		"  阻塞项已修复；剩余两条是建议。",
		"User message:\n刚才那条评审结果里还差什么？",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("prompt missing %q\n--- output ---\n%s", want, out)
		}
	}

	// The mirror must read as context for this turn, never as the member's
	// message: it has to precede "User message:" and be introduced as
	// platform-written.
	mirrorAt := strings.Index(out, mirroredPromptMarker)
	userAt := strings.Index(out, "User message:")
	if mirrorAt < 0 || userAt < 0 || mirrorAt > userAt {
		t.Fatalf("mirror block must precede the user message (mirror@%d, user@%d)\n--- output ---\n%s", mirrorAt, userAt, out)
	}
	if !strings.Contains(out, "the person did not type them") {
		t.Errorf("mirror block does not say who wrote it\n--- output ---\n%s", out)
	}
}

// TestChatPromptWithoutMirrorsAddsNothing is acceptance criterion 2. The only
// change to the prompt on a turn with no mirrors is one WriteString(""), so
// proving the block renders empty proves the turn is byte-identical to what
// this code produced before the feature: nothing is inserted, removed or
// reordered.
func TestChatPromptWithoutMirrorsAddsNothing(t *testing.T) {
	t.Parallel()

	task := Task{ChatSessionID: "sess-1", ChatMessage: "hi"}
	base := BuildPrompt(task, "claude")

	if block := buildMirroredChatMessagesBlock(nil); block != "" {
		t.Fatalf("nil messages rendered %q, want empty", block)
	}
	if block := buildMirroredChatMessagesBlock([]ChatContextMessage{}); block != "" {
		t.Fatalf("empty messages rendered %q, want empty", block)
	}
	if strings.Contains(base, mirroredPromptMarker) {
		t.Fatalf("prompt without mirrors carries the mirror heading:\n%s", base)
	}

	// And the identity holds end to end for a nil vs empty slice.
	withEmpty := task
	withEmpty.ChatContextMessages = []ChatContextMessage{}
	if got := BuildPrompt(withEmpty, "claude"); got != base {
		t.Fatalf("empty slice changed the prompt\n--- want ---\n%s\n--- got ---\n%s", base, got)
	}
}

// TestChatPromptTruncatesMirroredBody is acceptance criterion 3: a long review
// costs a bounded excerpt, and the parts a reference depends on — the verdict
// and the issue — survive the cut.
func TestChatPromptTruncatesMirroredBody(t *testing.T) {
	t.Parallel()

	body := strings.Repeat("这是一条很长的评审正文。", 400) // 4000 runes
	out := BuildPrompt(Task{
		ChatSessionID:       "sess-1",
		ChatMessage:         "继续",
		ChatContextMessages: []ChatContextMessage{reviewResultMessage(false, body, "MP-68")},
	}, "claude")

	if !strings.Contains(out, "  评审结果：不通过") {
		t.Fatalf("truncation dropped the conclusion:\n%s", out)
	}
	if !strings.Contains(out, "issue=MP-68") {
		t.Fatalf("truncation dropped the issue reference:\n%s", out)
	}
	if !strings.Contains(out, "…") {
		t.Fatalf("expected a truncation ellipsis in:\n%s", out)
	}
	if strings.Count(out, "这是一条很长的评审正文。") > maxMirroredMessageBodyRunes {
		t.Fatalf("body was not bounded to %d runes", maxMirroredMessageBodyRunes)
	}
	if !strings.Contains(out, "multica chat history") {
		t.Fatalf("truncated prompt does not say where the full text is:\n%s", out)
	}
}

// TestChatPromptKeepsNewestMirrors is the budget's other half: when more
// messages are stamped than the section can hold, the NEWEST survive — the
// member says "刚才那条", and that is the one that must still be there.
func TestChatPromptKeepsOnlyNewestMirrorsUnderBudget(t *testing.T) {
	t.Parallel()

	messages := make([]ChatContextMessage, 0, 6)
	for i := 0; i < 6; i++ {
		messages = append(messages, reviewResultMessage(true,
			strings.Repeat("旧评审意见。", 300), "MP-6"+string(rune('0'+i))))
	}
	block := buildMirroredChatMessagesBlock(messages)

	if !strings.Contains(block, "MP-65") {
		t.Fatalf("newest message (MP-65) was dropped:\n%s", block)
	}
	if strings.Contains(block, "MP-60") {
		t.Fatalf("oldest message (MP-60) survived the budget:\n%s", block)
	}
	if !strings.Contains(block, "Truncated") {
		t.Fatalf("dropping messages without saying so:\n%s", block)
	}
	if got := len([]rune(block)); got > maxMirroredContextRunes+400 {
		t.Fatalf("block is %d runes, over the %d-rune section budget plus its heading", got, maxMirroredContextRunes)
	}
}

// TestChatPromptRendersOtherMirrorKinds keeps the renderer structural rather
// than review-specific: a future mirror kind arrives with the same field and
// must render its content without a verdict or an issue reference invented for
// it.
func TestChatPromptRendersOtherMirrorKinds(t *testing.T) {
	t.Parallel()

	out := BuildPrompt(Task{
		ChatSessionID: "sess-1",
		ChatMessage:   "好",
		ChatContextMessages: []ChatContextMessage{
			{ID: "m-1", MessageKind: "status_note", Content: "构建已完成。"},
			{ID: "m-2", Content: "没有 kind 的旧行"},
		},
	}, "claude")

	for _, want := range []string{"- status_note", "  构建已完成。", "- message", "  没有 kind 的旧行"} {
		if !strings.Contains(out, want) {
			t.Errorf("prompt missing %q\n--- output ---\n%s", want, out)
		}
	}
	if strings.Contains(out, "passed=") || strings.Contains(out, "issue=") {
		t.Errorf("rendered verdict/issue for a message that has neither\n--- output ---\n%s", out)
	}
}

func TestTruncateRunesIsMultibyteSafe(t *testing.T) {
	t.Parallel()

	got, cut := truncateRunes("评审结果：通过", 5)
	if !cut {
		t.Fatal("expected the string to be reported as truncated")
	}
	if got != "评审结果："+"…" {
		t.Fatalf("truncateRunes = %q", got)
	}
	if _, cut := truncateRunes("short", 5); cut {
		t.Fatal("a string at the limit must not be reported as truncated")
	}
}
