package lark

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/multica-ai/multica/server/internal/events"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

const (
	inboxTestWorkspaceID       = "2222bbbb-2222-2222-2222-222222222222"
	inboxTestMemberID          = "3333cccc-3333-3333-3333-333333333333"
	inboxTestItemID            = "4444dddd-4444-4444-4444-444444444444"
	inboxTestIssueID           = "5555eeee-5555-5555-5555-555555555555"
	inboxTestInstallation      = "1111aaaa-1111-1111-1111-111111111111"
	inboxTestAgentID           = "8888abcd-8888-8888-8888-888888888888"
	inboxTestAgentInstallation = "9999ef01-9999-9999-9999-999999999999"
	inboxTestChannelUserID     = "ou_member_1"
)

func inboxTestItem() map[string]any {
	issueID := inboxTestIssueID
	return map[string]any{
		"id":             inboxTestItemID,
		"workspace_id":   inboxTestWorkspaceID,
		"recipient_type": "member",
		"recipient_id":   inboxTestMemberID,
		"type":           "task_completed",
		"title":          "Task completed",
		"body":           "Agent finished the task.",
		"issue_id":       &issueID,
	}
}

func inboxTestEvent(item map[string]any) events.Event {
	return events.Event{
		Type:    protocol.EventInboxNew,
		Payload: map[string]any{"item": item},
	}
}

func bindInboxTestMember(t *testing.T, q *fakePatcherQueries) {
	t.Helper()
	q.memberBinding = db.ChannelUserBinding{
		InstallationID: uuidFromString(t, inboxTestInstallation),
		ChannelType:    channelTypeFeishu,
		ChannelUserID:  inboxTestChannelUserID,
	}
	q.workspace = db.Workspace{Slug: "acme"}
}

// waitForInbox blocks until every job enqueued so far has been processed.
// handleInboxNew is deliberately asynchronous, so assertions must wait for
// the worker instead of relying on the subscriber having done the work.
func waitForInbox(t *testing.T, p *Patcher) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		p.inboxPending.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for inbox delivery")
	}
}

func TestPatcherRegistersInboxNewSubscriber(t *testing.T) {
	p, _, _ := newTestPatcher(t)
	bus := events.New()
	p.Register(bus)
	if got := bus.SubscriberCount(protocol.EventInboxNew); got != 1 {
		t.Fatalf("inbox:new subscribers = %d, want 1", got)
	}
}

func TestLarkInboxNewDeliversToBoundMember(t *testing.T) {
	p, q, api := newTestPatcher(t)
	p.cfg.AppURL = "https://app.example"
	bindInboxTestMember(t, q)

	p.handleInboxNew(inboxTestEvent(inboxTestItem()))
	waitForInbox(t, p)

	if len(api.userCardSent) != 1 {
		t.Fatalf("sent %d cards, want 1", len(api.userCardSent))
	}
	if len(api.userTextSent) != 0 {
		t.Fatalf("sent %d text fallbacks for a successful card, want 0", len(api.userTextSent))
	}
	got := api.userCardSent[0]
	if got.OpenID != OpenID(inboxTestChannelUserID) {
		t.Errorf("open_id = %q, want %q", got.OpenID, inboxTestChannelUserID)
	}
	if got.InstallationID.AppID != "cli_test_app" {
		t.Errorf("app_id = %q, want cli_test_app", got.InstallationID.AppID)
	}
	if got.Title != "Task completed" {
		t.Errorf("title = %q, want Task completed", got.Title)
	}
	wantMarkdown := "Agent finished the task.\n\n[查看详情](https://app.example/acme/inbox?issue=" + inboxTestIssueID + ")"
	if got.Markdown != wantMarkdown {
		t.Errorf("markdown = %q, want %q", got.Markdown, wantMarkdown)
	}
	if got.UUID != inboxProviderUUID(inboxTestItemID, inboxTestMemberID, inboxTestInstallation) {
		t.Errorf("uuid = %q, want stable inbox idempotency key", got.UUID)
	}
	if q.agentBindingCalls != 0 {
		t.Errorf("agent binding lookups = %d, want 0 for a member-authored item", q.agentBindingCalls)
	}
}

func TestLarkInboxNewSkipsAgentRecipients(t *testing.T) {
	p, q, api := newTestPatcher(t)
	bindInboxTestMember(t, q)
	item := inboxTestItem()
	item["recipient_type"] = "agent"

	p.handleInboxNew(inboxTestEvent(item))
	waitForInbox(t, p)

	if len(api.userCardSent)+len(api.userTextSent) != 0 {
		t.Fatalf("agent recipient received %d messages, want 0", len(api.userCardSent)+len(api.userTextSent))
	}
}

