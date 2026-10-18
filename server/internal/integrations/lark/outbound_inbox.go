package lark

// outbound_inbox.go delivers inbox:new items to members who bound the Bot.
// It mirrors the WeCom smart-bot inbox relay: member recipients only, the
// most recently bound active installation, best-effort delivery, and no
// effect on the inbox row or the request that created it.
//
// Delivery is asynchronous. The bus publishes synchronously on the request
// that created the inbox item, so the subscriber only validates and enqueues;
// a single bounded worker performs the DB lookups and the Lark HTTP call.

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/events"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

const (
	inboxPushTimeout = 5 * time.Second
	inboxDedupeTTL   = 10 * time.Minute
	// inboxQueueSize bounds memory when Lark is slow or unavailable. A full
	// queue drops the push (the in-app inbox still has the item) rather than
	// blocking the HTTP request that published the event.
	inboxQueueSize = 256
)

// inboxJob is the validated, minimal payload the worker needs. Keeping the
// map snapshot (rather than the event) means no part of the request path
// stays referenced after the publisher returns.
type inboxJob struct {
	item        map[string]any
	itemID      string
	recipientID string
	workspaceID string
}

// inboxDedupe keeps the last few minutes of delivered inbox items in memory.
// Bus delivery is in-process, so a duplicate publish of one item arrives on
// the same replica and is suppressed here. The key includes the recipient
// and installation because one item is delivered at most once per
// recipient/installation pair. Cross-replica duplicates are covered by the
// stable UUID sent to Lark, which is independent of this cache.
type inboxDedupe struct {
	mu   sync.Mutex
	seen map[string]time.Time
	now  func() time.Time
}

func newInboxDedupe() *inboxDedupe {
	return &inboxDedupe{seen: make(map[string]time.Time), now: time.Now}
}

