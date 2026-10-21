package lark

// outbound_inbox.go turns an inbox:new event into a durable Feishu delivery.
//
// The inbox item is the source of truth. The recipient's explicit
// inbox_lark_destination selects the bot and channel identity; the actor agent
// is never consulted. If the member has no active destination the item stays
// in the in-app inbox and nothing is sent.
//
// Delivery is asynchronous and recoverable. The subscriber only validates the
// event and writes an outbox row keyed by (item, recipient, installation,
// channel user). A worker claims due rows with FOR UPDATE SKIP LOCKED, so a
// restart resumes pending work and replicas cannot consume the same row twice.

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/events"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

const (
	inboxPushTimeout = 8 * time.Second
	inboxWorkerTick  = 2 * time.Second
	inboxBatchSize   = 20
	inboxMaxAttempts = 5
)

// handleInboxNew is the inbox:new subscriber. It validates the payload and
// persists one outbox row for the recipient's explicit destination. Any miss -
// non-member recipient, no destination, disabled destination, invalid ids - is
// a no-op; the in-app inbox remains the guaranteed channel.
func (p *Patcher) handleInboxNew(e events.Event) {
	payload, ok := e.Payload.(map[string]any)
	if !ok {
		return
	}
	item, ok := payload["item"].(map[string]any)
	if !ok {
		return
	}
	if recipientType := inboxItemString(item, "recipient_type"); recipientType != "member" {
		return
	}
	itemID := inboxItemString(item, "id")
	recipientID := inboxItemString(item, "recipient_id")
	workspaceID := inboxItemString(item, "workspace_id")
	if itemID == "" || recipientID == "" || workspaceID == "" {
		return
	}
	p.enqueueInboxDelivery(item, itemID, recipientID, workspaceID)
	p.wakeInboxWorker()
}

// enqueueInboxDelivery persists the outbox row. It runs on the request that
// published inbox:new, so it uses a short context and never performs a network
// call. A DB failure only loses the optional external push, never the item.
func (p *Patcher) enqueueInboxDelivery(item map[string]any, itemID, recipientIDStr, workspaceIDStr string) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	recipientID, err := util.ParseUUID(recipientIDStr)
	if err != nil || !recipientID.Valid {
		return
	}
	workspaceID, err := util.ParseUUID(workspaceIDStr)
	if err != nil || !workspaceID.Valid {
		return
	}
	destination, err := p.queries.GetInboxLarkDestination(ctx, workspaceID, recipientID)
	if err != nil {
		if !errors.Is(err, pgx.ErrNoRows) {
			p.cfg.Logger.Warn("lark patcher: inbox destination lookup failed",
				"error", err, "workspace_id", workspaceIDStr, "recipient_id", recipientIDStr)
		}
		return
	}
	if destination.Status != "active" || !destination.InstallationID.Valid || destination.ChannelUserID == "" {
		return
	}
	itemUUID, err := util.ParseUUID(itemID)
	if err != nil || !itemUUID.Valid {
		return
	}

	payload, err := json.Marshal(item)
	if err != nil {
		p.cfg.Logger.Warn("lark patcher: inbox payload encode failed",
			"error", err, "item_id", itemID)
		return
	}
	key := inboxDeliveryKey(itemID, recipientIDStr, uuidString(destination.InstallationID), destination.ChannelUserID)
	delivery, err := p.queries.CreateInboxLarkDelivery(ctx, db.CreateInboxLarkDeliveryParams{
		WorkspaceID:       workspaceID,
		InboxItemID:       itemUUID,
		RecipientMemberID: recipientID,
		InstallationID:    destination.InstallationID,
		ChannelUserID:     destination.ChannelUserID,
		IdempotencyKey:    key,
		Payload:           payload,
	})
	if err != nil {
		p.cfg.Logger.Warn("lark patcher: inbox delivery enqueue failed",
			"error", err, "item_id", itemID, "recipient_id", recipientIDStr)
		return
	}
	p.cfg.Logger.Debug("lark patcher: inbox delivery enqueued",
		"delivery_id", uuidString(delivery.ID), "item_id", itemID,
		"installation_id", uuidString(destination.InstallationID),
		"recipient_id", recipientIDStr)
}

// RunInboxDeliveryWorker drains the durable outbox until ctx is cancelled.
// The ticker is the restart-recovery path; wakeInboxWorker shortens the
// latency for a freshly enqueued item.
func (p *Patcher) RunInboxDeliveryWorker(ctx context.Context) {
	ticker := time.NewTicker(inboxWorkerTick)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			p.processDueInboxDeliveries(ctx)
		case <-p.inboxWake:
			p.processDueInboxDeliveries(ctx)
		}
	}
}

func (p *Patcher) wakeInboxWorker() {
	if p.inboxWake == nil {
		return
	}
	select {
	case p.inboxWake <- struct{}{}:
	default:
	}
}

