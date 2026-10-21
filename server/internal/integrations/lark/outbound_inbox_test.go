package lark

import (
	"context"
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
	inboxTestWorkspaceID  = "2222bbbb-2222-2222-2222-222222222222"
	inboxTestMemberID     = "3333cccc-3333-3333-3333-333333333333"
	inboxTestItemID       = "4444dddd-4444-4444-4444-444444444444"
	inboxTestIssueID      = "5555eeee-5555-5555-5555-555555555555"
	inboxTestInstallation = "1111aaaa-1111-1111-1111-111111111111"
	inboxTestOtherInstall = "9999ef01-9999-9999-9999-999999999999"
	inboxTestChannelUser  = "ou_member_1"
	inboxTestOtherUser    = "ou_other_bot"
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

func bindInboxTestDestination(t *testing.T, q *fakePatcherQueries, installationID, channelUserID string) {
	t.Helper()
	wsID := uuidFromString(t, inboxTestWorkspaceID)
	memberID := uuidFromString(t, inboxTestMemberID)
	instID := uuidFromString(t, installationID)
	q.mu.Lock()
	q.inboxDestinations[inboxMemberKey(wsID, memberID)] = db.InboxLarkDestination{
		ID:             uuidFromString(t, "6666ffff-6666-6666-6666-666666666666"),
		WorkspaceID:    wsID,
		MemberID:       memberID,
		InstallationID: instID,
		ChannelUserID:  channelUserID,
		Status:         "active",
	}
	q.installation = Installation{
		ID:          instID,
		WorkspaceID: wsID,
		Purpose:     InstallationPurposeAgent,
		AppID:       "cli_test_app",
		BotOpenID:   "bot_open_id",
		Status:      string(InstallationActive),
		Region:      string(RegionFeishu),
	}
	q.workspace = db.Workspace{Slug: "acme"}
	q.mu.Unlock()
}

func inboxDeliveryCount(q *fakePatcherQueries) int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.inboxDeliveries)
}

func inboxDelivery(t *testing.T, q *fakePatcherQueries) db.InboxLarkDelivery {
	t.Helper()
	q.mu.Lock()
	defer q.mu.Unlock()
	if len(q.inboxDeliveryOrder) == 0 {
		t.Fatal("no inbox delivery recorded")
	}
	return *q.inboxDeliveries[q.inboxDeliveryOrder[0]]
}

func TestPatcherRegistersInboxNewSubscriber(t *testing.T) {
	p, _, _ := newTestPatcher(t)
	bus := events.New()
	p.Register(bus)
	if got := bus.SubscriberCount(protocol.EventInboxNew); got != 1 {
		t.Fatalf("inbox:new subscribers = %d, want 1", got)
	}
}

func TestInboxNewUsesExplicitDestinationRegardlessOfActor(t *testing.T) {
	p, q, api := newTestPatcher(t)
	p.cfg.AppURL = "https://app.example"
	bindInboxTestDestination(t, q, inboxTestInstallation, inboxTestChannelUser)
	item := inboxTestItem()
	item["actor_type"] = "agent"
	item["actor_id"] = "8888abcd-8888-8888-8888-888888888888"

	p.handleInboxNew(inboxTestEvent(item))
	p.ProcessInboxDeliveriesOnce(context.Background())

	if len(api.userCardSent) != 1 {
		t.Fatalf("sent %d cards, want 1", len(api.userCardSent))
	}
	got := api.userCardSent[0]
	if got.OpenID != OpenID(inboxTestChannelUser) {
		t.Errorf("open_id = %q, want %q", got.OpenID, inboxTestChannelUser)
	}
	wantUUID := inboxProviderUUIDForDelivery(inboxDeliveryKey(
		inboxTestItemID, inboxTestMemberID, inboxTestInstallation, inboxTestChannelUser,
	))
	if got.UUID != wantUUID {
		t.Errorf("uuid = %q, want %q", got.UUID, wantUUID)
	}
	if delivery := inboxDelivery(t, q); delivery.Status != "delivered" {
		t.Errorf("delivery status = %q, want delivered", delivery.Status)
	}
}

func TestInboxNewWithoutDestinationStaysInApp(t *testing.T) {
	p, q, api := newTestPatcher(t)

	p.handleInboxNew(inboxTestEvent(inboxTestItem()))
	p.ProcessInboxDeliveriesOnce(context.Background())

	if len(api.userCardSent)+len(api.userTextSent) != 0 {
		t.Fatalf("sent %d messages, want 0", len(api.userCardSent)+len(api.userTextSent))
	}
	if n := inboxDeliveryCount(q); n != 0 {
		t.Fatalf("created %d deliveries, want 0", n)
	}
}