// claim records key and reports whether the caller should deliver. An
// expired key can be claimed again; a live key cannot.
func (d *inboxDedupe) claim(key string) bool {
	if d == nil {
		return true
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	now := d.now()
	if expiry, ok := d.seen[key]; ok && now.Before(expiry) {
		return false
	}
	if len(d.seen) > 4096 {
		for k, expiry := range d.seen {
			if !now.Before(expiry) {
				delete(d.seen, k)
			}
		}
	}
	d.seen[key] = now.Add(inboxDedupeTTL)
	return true
}

// release removes a claim after a send failed. A transient failure must not
// suppress the next delivery attempt for the whole TTL.
func (d *inboxDedupe) release(key string) {
	if d == nil {
		return
	}
	d.mu.Lock()
	delete(d.seen, key)
	d.mu.Unlock()
}

func inboxDedupeKey(itemID, recipientID, installationID string) string {
	return strings.Join([]string{itemID, recipientID, installationID}, "\x00")
}

// inboxProviderUUID derives Lark's idempotency key from the same tuple as the
// local dedupe key. It is stable across replicas and restarts, so a retry or
// a duplicate publish cannot create a second chat message. The SHA-256 digest
// is formatted as an RFC 4122 version-5 UUID.
func inboxProviderUUID(itemID, recipientID, installationID string) string {
	sum := sha256.Sum256([]byte(inboxDedupeKey(itemID, recipientID, installationID)))
	b := sum[:16]
	b[6] = (b[6] & 0x0f) | 0x50
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// handleInboxNew is the inbox:new subscriber. It validates the payload and
// hands it to the worker without touching the network: the bus runs handlers
// synchronously on the request that created the item. Any miss - non-member
// recipient, no active binding, inactive installation, failed send - is a
// no-op; the member still receives the notification through the in-app inbox.
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

	p.startInboxWorker()
	job := inboxJob{item: item, itemID: itemID, recipientID: recipientID, workspaceID: workspaceID}
	p.inboxPending.Add(1)
	select {
	case p.inboxQueue <- job:
	default:
		p.inboxPending.Done()
		p.cfg.Logger.Warn("lark patcher: inbox queue full, dropping push",
			"item_id", itemID, "recipient_id", recipientID)
	}
}

// startInboxWorker lazily starts the single delivery worker. One worker keeps
// notifications in publish order; the 5s per-item timeout bounds head-of-line
// blocking.
func (p *Patcher) startInboxWorker() {
	p.inboxOnce.Do(func() {
		go func() {
			for job := range p.inboxQueue {
				p.processInboxJob(job)
			}
		}()
	})
}

func (p *Patcher) processInboxJob(job inboxJob) {
	defer p.inboxPending.Done()
	defer func() {
		if r := recover(); r != nil {
			p.cfg.Logger.Error("lark patcher: inbox worker panic",
				"panic", r, "item_id", job.itemID, "recipient_id", job.recipientID)
		}
	}()
	// The worker outlives the request that published the event, so it must
	// build its own context instead of borrowing the publisher's.
	ctx, cancel := context.WithTimeout(context.Background(), inboxPushTimeout)
	defer cancel()
	p.deliverInbox(ctx, job.item, job.itemID, job.recipientID, job.workspaceID)
}

func (p *Patcher) deliverInbox(ctx context.Context, item map[string]any, itemID, recipientIDStr, workspaceIDStr string) {
	recipientID, err := util.ParseUUID(recipientIDStr)
	if err != nil || !recipientID.Valid {
		return
	}
	workspaceID, err := util.ParseUUID(workspaceIDStr)
	if err != nil || !workspaceID.Valid {
		return
	}
	binding, err := p.inboxMemberBinding(ctx, item, workspaceID, recipientID)
	if err != nil {
		if !errors.Is(err, pgx.ErrNoRows) {
			p.cfg.Logger.Warn("lark patcher: inbox binding lookup failed",
				"error", err, "workspace_id", workspaceIDStr, "recipient_id", recipientIDStr)
		}
		return
	}
	if !binding.InstallationID.Valid || binding.ChannelUserID == "" {
		return
	}
	// A send failure is deliberately not re-routed to the recipient-wide
	// binding: an HTTP error does not prove Lark accepted nothing, and an
	// inline second send could duplicate the notification. A later duplicate
	// event retries the same route under the stable UUID below.
	inst, err := p.queries.GetLarkInstallation(ctx, binding.InstallationID)
	if err != nil {
		p.cfg.Logger.Warn("lark patcher: inbox installation lookup failed",
			"error", err, "installation_id", uuidString(binding.InstallationID), "recipient_id", recipientIDStr)
		return
	}
	if InstallationStatus(inst.Status) != InstallationActive {
		return
	}
	creds, err := p.installationCredentials(inst)
	if err != nil {
		p.cfg.Logger.Warn("lark patcher: inbox credentials resolution failed",
			"error", err, "installation_id", uuidString(binding.InstallationID), "recipient_id", recipientIDStr)
		return
	}
	if p.client == nil {
		return
	}

	slug := ""
	if p.cfg.AppURL != "" {
		if ws, err := p.queries.GetWorkspace(ctx, workspaceID); err != nil {
			p.cfg.Logger.Warn("lark patcher: inbox workspace lookup failed; sending without link",
				"error", err, "workspace_id", workspaceIDStr)
		} else {
			slug = strings.TrimSpace(ws.Slug)
		}
		if slug == "" {
			p.cfg.Logger.Warn("lark patcher: inbox workspace has no slug; sending without link",
				"workspace_id", workspaceIDStr)
		}
	}
	content := buildInboxText(item, slug, p.cfg.AppURL)
	if content == "" {
		return
	}

	installationID := uuidString(binding.InstallationID)
	dedupeKey := inboxDedupeKey(itemID, recipientIDStr, installationID)
	if !p.inboxSeen.claim(dedupeKey) {
		return
	}
	if _, err := p.client.SendUserTextMessage(ctx, SendUserTextParams{
		InstallationID: creds,
		OpenID:         OpenID(binding.ChannelUserID),
		Text:           content,
		UUID:           inboxProviderUUID(itemID, recipientIDStr, installationID),
	}); err != nil {
		p.inboxSeen.release(dedupeKey)
		p.cfg.Logger.Warn("lark patcher: inbox push failed",
			"error", err, "item_id", itemID, "installation_id", installationID, "recipient_id", recipientIDStr)
		return
	}
	p.cfg.Logger.Debug("lark patcher: inbox delivered",
		"item_id", itemID, "installation_id", installationID, "recipient_id", recipientIDStr)
}

// inboxMemberBinding resolves which of the recipient's Lark bots carries this
// notification. A member-authored notification uses the recipient-wide
// binding. When an agent raised it, the recipient's binding ON that agent's
// bot is preferred, so the message arrives through the bot the member
// associates with the agent rather than whichever bot they bound last. The
// recipient-wide lookup remains the fallback when the agent has no active
// installation the member bound.
func (p *Patcher) inboxMemberBinding(ctx context.Context, item map[string]any, workspaceID, recipientID pgtype.UUID) (db.ChannelUserBinding, error) {
	if agentID, ok := inboxActorAgent(item); ok {
		binding, err := p.queries.FindChannelBindingForMemberOnAgentInstallation(ctx, db.FindChannelBindingForMemberOnAgentInstallationParams{
			WorkspaceID:   workspaceID,
			MulticaUserID: recipientID,
			ChannelType:   channelTypeFeishu,
			AgentID:       agentID,
		})
		switch {
		case err == nil:
			return binding, nil
		case !errors.Is(err, pgx.ErrNoRows):
			p.cfg.Logger.Warn("lark patcher: agent-bot binding lookup failed; using recipient-wide binding",
				"error", err, "agent_id", uuidString(agentID))
		}
	}
	return p.queries.FindChannelBindingForMember(ctx, db.FindChannelBindingForMemberParams{
		WorkspaceID:   workspaceID,
		MulticaUserID: recipientID,
		ChannelType:   channelTypeFeishu,
	})
}

// inboxActorAgent reads the agent an inbox notification names as its actor,
// when it names one. The in-process payload carries strings or *string for
// nullable fields; inboxItemString handles both spellings.
func inboxActorAgent(item map[string]any) (pgtype.UUID, bool) {
	if inboxItemString(item, "actor_type") != "agent" {
		return pgtype.UUID{}, false
	}
	id := inboxItemString(item, "actor_id")
	if id == "" {
		return pgtype.UUID{}, false
	}
	agentID, err := util.ParseUUID(id)
	if err != nil || !agentID.Valid {
		return pgtype.UUID{}, false
	}
	return agentID, true
}