func TestLarkInboxNewSkipsUnboundMember(t *testing.T) {
	p, q, api := newTestPatcher(t)
	q.memberBindingErr = pgx.ErrNoRows

	p.handleInboxNew(inboxTestEvent(inboxTestItem()))
	waitForInbox(t, p)

	if len(api.userCardSent)+len(api.userTextSent) != 0 {
		t.Fatalf("unbound member received %d messages, want 0", len(api.userCardSent)+len(api.userTextSent))
	}
}

func TestLarkInboxNewSkipsInactiveInstallation(t *testing.T) {
	p, q, api := newTestPatcher(t)
	bindInboxTestMember(t, q)
	q.installation.Status = string(InstallationRevoked)

	p.handleInboxNew(inboxTestEvent(inboxTestItem()))
	waitForInbox(t, p)

	if len(api.userCardSent)+len(api.userTextSent) != 0 {
		t.Fatalf("inactive installation sent %d messages, want 0", len(api.userCardSent)+len(api.userTextSent))
	}
}

func TestLarkInboxNewSendFailureReleasesDedupeClaim(t *testing.T) {
	p, q, api := newTestPatcher(t)
	bindInboxTestMember(t, q)
	api.userCardErr = errors.New("card endpoint down")
	api.userTextErr = errors.New("network down")
	item := inboxTestItem()

	p.handleInboxNew(inboxTestEvent(item))
	waitForInbox(t, p)
	if len(api.userCardSent) != 1 || len(api.userTextSent) != 1 {
		t.Fatalf("attempts = card:%d text:%d, want 1 each", len(api.userCardSent), len(api.userTextSent))
	}

	// A failed send must not suppress the same item for the rest of the
	// dedupe TTL.
	p.handleInboxNew(inboxTestEvent(item))
	waitForInbox(t, p)
	if len(api.userCardSent) != 2 || len(api.userTextSent) != 2 {
		t.Fatalf("attempts after retry = card:%d text:%d, want 2 each", len(api.userCardSent), len(api.userTextSent))
	}

	// A later item must still be processed after the failed send.
	next := inboxTestItem()
	next["id"] = "6666ffff-6666-6666-6666-666666666666"
	p.handleInboxNew(inboxTestEvent(next))
	waitForInbox(t, p)
	if len(api.userCardSent) != 3 || len(api.userTextSent) != 3 {
		t.Fatalf("attempts after later item = card:%d text:%d, want 3 each", len(api.userCardSent), len(api.userTextSent))
	}
}

func TestLarkInboxNewFallsBackToTextWhenCardFails(t *testing.T) {
	p, q, api := newTestPatcher(t)
	p.cfg.AppURL = "https://app.example"
	bindInboxTestMember(t, q)
	api.userCardErr = errors.New("card rejected")

	p.handleInboxNew(inboxTestEvent(inboxTestItem()))
	waitForInbox(t, p)

	if len(api.userCardSent) != 1 {
		t.Fatalf("card attempts = %d, want 1", len(api.userCardSent))
	}
	if len(api.userTextSent) != 1 {
		t.Fatalf("text fallback attempts = %d, want 1", len(api.userTextSent))
	}
	want := buildInboxText(inboxTestItem(), "acme", "https://app.example")
	if got := api.userTextSent[0].Text; got != want {
		t.Errorf("fallback text = %q, want %q", got, want)
	}
	wantUUID := inboxProviderUUID(inboxTestItemID, inboxTestMemberID, inboxTestInstallation)
	if got := api.userTextSent[0].UUID; got != wantUUID {
		t.Errorf("fallback uuid = %q, want the card's idempotency key %q", got, wantUUID)
	}
}

func TestLarkInboxNewDeduplicatesSameItem(t *testing.T) {
	p, q, api := newTestPatcher(t)
	bindInboxTestMember(t, q)
	item := inboxTestItem()

	p.handleInboxNew(inboxTestEvent(item))
	p.handleInboxNew(inboxTestEvent(item))
	waitForInbox(t, p)

	if len(api.userCardSent) != 1 || len(api.userTextSent) != 0 {
		t.Fatalf("duplicate item sent card:%d text:%d, want 1 card and 0 text", len(api.userCardSent), len(api.userTextSent))
	}

	next := inboxTestItem()
	next["id"] = "7777aaaa-7777-7777-7777-777777777777"
	p.handleInboxNew(inboxTestEvent(next))
	waitForInbox(t, p)
	if len(api.userCardSent) != 2 || len(api.userTextSent) != 0 {
		t.Fatalf("new item sent card:%d text:%d total, want 2 cards and 0 text", len(api.userCardSent), len(api.userTextSent))
	}
}

