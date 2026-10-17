package lark

import (
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/multica-ai/multica/server/internal/events"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

const (
	inboxTestWorkspaceID   = "2222bbbb-2222-2222-2222-222222222222"
	inboxTestMemberID      = "3333cccc-3333-3333-3333-333333333333"
	inboxTestItemID        = "4444dddd-4444-4444-4444-444444444444"
	inboxTestIssueID       = "5555eeee-5555-5555-5555-555555555555"
	inboxTestInstallation  = "1111aaaa-1111-1111-1111-111111111111"
	inboxTestChannelUserID = "ou_member_1"
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

	if len(api.userTextSent) != 1 {
		t.Fatalf("sent %d messages, want 1", len(api.userTextSent))
	}
	got := api.userTextSent[0]
	if got.OpenID != OpenID(inboxTestChannelUserID) {
		t.Errorf("open_id = %q, want %q", got.OpenID, inboxTestChannelUserID)
	}
	if got.InstallationID.AppID != "cli_test_app" {
		t.Errorf("app_id = %q, want cli_test_app", got.InstallationID.AppID)
	}
	want := "Task completed\nAgent finished the task.\nhttps://app.example/acme/inbox?issue=" + inboxTestIssueID
	if got.Text != want {
		t.Errorf("text = %q, want %q", got.Text, want)
	}
}

func TestLarkInboxNewSkipsAgentRecipients(t *testing.T) {
	p, q, api := newTestPatcher(t)
	bindInboxTestMember(t, q)
	item := inboxTestItem()
	item["recipient_type"] = "agent"

	p.handleInboxNew(inboxTestEvent(item))

	if len(api.userTextSent) != 0 {
		t.Fatalf("agent recipient received %d messages, want 0", len(api.userTextSent))
	}
}

func TestLarkInboxNewSkipsUnboundMember(t *testing.T) {
	p, q, api := newTestPatcher(t)
	q.memberBindingErr = pgx.ErrNoRows

	p.handleInboxNew(inboxTestEvent(inboxTestItem()))

	if len(api.userTextSent) != 0 {
		t.Fatalf("unbound member received %d messages, want 0", len(api.userTextSent))
	}
}

func TestLarkInboxNewSkipsInactiveInstallation(t *testing.T) {
	p, q, api := newTestPatcher(t)
	bindInboxTestMember(t, q)
	q.installation.Status = string(InstallationRevoked)

	p.handleInboxNew(inboxTestEvent(inboxTestItem()))

	if len(api.userTextSent) != 0 {
		t.Fatalf("inactive installation sent %d messages, want 0", len(api.userTextSent))
	}
}

func TestLarkInboxNewSendFailureDoesNotPanic(t *testing.T) {
	p, q, api := newTestPatcher(t)
	bindInboxTestMember(t, q)
	api.userTextErr = errors.New("network down")

	p.handleInboxNew(inboxTestEvent(inboxTestItem()))

	if len(api.userTextSent) != 1 {
		t.Fatalf("attempts = %d, want 1", len(api.userTextSent))
	}

	// A later item must still be processed after the failed send.
	next := inboxTestItem()
	next["id"] = "6666ffff-6666-6666-6666-666666666666"
	p.handleInboxNew(inboxTestEvent(next))
	if len(api.userTextSent) != 2 {
		t.Fatalf("attempts after later item = %d, want 2", len(api.userTextSent))
	}
}

func TestLarkInboxNewDeduplicatesSameItem(t *testing.T) {
	p, q, api := newTestPatcher(t)
	bindInboxTestMember(t, q)
	item := inboxTestItem()

	p.handleInboxNew(inboxTestEvent(item))
	p.handleInboxNew(inboxTestEvent(item))

	if len(api.userTextSent) != 1 {
		t.Fatalf("duplicate item sent %d messages, want 1", len(api.userTextSent))
	}

	next := inboxTestItem()
	next["id"] = "7777aaaa-7777-7777-7777-777777777777"
	p.handleInboxNew(inboxTestEvent(next))
	if len(api.userTextSent) != 2 {
		t.Fatalf("new item sent %d messages total, want 2", len(api.userTextSent))
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

func TestBuildInboxText(t *testing.T) {
	item := inboxTestItem()

	t.Run("full message", func(t *testing.T) {
		got := buildInboxText(item, inboxTestWorkspaceID, "acme", "https://app.example/")
		want := "Task completed\nAgent finished the task.\nhttps://app.example/acme/inbox?issue=" + inboxTestIssueID
		if got != want {
			t.Errorf("got %q, want %q", got, want)
		}
	})

	t.Run("no app url drops link", func(t *testing.T) {
		got := buildInboxText(item, inboxTestWorkspaceID, "acme", "")
		if strings.Contains(got, "http") {
			t.Errorf("text should not contain a link: %q", got)
		}
	})

	t.Run("workspace id fallback", func(t *testing.T) {
		got := buildInboxText(item, inboxTestWorkspaceID, "", "https://app.example")
		if !strings.Contains(got, "/"+inboxTestWorkspaceID+"/inbox") {
			t.Errorf("link should fall back to workspace id: %q", got)
		}
	})

	t.Run("chat item has no issue query", func(t *testing.T) {
		chatItem := inboxTestItem()
		delete(chatItem, "issue_id")
		got := buildInboxText(chatItem, inboxTestWorkspaceID, "acme", "https://app.example")
		if !strings.HasSuffix(got, "https://app.example/acme/inbox") {
			t.Errorf("chat link = %q, want no issue query", got)
		}
	})

	t.Run("long body truncated", func(t *testing.T) {
		longItem := inboxTestItem()
		longItem["body"] = strings.Repeat("x", inboxBodyMaxLen+50)
		got := buildInboxText(longItem, inboxTestWorkspaceID, "acme", "")
		if strings.Count(got, "x") != inboxBodyMaxLen {
			t.Errorf("body runes = %d, want %d", strings.Count(got, "x"), inboxBodyMaxLen)
		}
	})
}