func TestInboxNewSkipsDisabledDestination(t *testing.T) {
	p, q, api := newTestPatcher(t)
	bindInboxTestDestination(t, q, inboxTestInstallation, inboxTestChannelUser)
	q.mu.Lock()
	dest := q.inboxDestinations[inboxMemberKey(
		uuidFromString(t, inboxTestWorkspaceID), uuidFromString(t, inboxTestMemberID))]
	dest.Status = "disabled"
	q.inboxDestinations[inboxMemberKey(
		uuidFromString(t, inboxTestWorkspaceID), uuidFromString(t, inboxTestMemberID))] = dest
	q.mu.Unlock()

	p.handleInboxNew(inboxTestEvent(inboxTestItem()))
	p.ProcessInboxDeliveriesOnce(context.Background())

	if len(api.userCardSent)+len(api.userTextSent) != 0 {
		t.Fatalf("disabled destination sent %d messages, want 0", len(api.userCardSent)+len(api.userTextSent))
	}
	if n := inboxDeliveryCount(q); n != 0 {
		t.Fatalf("created %d deliveries for a disabled destination, want 0", n)
	}
}

func TestInboxNewSkipsAgentRecipients(t *testing.T) {
	p, q, api := newTestPatcher(t)
	bindInboxTestDestination(t, q, inboxTestInstallation, inboxTestChannelUser)
	item := inboxTestItem()
	item["recipient_type"] = "agent"

	p.handleInboxNew(inboxTestEvent(item))
	p.ProcessInboxDeliveriesOnce(context.Background())

	if len(api.userCardSent)+len(api.userTextSent) != 0 {
		t.Fatalf("agent recipient received %d messages, want 0", len(api.userCardSent)+len(api.userTextSent))
	}
}

func TestInboxDeliveryCancelsAfterTargetReplaced(t *testing.T) {
	p, q, api := newTestPatcher(t)
	bindInboxTestDestination(t, q, inboxTestInstallation, inboxTestChannelUser)
	p.handleInboxNew(inboxTestEvent(inboxTestItem()))

	// Replace the destination before the worker claims the queued row.
	bindInboxTestDestination(t, q, inboxTestOtherInstall, inboxTestOtherUser)
	p.ProcessInboxDeliveriesOnce(context.Background())

	if len(api.userCardSent)+len(api.userTextSent) != 0 {
		t.Fatalf("replaced target received %d messages, want 0", len(api.userCardSent)+len(api.userTextSent))
	}
	delivery := inboxDelivery(t, q)
	if delivery.Status != "cancelled" {
		t.Errorf("delivery status = %q, want cancelled", delivery.Status)
	}
	if delivery.CancelledReason.String != "target_changed_or_disabled" {
		t.Errorf("cancel reason = %q", delivery.CancelledReason.String)
	}
}

func TestInboxDeliveryRetriesThenSucceeds(t *testing.T) {
	p, q, api := newTestPatcher(t)
	now := time.Date(2026, 10, 21, 10, 0, 0, 0, time.UTC)
	q.inboxNow = func() time.Time { return now }
	p.cfg.Now = q.inboxNow
	bindInboxTestDestination(t, q, inboxTestInstallation, inboxTestChannelUser)
	api.userCardErr = errors.New("card endpoint down")
	api.userTextErr = errors.New("network down")

	p.handleInboxNew(inboxTestEvent(inboxTestItem()))
	p.ProcessInboxDeliveriesOnce(context.Background())

	delivery := inboxDelivery(t, q)
	if delivery.Status != "retry" || delivery.Attempts != 1 {
		t.Fatalf("after first failure = (%s, %d), want (retry, 1)", delivery.Status, delivery.Attempts)
	}
	if !delivery.NextAttemptAt.Time.After(now) {
		t.Fatalf("next_attempt_at = %s, want after %s", delivery.NextAttemptAt.Time, now)
	}

	// Not due yet: no second attempt.
	p.ProcessInboxDeliveriesOnce(context.Background())
	if len(api.userCardSent) != 1 {
		t.Fatalf("attempts before backoff = %d, want 1", len(api.userCardSent))
	}

	now = now.Add(11 * time.Second)
	api.userCardErr = nil
	api.userTextErr = nil
	p.ProcessInboxDeliveriesOnce(context.Background())

	if len(api.userCardSent) != 2 {
		t.Fatalf("attempts after backoff = %d, want 2", len(api.userCardSent))
	}
	if delivery := inboxDelivery(t, q); delivery.Status != "delivered" {
		t.Errorf("delivery status = %q, want delivered", delivery.Status)
	}
}

func TestInboxDeliveryFailsAfterMaxAttempts(t *testing.T) {
	p, q, api := newTestPatcher(t)
	now := time.Date(2026, 10, 21, 10, 0, 0, 0, time.UTC)
	q.inboxNow = func() time.Time { return now }
	p.cfg.Now = q.inboxNow
	bindInboxTestDestination(t, q, inboxTestInstallation, inboxTestChannelUser)
	api.userCardErr = errors.New("card rejected")
	api.userTextErr = errors.New("network down")

	p.handleInboxNew(inboxTestEvent(inboxTestItem()))
	for i := 0; i < inboxMaxAttempts; i++ {
		p.ProcessInboxDeliveriesOnce(context.Background())
		now = now.Add(20 * time.Minute)
	}

	delivery := inboxDelivery(t, q)
	if delivery.Status != "failed" {
		t.Fatalf("delivery status = %q, want failed", delivery.Status)
	}
	if delivery.Attempts != inboxMaxAttempts {
		t.Fatalf("attempts = %d, want %d", delivery.Attempts, inboxMaxAttempts)
	}
	p.ProcessInboxDeliveriesOnce(context.Background())
	if len(api.userCardSent) != inboxMaxAttempts {
		t.Fatalf("attempts after terminal failure = %d, want %d", len(api.userCardSent), inboxMaxAttempts)
	}
}