func TestLarkInboxNewPrefersActingAgentsBot(t *testing.T) {
	p, q, api := newTestPatcher(t)
	bindInboxTestMember(t, q)
	q.agentMemberBinding = db.ChannelUserBinding{
		InstallationID: uuidFromString(t, inboxTestAgentInstallation),
		ChannelType:    channelTypeFeishu,
		ChannelUserID:  "ou_agent_bot",
	}
	item := inboxTestItem()
	item["actor_type"] = "agent"
	item["actor_id"] = inboxTestAgentID

	p.handleInboxNew(inboxTestEvent(item))
	waitForInbox(t, p)

	if len(api.userCardSent) != 1 {
		t.Fatalf("sent %d cards, want 1", len(api.userCardSent))
	}
	if got := api.userCardSent[0].OpenID; got != OpenID("ou_agent_bot") {
		t.Errorf("open_id = %q, want the acting agent's bot (ou_agent_bot)", got)
	}
	if q.agentBindingCalls != 1 {
		t.Errorf("agent binding lookups = %d, want 1", q.agentBindingCalls)
	}
	if len(q.installationGetIDs) != 1 || uuidString(q.installationGetIDs[0]) != inboxTestAgentInstallation {
		t.Errorf("installation lookups = %v, want only the agent installation %s", q.installationGetIDs, inboxTestAgentInstallation)
	}
}

func TestLarkInboxNewFallsBackToRecipientWideBinding(t *testing.T) {
	p, q, api := newTestPatcher(t)
	bindInboxTestMember(t, q)
	q.agentMemberBindingErr = pgx.ErrNoRows
	item := inboxTestItem()
	item["actor_type"] = "agent"
	item["actor_id"] = inboxTestAgentID

	p.handleInboxNew(inboxTestEvent(item))
	waitForInbox(t, p)

	if len(api.userCardSent) != 1 {
		t.Fatalf("sent %d cards, want 1", len(api.userCardSent))
	}
	if got := api.userCardSent[0].OpenID; got != OpenID(inboxTestChannelUserID) {
		t.Errorf("open_id = %q, want recipient-wide binding %q", got, inboxTestChannelUserID)
	}
	if q.agentBindingCalls != 1 {
		t.Errorf("agent binding lookups = %d, want 1 before fallback", q.agentBindingCalls)
	}
}

func TestLarkInboxNewPublisherDoesNotWaitForSlowSend(t *testing.T) {
	p, q, api := newTestPatcher(t)
	bindInboxTestMember(t, q)
	started := make(chan struct{}, 1)
	block := make(chan struct{})
	api.mu.Lock()
	api.userCardStarted = started
	api.userCardBlock = block
	api.mu.Unlock()

	begin := time.Now()
	p.handleInboxNew(inboxTestEvent(inboxTestItem()))
	if elapsed := time.Since(begin); elapsed > 500*time.Millisecond {
		t.Fatalf("publisher blocked for %s while the send was in flight", elapsed)
	}

	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("worker never reached SendUserMarkdownCard")
	}
	close(block)
	waitForInbox(t, p)

	if len(api.userCardSent) != 1 {
		t.Fatalf("sent %d cards after release, want 1", len(api.userCardSent))
	}
}

func TestInboxDedupeKeyIncludesRecipientAndInstallation(t *testing.T) {
	base := inboxDedupeKey("item", "recipient", "installation")
	if base != inboxDedupeKey("item", "recipient", "installation") {
		t.Fatal("same key inputs produced different keys")
	}
	if base == inboxDedupeKey("item", "other-recipient", "installation") {
		t.Fatal("recipient must be part of the dedupe key")
	}
	if base == inboxDedupeKey("item", "recipient", "other-installation") {
		t.Fatal("installation must be part of the dedupe key")
	}
	if base == inboxDedupeKey("other-item", "recipient", "installation") {
		t.Fatal("item id must be part of the dedupe key")
	}
}

