package lark

// outbound_inbox.go delivers inbox:new items to members who bound the Bot.
// It mirrors the WeCom smart-bot inbox relay: member recipients only, the
// most recently bound active installation, best-effort delivery, and no
// effect on the inbox row or the request that created it.

import (
	"context"
	"errors"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/multica-ai/multica/server/internal/events"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

const (
	inboxPushTimeout = 5 * time.Second
	inboxDedupeTTL   = 10 * time.Minute
)

// inboxDedupe keeps the last few minutes of delivered inbox items in memory.
// Bus delivery is in-process, so a duplicate publish of one item arrives on
// the same replica and is suppressed here. The key includes the recipient
// and installation because one item is delivered at most once per
// recipient/installation pair.
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

func inboxDedupeKey(itemID, recipientID, installationID string) string {
	return strings.Join([]string{itemID, recipientID, installationID}, "\x00")
}

// handleInboxNew is the inbox:new subscriber. Any miss - non-member
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

	ctx, cancel := context.WithTimeout(context.Background(), inboxPushTimeout)
	defer cancel()
	p.deliverInbox(ctx, item, itemID, recipientID, workspaceID)
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
	// Same selection rule as the WeCom inbox relay: the member's most recent
	// binding whose installation is active.
	binding, err := p.queries.FindChannelBindingForMember(ctx, db.FindChannelBindingForMemberParams{
		WorkspaceID:   workspaceID,
		MulticaUserID: recipientID,
		ChannelType:   channelTypeFeishu,
	})
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
		if ws, err := p.queries.GetWorkspace(ctx, workspaceID); err == nil {
			slug = ws.Slug
		}
	}
	content := buildInboxText(item, workspaceIDStr, slug, p.cfg.AppURL)
	if content == "" {
		return
	}

	installationID := uuidString(binding.InstallationID)
	if !p.inboxSeen.claim(inboxDedupeKey(itemID, recipientIDStr, installationID)) {
		return
	}
	if _, err := p.client.SendUserTextMessage(ctx, SendUserTextParams{
		InstallationID: creds,
		OpenID:         OpenID(binding.ChannelUserID),
		Text:           content,
	}); err != nil {
		p.cfg.Logger.Warn("lark patcher: inbox push failed",
			"error", err, "item_id", itemID, "installation_id", installationID, "recipient_id", recipientIDStr)
		return
	}
	p.cfg.Logger.Debug("lark patcher: inbox delivered",
		"item_id", itemID, "installation_id", installationID, "recipient_id", recipientIDStr)
}