func TestInboxDuplicateEventEnqueuesOneDelivery(t *testing.T) {
	p, q, api := newTestPatcher(t)
	bindInboxTestDestination(t, q, inboxTestInstallation, inboxTestChannelUser)
	item := inboxTestItem()

	p.handleInboxNew(inboxTestEvent(item))
	p.handleInboxNew(inboxTestEvent(item))
	p.ProcessInboxDeliveriesOnce(context.Background())

	if n := inboxDeliveryCount(q); n != 1 {
		t.Fatalf("deliveries = %d, want 1", n)
	}
	if len(api.userCardSent) != 1 {
		t.Fatalf("cards = %d, want 1", len(api.userCardSent))
	}
}

func TestInboxDeliverySurvivesRestart(t *testing.T) {
	p, q, api := newTestPatcher(t)
	bindInboxTestDestination(t, q, inboxTestInstallation, inboxTestChannelUser)
	p.handleInboxNew(inboxTestEvent(inboxTestItem()))

	// Simulate a new process: only the durable outbox row survives.
	restarted := NewPatcher(q, fakeCredentials{}, api, PatcherConfig{})
	restarted.ProcessInboxDeliveriesOnce(context.Background())

	if len(api.userCardSent) != 1 {
		t.Fatalf("cards after restart = %d, want 1", len(api.userCardSent))
	}
	if delivery := inboxDelivery(t, q); delivery.Status != "delivered" {
		t.Errorf("delivery status = %q, want delivered", delivery.Status)
	}
}

func TestInboxDeliveryClaimIsExclusive(t *testing.T) {
	p, q, _ := newTestPatcher(t)
	bindInboxTestDestination(t, q, inboxTestInstallation, inboxTestChannelUser)
	p.handleInboxNew(inboxTestEvent(inboxTestItem()))

	if _, err := q.ClaimNextInboxLarkDelivery(context.Background(), newInboxLarkClaimToken()); err != nil {
		t.Fatalf("first claim: %v", err)
	}
	if _, err := q.ClaimNextInboxLarkDelivery(context.Background(), newInboxLarkClaimToken()); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("second claim error = %v, want ErrNoRows", err)
	}
}

func TestInboxProviderUUIDIsStableAndTargetScoped(t *testing.T) {
	key := inboxDeliveryKey("item", "recipient", "installation", "open-id")
	if strings.ContainsRune(key, '\x00') {
		t.Fatal("delivery key must not contain NUL; PostgreSQL TEXT rejects it")
	}
	base := inboxProviderUUIDForDelivery(key)
	if len(base) != 36 || base[14] != '5' {
		t.Fatalf("not a v5 uuid: %q", base)
	}
	if base != inboxProviderUUIDForDelivery(key) {
		t.Fatal("same key produced a different uuid")
	}
	if base == inboxProviderUUIDForDelivery(inboxDeliveryKey("item", "recipient", "installation", "other-open-id")) {
		t.Fatal("channel user must be part of the provider uuid")
	}
	if base == inboxProviderUUIDForDelivery(inboxDeliveryKey("item", "other", "installation", "open-id")) {
		t.Fatal("recipient must be part of the provider uuid")
	}
}

func TestInboxDeliveryDoesNotRedirectToReplacementTarget(t *testing.T) {
	p, q, api := newTestPatcher(t)
	bindInboxTestDestination(t, q, inboxTestInstallation, inboxTestChannelUser)
	p.handleInboxNew(inboxTestEvent(inboxTestItem()))
	bindInboxTestDestination(t, q, inboxTestOtherInstall, inboxTestOtherUser)

	p.ProcessInboxDeliveriesOnce(context.Background())

	for _, sent := range api.userCardSent {
		if sent.OpenID == OpenID(inboxTestOtherUser) {
			t.Fatal("old item was redirected to the replacement target")
		}
	}
	if len(api.userCardSent) != 0 {
		t.Fatalf("old item sent %d times, want 0", len(api.userCardSent))
	}
}

func TestBuildInboxCard(t *testing.T) {
	item := inboxTestItem()

	got := buildInboxCard(item, "acme", "https://app.example/")
	if got.Title != "Task completed" {
		t.Errorf("title = %q", got.Title)
	}
	wantMarkdown := "Agent finished the task.\n\n[查看详情](https://app.example/acme/inbox?issue=" + inboxTestIssueID + ")"
	if got.Markdown != wantMarkdown {
		t.Errorf("markdown = %q, want %q", got.Markdown, wantMarkdown)
	}

	raw, err := renderInboxCard(got, inboxCardMaxBytes)
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

	noLink := buildInboxCard(item, "acme", "")
	if strings.Contains(noLink.Markdown, "http") {
		t.Errorf("card should not contain a link: %q", noLink.Markdown)
	}
}