func TestInboxProviderUUIDIsStableAndScoped(t *testing.T) {
	base := inboxProviderUUID("item", "recipient", "installation")
	if len(base) != 36 {
		t.Fatalf("uuid length = %d, want 36: %q", len(base), base)
	}
	if base[14] != '5' {
		t.Errorf("uuid version nibble = %c, want 5", base[14])
	}
	if !strings.ContainsRune("89ab", rune(base[19])) {
		t.Errorf("uuid variant nibble = %c, want one of 89ab", base[19])
	}
	if base != inboxProviderUUID("item", "recipient", "installation") {
		t.Fatal("same inputs must produce the same uuid")
	}
	if base == inboxProviderUUID("item", "other-recipient", "installation") {
		t.Fatal("recipient must be part of the provider uuid")
	}
	if base == inboxProviderUUID("item", "recipient", "other-installation") {
		t.Fatal("installation must be part of the provider uuid")
	}
}

func TestBuildInboxCard(t *testing.T) {
	item := inboxTestItem()

	t.Run("card content", func(t *testing.T) {
		got := buildInboxCard(item, "acme", "https://app.example/")
		if got.Title != "Task completed" {
			t.Errorf("title = %q, want Task completed", got.Title)
		}
		if got.Summary != "Task completed" {
			t.Errorf("summary = %q, want Task completed", got.Summary)
		}
		wantMarkdown := "Agent finished the task.\n\n[查看详情](https://app.example/acme/inbox?issue=" + inboxTestIssueID + ")"
		if got.Markdown != wantMarkdown {
			t.Errorf("markdown = %q, want %q", got.Markdown, wantMarkdown)
		}
	})

	t.Run("schema 2.0 envelope", func(t *testing.T) {
		raw, err := renderInboxCard(buildInboxCard(item, "acme", "https://app.example"), inboxCardMaxBytes)
		if err != nil {
			t.Fatalf("render card: %v", err)
		}
		var doc map[string]any
		if err := json.Unmarshal(raw, &doc); err != nil {
			t.Fatalf("card is not valid JSON: %v", err)
		}
		if doc["schema"] != "2.0" {
			t.Errorf("schema = %v, want 2.0", doc["schema"])
		}
		header, _ := doc["header"].(map[string]any)
		title, _ := header["title"].(map[string]any)
		if title["content"] != "Task completed" {
			t.Errorf("header title = %v, want Task completed", title["content"])
		}
		body, _ := doc["body"].(map[string]any)
		elements, _ := body["elements"].([]any)
		if len(elements) != 1 {
			t.Fatalf("body elements = %d, want 1", len(elements))
		}
		element, _ := elements[0].(map[string]any)
		if element["tag"] != "markdown" {
			t.Errorf("body element tag = %v, want markdown", element["tag"])
		}
		content, _ := element["content"].(string)
		if !strings.Contains(content, "[查看详情](https://app.example/acme/inbox?issue="+inboxTestIssueID+")") {
			t.Errorf("markdown body is missing the detail link: %q", content)
		}
		config, _ := doc["config"].(map[string]any)
		summary, _ := config["summary"].(map[string]any)
		if summary["content"] != "Task completed" {
			t.Errorf("summary = %v, want Task completed", summary["content"])
		}
	})

	t.Run("no app url drops link", func(t *testing.T) {
		got := buildInboxCard(item, "acme", "")
		if strings.Contains(got.Markdown, "http") {
			t.Errorf("card should not contain a link: %q", got.Markdown)
		}
	})

	t.Run("missing slug drops link", func(t *testing.T) {
		got := buildInboxCard(item, "", "https://app.example")
		if strings.Contains(got.Markdown, "http") {
			t.Errorf("card should not contain a broken link: %q", got.Markdown)
		}
	})

	t.Run("empty title uses first body line", func(t *testing.T) {
		untitled := inboxTestItem()
		untitled["title"] = ""
		got := buildInboxCard(untitled, "acme", "https://app.example")
		if got.Title != "Agent finished the task." {
			t.Errorf("title = %q, want first body line", got.Title)
		}
		if strings.Contains(got.Title, "task_completed") {
			t.Errorf("card should not expose the raw notification type: %q", got.Title)
		}
	})

	t.Run("chat item has no issue query", func(t *testing.T) {
		chatItem := inboxTestItem()
		delete(chatItem, "issue_id")
		got := buildInboxCard(chatItem, "acme", "https://app.example")
		if !strings.Contains(got.Markdown, "(https://app.example/acme/inbox)") {
			t.Errorf("chat link = %q, want no issue query", got.Markdown)
		}
	})

	t.Run("body is not capped at the fallback excerpt length", func(t *testing.T) {
		longItem := inboxTestItem()
		longItem["body"] = strings.Repeat("x", inboxBodyMaxLen+200)
		got := buildInboxCard(longItem, "acme", "")
		if len([]rune(got.Markdown)) != inboxBodyMaxLen+200 {
			t.Errorf("card body runes = %d, want the full %d", len([]rune(got.Markdown)), inboxBodyMaxLen+200)
		}
	})
}