// ProcessInboxDeliveriesOnce is the test seam for the worker loop.
func (p *Patcher) ProcessInboxDeliveriesOnce(ctx context.Context) {
	p.processDueInboxDeliveries(ctx)
}

func (p *Patcher) processDueInboxDeliveries(ctx context.Context) {
	if _, err := p.queries.RecoverStaleInboxLarkDeliveries(ctx); err != nil {
		p.cfg.Logger.Warn("lark patcher: recover stale inbox deliveries failed", "error", err)
	}
	for i := 0; i < inboxBatchSize; i++ {
		if ctx.Err() != nil {
			return
		}
		claimToken := newInboxLarkClaimToken()
		if !claimToken.Valid {
			p.cfg.Logger.Error("lark patcher: could not mint inbox claim token")
			return
		}
		delivery, err := p.queries.ClaimNextInboxLarkDelivery(ctx, claimToken)
		if err != nil {
			if !errors.Is(err, pgx.ErrNoRows) {
				p.cfg.Logger.Warn("lark patcher: claim inbox delivery failed", "error", err)
			}
			return
		}
		p.processInboxDelivery(ctx, delivery)
	}
}

// processInboxDelivery sends one claimed row. The row's target is frozen at
// enqueue time; the current destination is only consulted to prove the target
// is still explicitly selected. A replaced or unbound destination cancels the
// row instead of redirecting it.
func (p *Patcher) processInboxDelivery(ctx context.Context, delivery db.InboxLarkDelivery) {
	defer func() {
		if r := recover(); r != nil {
			p.cfg.Logger.Error("lark patcher: inbox delivery panic",
				"panic", r, "delivery_id", uuidString(delivery.ID))
			p.failInboxDelivery(ctx, delivery, fmt.Errorf("panic: %v", r))
		}
	}()
	ctx, cancel := context.WithTimeout(ctx, inboxPushTimeout)
	defer cancel()

	destination, err := p.queries.GetInboxLarkDestination(ctx, delivery.WorkspaceID, delivery.RecipientMemberID)
	if err != nil || destination.Status != "active" ||
		destination.InstallationID != delivery.InstallationID ||
		destination.ChannelUserID != delivery.ChannelUserID {
		p.cancelInboxDelivery(ctx, delivery, "target_changed_or_disabled")
		return
	}
	binding, err := p.queries.FindChannelBindingForMemberOnInstallation(ctx, delivery.WorkspaceID, delivery.RecipientMemberID, delivery.InstallationID)
	if err != nil || binding.ChannelUserID != delivery.ChannelUserID {
		p.cancelInboxDelivery(ctx, delivery, "binding_changed_or_removed")
		return
	}

	inst, err := p.queries.GetLarkInstallation(ctx, delivery.InstallationID)
	if err != nil {
		p.cancelInboxDelivery(ctx, delivery, "installation_not_found")
		return
	}
	if InstallationStatus(inst.Status) != InstallationActive {
		p.cancelInboxDelivery(ctx, delivery, "installation_revoked")
		return
	}
	creds, err := p.installationCredentials(inst)
	if err != nil {
		p.failInboxDelivery(ctx, delivery, err)
		return
	}
	if p.client == nil {
		p.failInboxDelivery(ctx, delivery, errors.New("lark client unavailable"))
		return
	}

	var item map[string]any
	if err := json.Unmarshal(delivery.Payload, &item); err != nil {
		p.cancelInboxDelivery(ctx, delivery, "invalid_payload")
		return
	}

	slug := ""
	if p.cfg.AppURL != "" {
		if ws, err := p.queries.GetWorkspace(ctx, delivery.WorkspaceID); err != nil {
			p.cfg.Logger.Warn("lark patcher: inbox workspace lookup failed; sending without link",
				"error", err, "workspace_id", uuidString(delivery.WorkspaceID))
		} else {
			slug = strings.TrimSpace(ws.Slug)
		}
	}
	card := buildInboxCard(item, slug, p.cfg.AppURL)
	if card.Markdown == "" && card.Title == "" {
		p.cancelInboxDelivery(ctx, delivery, "empty_content")
		return
	}

	providerUUID := inboxProviderUUIDForDelivery(delivery.IdempotencyKey)
	delivered := false
	if card.Markdown != "" {
		if _, err := p.client.SendUserMarkdownCard(ctx, SendUserMarkdownCardParams{
			InstallationID: creds,
			OpenID:         OpenID(delivery.ChannelUserID),
			Title:          card.Title,
			Markdown:       card.Markdown,
			Summary:        card.Summary,
			UUID:           providerUUID,
		}); err != nil {
			p.cfg.Logger.Warn("lark patcher: inbox card push failed; falling back to text",
				"error", err, "delivery_id", uuidString(delivery.ID),
				"installation_id", uuidString(delivery.InstallationID))
		} else {
			delivered = true
		}
	}
	if !delivered {
		content := buildInboxText(item, slug, p.cfg.AppURL)
		if content == "" {
			p.cancelInboxDelivery(ctx, delivery, "empty_content")
			return
		}
		if _, err := p.client.SendUserTextMessage(ctx, SendUserTextParams{
			InstallationID: creds,
			OpenID:         OpenID(delivery.ChannelUserID),
			Text:           content,
			UUID:           providerUUID,
		}); err != nil {
			p.failInboxDelivery(ctx, delivery, err)
			return
		}
	}

	markCtx, markCancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer markCancel()
	updated, err := p.queries.MarkInboxLarkDeliveryDelivered(markCtx, delivery.ID, delivery.ClaimToken)
	if err != nil {
		p.cfg.Logger.Warn("lark patcher: mark inbox delivery delivered failed",
			"error", err, "delivery_id", uuidString(delivery.ID))
		return
	}
	if updated == 0 {
		p.cfg.Logger.Warn("lark patcher: inbox delivery claim was lost before commit",
			"delivery_id", uuidString(delivery.ID))
		return
	}
	p.cfg.Logger.Debug("lark patcher: inbox delivered",
		"delivery_id", uuidString(delivery.ID),
		"item_id", uuidString(delivery.InboxItemID),
		"installation_id", uuidString(delivery.InstallationID),
		"recipient_id", uuidString(delivery.RecipientMemberID))
}