func TestRenderInboxCardTruncatesToByteBudget(t *testing.T) {
	item := inboxTestItem()
	item["body"] = strings.Repeat("长", 20000)

	raw, err := renderInboxCard(buildInboxCard(item, "acme", "https://app.example"), inboxCardMaxBytes)
	if err != nil {
		t.Fatalf("render oversized card: %v", err)
	}
	if len(raw) > inboxCardMaxBytes {
		t.Fatalf("card encoded to %d bytes, want <= %d", len(raw), inboxCardMaxBytes)
	}
	if !json.Valid(raw) {
		t.Fatal("truncated card is not valid JSON")
	}

	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("decode truncated card: %v", err)
	}
	body, _ := doc["body"].(map[string]any)
	elements, _ := body["elements"].([]any)
	if len(elements) != 1 {
		t.Fatalf("body elements = %d, want 1", len(elements))
	}
	element, _ := elements[0].(map[string]any)
	content, _ := element["content"].(string)
	if !strings.HasSuffix(content, "[查看详情](https://app.example/acme/inbox?issue="+inboxTestIssueID+")") {
		t.Errorf("truncated card must keep the detail link at the end: %q", content)
	}
	if !strings.Contains(content, "\u2026") {
		t.Errorf("truncated card body should end with an ellipsis: %q", content)
	}
	if len([]rune(content)) <= inboxBodyMaxLen {
		t.Errorf("truncated card body = %d runes, want more than the fallback limit %d", len([]rune(content)), inboxBodyMaxLen)
	}
}

func TestBuildInboxText(t *testing.T) {
	item := inboxTestItem()

	t.Run("full message", func(t *testing.T) {
		got := buildInboxText(item, "acme", "https://app.example/")
		want := "Task completed\nAgent finished the task.\nhttps://app.example/acme/inbox?issue=" + inboxTestIssueID
		if got != want {
			t.Errorf("got %q, want %q", got, want)
		}
	})

	t.Run("no app url drops link", func(t *testing.T) {
		got := buildInboxText(item, "acme", "")
		if strings.Contains(got, "http") {
			t.Errorf("text should not contain a link: %q", got)
		}
	})

	t.Run("missing slug drops link", func(t *testing.T) {
		got := buildInboxText(item, "", "https://app.example")
		if strings.Contains(got, "http") {
			t.Errorf("text should not contain a broken link: %q", got)
		}
	})

	t.Run("empty title does not fall back to raw type", func(t *testing.T) {
		untitled := inboxTestItem()
		untitled["title"] = ""
		got := buildInboxText(untitled, "acme", "https://app.example")
		if strings.Contains(got, "task_completed") {
			t.Errorf("text should not expose the raw notification type: %q", got)
		}
		if !strings.HasPrefix(got, "Agent finished the task.") {
			t.Errorf("text should start with the body: %q", got)
		}
	})

	t.Run("chat item has no issue query", func(t *testing.T) {
		chatItem := inboxTestItem()
		delete(chatItem, "issue_id")
		got := buildInboxText(chatItem, "acme", "https://app.example")
		if !strings.HasSuffix(got, "https://app.example/acme/inbox") {
			t.Errorf("chat link = %q, want no issue query", got)
		}
	})

	t.Run("long body truncated with ellipsis", func(t *testing.T) {
		longItem := inboxTestItem()
		longItem["body"] = strings.Repeat("x", inboxBodyMaxLen+50)
		got := buildInboxText(longItem, "acme", "")
		if strings.Count(got, "x") != inboxBodyMaxLen-1 {
			t.Errorf("body runes = %d, want %d", strings.Count(got, "x"), inboxBodyMaxLen-1)
		}
		if !strings.HasSuffix(got, "\u2026") {
			t.Errorf("truncated body should end with an ellipsis: %q", got)
		}
	})
}