func (p *Patcher) failInboxDelivery(ctx context.Context, delivery db.InboxLarkDelivery, cause error) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	message := cause.Error()
	if delivery.Attempts >= inboxMaxAttempts {
		updated, err := p.queries.FailInboxLarkDelivery(ctx, delivery.ID, delivery.ClaimToken, message)
		if err != nil {
			p.cfg.Logger.Warn("lark patcher: mark inbox delivery failed",
				"error", err, "delivery_id", uuidString(delivery.ID))
		} else if updated == 0 {
			p.cfg.Logger.Warn("lark patcher: inbox delivery claim was lost before failure commit",
				"delivery_id", uuidString(delivery.ID))
		}
		return
	}
	next := p.cfg.Now().Add(inboxRetryDelay(delivery.Attempts))
	updated, err := p.queries.RetryInboxLarkDelivery(ctx, delivery.ID, delivery.ClaimToken,
		pgtype.Timestamptz{Time: next, Valid: true}, message)
	if err != nil {
		p.cfg.Logger.Warn("lark patcher: schedule inbox delivery retry",
			"error", err, "delivery_id", uuidString(delivery.ID))
	} else if updated == 0 {
		p.cfg.Logger.Warn("lark patcher: inbox delivery claim was lost before retry commit",
			"delivery_id", uuidString(delivery.ID))
	}
}

func (p *Patcher) cancelInboxDelivery(ctx context.Context, delivery db.InboxLarkDelivery, reason string) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	updated, err := p.queries.CancelInboxLarkDelivery(ctx, delivery.ID, delivery.ClaimToken, reason)
	if err != nil {
		p.cfg.Logger.Warn("lark patcher: cancel inbox delivery failed",
			"error", err, "delivery_id", uuidString(delivery.ID), "reason", reason)
		return
	}
	if updated == 0 {
		p.cfg.Logger.Warn("lark patcher: inbox delivery claim was lost before cancel",
			"delivery_id", uuidString(delivery.ID))
	}
}

// inboxRetryDelay is bounded exponential backoff for a claim's attempt count
// (already incremented when the row was claimed).
func inboxRetryDelay(attempts int32) time.Duration {
	switch {
	case attempts <= 1:
		return 10 * time.Second
	case attempts == 2:
		return time.Minute
	case attempts == 3:
		return 5 * time.Minute
	default:
		return 15 * time.Minute
	}
}

// inboxDeliveryKey includes the recipient and the explicit target. Changing
// either produces a different row; the item alone is never enough to resend.
func inboxDeliveryKey(itemID, recipientID, installationID, channelUserID string) string {
	// PostgreSQL TEXT rejects NUL bytes. Use the unit separator so the key
	// remains collision-resistant without making the insert fail.
	return strings.Join([]string{itemID, recipientID, installationID, channelUserID}, "\x1f")
}

// inboxProviderUUIDForDelivery derives Lark's idempotency key from the durable
// outbox key. It is stable across replicas, retries, and restarts. Lark may
// still produce a duplicate when a request times out after the provider
// accepted it; delivery records expose that uncertainty instead of claiming
// end-to-end exactly-once semantics.
func inboxProviderUUIDForDelivery(key string) string {
	sum := sha256.Sum256([]byte(key))
	b := sum[:16]
	b[6] = (b[6] & 0x0f) | 0x50
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}
