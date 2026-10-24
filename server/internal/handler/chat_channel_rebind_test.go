package handler

// chat_channel_rebind_test.go — MP-70: a Chat that came from an external
// channel must be able to get back onto it after an archive retired the route.
//
// Archiving used to hard-delete the binding row, which took channel_source with
// it: the header had nothing left to render a change-binding entry from, and
// unarchiving could not bring the route back. Archiving now retires the row, and
// the new POST /channel-binding endpoint reattaches a Chat to the conversation
// its own history names — provided nobody else owns that conversation now.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

type channelRebindFixture struct {
	agentID        string
	installationID string
	sessionID      string
	channelChatID  string
	bindingID      string
}

// newChannelRebindFixture seeds one Chat already bound to one Feishu p2p
// conversation, with a context generation, so archive → attach has the same
// starting state a real MP-55 Chat has.
func newChannelRebindFixture(t *testing.T, name string) channelRebindFixture {
	t.Helper()
	if testHandler == nil {
		t.Skip("database not available")
	}
	ctx := context.Background()
	agentID := createHandlerTestAgent(t, name+" agent", []byte("[]"))
	channelChatID := fmt.Sprintf("oc_rebind_%s_%d", strings.ReplaceAll(name, " ", "_"), time.Now().UnixNano())

	var installationID string
	if err := testPool.QueryRow(ctx, `
		INSERT INTO channel_installation (
			workspace_id, agent_id, channel_type, config, status, purpose, installer_user_id
		) VALUES ($1, $2, 'feishu', '{}'::jsonb, 'active', 'agent', $3)
		RETURNING id
	`, testWorkspaceID, agentID, testUserID).Scan(&installationID); err != nil {
		t.Fatalf("create channel installation: %v", err)
	}

	var sessionID string
	if err := testPool.QueryRow(ctx, `
		INSERT INTO chat_session (
			workspace_id, agent_id, creator_id, title, status, explicitly_created_at, last_read_at
		) VALUES ($1, $2, $3, $4, 'active', now(), now())
		RETURNING id
	`, testWorkspaceID, agentID, testUserID, name+" chat").Scan(&sessionID); err != nil {
		t.Fatalf("create chat session: %v", err)
	}

	var bindingID string
	if err := testPool.QueryRow(ctx, `
		INSERT INTO channel_chat_session_binding (
			chat_session_id, installation_id, channel_type, channel_chat_id, chat_type,
			last_message_id, route_revision, context_revision, history_start_message_id
		) VALUES ($1, $2, 'feishu', $3, 'p2p', 'om_rebind_last', 1, 1, 'om_rebind_start')
		RETURNING id
	`, sessionID, installationID, channelChatID).Scan(&bindingID); err != nil {
		t.Fatalf("create channel binding: %v", err)
	}
	if _, err := testPool.Exec(ctx, `
		INSERT INTO channel_chat_context_generation (
			chat_session_id, revision, history_start_message_id
		) VALUES ($1, 1, 'om_rebind_start')
	`, sessionID); err != nil {
		t.Fatalf("create channel context generation: %v", err)
	}

	t.Cleanup(func() {
		cleanupCtx := context.Background()
		testPool.Exec(cleanupCtx, `DELETE FROM channel_task_delivery WHERE installation_id = $1`, installationID)
		testPool.Exec(cleanupCtx, `DELETE FROM channel_outbound_message WHERE installation_id = $1`, installationID)
		testPool.Exec(cleanupCtx, `DELETE FROM agent_task_queue WHERE chat_session_id = $1`, sessionID)
		testPool.Exec(cleanupCtx, `
			DELETE FROM channel_chat_session_binding WHERE installation_id = $1
		`, installationID)
		testPool.Exec(cleanupCtx, `
			DELETE FROM channel_chat_context_generation WHERE chat_session_id = $1
		`, sessionID)
		testPool.Exec(cleanupCtx, `DELETE FROM chat_session WHERE id = $1`, sessionID)
		testPool.Exec(cleanupCtx, `DELETE FROM channel_installation WHERE id = $1`, installationID)
	})

	return channelRebindFixture{
		agentID:        agentID,
		installationID: installationID,
		sessionID:      sessionID,
		channelChatID:  channelChatID,
		bindingID:      bindingID,
	}
}

func setChatSessionArchivedRequest(t *testing.T, sessionID string, archived bool) *httptest.ResponseRecorder {
	t.Helper()
	req := newRequestAs(testUserID, http.MethodPatch, "/api/chat/sessions/"+sessionID+"/archive", map[string]any{
		"archived": archived,
	})
	req = withURLParam(req, "sessionId", sessionID)
	req = withChatMemberContext(t, req, testUserID)
	w := httptest.NewRecorder()
	testHandler.SetChatSessionArchived(w, req)
	return w
}

func attachChannelBindingRequest(t *testing.T, userID, sessionID string) *httptest.ResponseRecorder {
	t.Helper()
	req := newRequestAs(userID, http.MethodPost, "/api/chat/sessions/"+sessionID+"/channel-binding", nil)
	req = withURLParam(req, "sessionId", sessionID)
	req = withChatMemberContext(t, req, userID)
	w := httptest.NewRecorder()
	testHandler.AttachChatSessionChannelBinding(w, req)
	return w
}

func getChatSessionRequest(t *testing.T, userID, sessionID string) ChatSessionResponse {
	t.Helper()
	req := newRequestAs(userID, http.MethodGet, "/api/chat/sessions/"+sessionID, nil)
	req = withURLParam(req, "sessionId", sessionID)
	req = withChatMemberContext(t, req, userID)
	w := httptest.NewRecorder()
	testHandler.GetChatSession(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("GetChatSession: expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var session ChatSessionResponse
	if err := json.Unmarshal(w.Body.Bytes(), &session); err != nil {
		t.Fatalf("decode chat session: %v", err)
	}
	return session
}

// TestAttachChatSessionChannelBinding_RestoresTheRetiredRoute walks the exact
// user-reported sequence: archive a bound Chat, unarchive it, reattach it. The
// route must come back on the same external conversation, the retired row must
// survive, and inbound resolution must find the Chat again afterwards.
func TestAttachChatSessionChannelBinding_RestoresTheRetiredRoute(t *testing.T) {
	fixture := newChannelRebindFixture(t, "Restore Retired Route")
	ctx := context.Background()

	if w := setChatSessionArchivedRequest(t, fixture.sessionID, true); w.Code != http.StatusOK {
		t.Fatalf("archive: expected 200, got %d: %s", w.Code, w.Body.String())
	}
	// Archived: the Chat keeps its channel source but is not the current route,
	// and inbound resolution can no longer reach it (so the next Feishu message
	// forks a new Chat instead of reviving a read-only one).
	archived := getChatSessionRequest(t, testUserID, fixture.sessionID)
	if archived.ChannelSource == nil || archived.ChannelSource.ChannelChatID != fixture.channelChatID {
		t.Fatalf("archived chat channel_source = %+v", archived.ChannelSource)
	}
	if archived.IsCurrentChannelRoute == nil || *archived.IsCurrentChannelRoute {
		t.Fatalf("archived chat current-route flag = %+v, want false", archived.IsCurrentChannelRoute)
	}
	assertNoActiveRoute(t, fixture.installationID, fixture.channelChatID)

	if w := setChatSessionArchivedRequest(t, fixture.sessionID, false); w.Code != http.StatusOK {
		t.Fatalf("unarchive: expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if w := attachChannelBindingRequest(t, testUserID, fixture.sessionID); w.Code != http.StatusOK {
		t.Fatalf("attach: expected 200, got %d: %s", w.Code, w.Body.String())
	}

	restored := getChatSessionRequest(t, testUserID, fixture.sessionID)
	if restored.ChannelSource == nil ||
		restored.ChannelSource.ChannelChatID != fixture.channelChatID ||
		restored.ChannelSource.InstallationID != fixture.installationID {
		t.Fatalf("restored channel source = %+v", restored.ChannelSource)
	}
	if restored.IsCurrentChannelRoute == nil || !*restored.IsCurrentChannelRoute {
		t.Fatalf("restored current-route flag = %+v, want true", restored.IsCurrentChannelRoute)
	}
	if restored.ChannelSource.RouteRevision != 2 {
		t.Fatalf("restored route revision = %d, want 2 (the retired generation keeps revision 1)",
			restored.ChannelSource.RouteRevision)
	}

	var total, active int
	if err := testPool.QueryRow(ctx, `
		SELECT count(*), count(*) FILTER (WHERE retired_at IS NULL)
		FROM channel_chat_session_binding WHERE chat_session_id = $1
	`, fixture.sessionID).Scan(&total, &active); err != nil {
		t.Fatalf("count bindings: %v", err)
	}
	if total != 2 || active != 1 {
		t.Fatalf("bindings after attach = %d total / %d active, want 2/1 (history + current route)", total, active)
	}

	// Inbound resolution must now find this Chat again; that is what makes the
	// next Feishu message land here instead of forking another session.
	binding, err := testHandler.Queries.GetChannelChatSessionBinding(ctx, db.GetChannelChatSessionBindingParams{
		InstallationID: parseUUID(fixture.installationID),
		ChannelChatID:  fixture.channelChatID,
	})
	if err != nil {
		t.Fatalf("resolve inbound route after attach: %v", err)
	}
	if got := uuidToString(binding.ChatSessionID); got != fixture.sessionID {
		t.Fatalf("inbound route resolves to %s, want %s", got, fixture.sessionID)
	}

	// Repeating the attach is a successful no-op that does not advance the route.
	if w := attachChannelBindingRequest(t, testUserID, fixture.sessionID); w.Code != http.StatusOK {
		t.Fatalf("idempotent attach: expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var revision int64
	if err := testPool.QueryRow(ctx, `
		SELECT route_revision FROM channel_chat_session_binding
		WHERE chat_session_id = $1 AND retired_at IS NULL
	`, fixture.sessionID).Scan(&revision); err != nil {
		t.Fatalf("read active revision: %v", err)
	}
	if revision != 2 {
		t.Fatalf("idempotent attach advanced the route to revision %d", revision)
	}
}

// TestAttachChatSessionChannelBinding_RefusesARouteThatMovedOn covers the case
// the archive semantics exist to protect: once a retired route is picked up by
// another Chat (the next inbound message forks one), attaching here would take
// that Chat's live route away. It must be refused, and nothing may change.
func TestAttachChatSessionChannelBinding_RefusesARouteThatMovedOn(t *testing.T) {
	fixture := newChannelRebindFixture(t, "Route Moved On")
	ctx := context.Background()

	if w := setChatSessionArchivedRequest(t, fixture.sessionID, true); w.Code != http.StatusOK {
		t.Fatalf("archive: expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if w := setChatSessionArchivedRequest(t, fixture.sessionID, false); w.Code != http.StatusOK {
		t.Fatalf("unarchive: expected 200, got %d: %s", w.Code, w.Body.String())
	}

	// The forked Chat: same external conversation, new active binding.
	var otherSessionID, otherBindingID string
	if err := testPool.QueryRow(ctx, `
		INSERT INTO chat_session (
			workspace_id, agent_id, creator_id, title, status, explicitly_created_at
		) VALUES ($1, $2, $3, 'Forked chat', 'active', now())
		RETURNING id
	`, testWorkspaceID, fixture.agentID, testUserID).Scan(&otherSessionID); err != nil {
		t.Fatalf("create forked session: %v", err)
	}
	if err := testPool.QueryRow(ctx, `
		INSERT INTO channel_chat_session_binding (
			chat_session_id, installation_id, channel_type, channel_chat_id, chat_type, route_revision, context_revision
		) VALUES ($1, $2, 'feishu', $3, 'p2p', 2, 2)
		RETURNING id
	`, otherSessionID, fixture.installationID, fixture.channelChatID).Scan(&otherBindingID); err != nil {
		t.Fatalf("create forked binding: %v", err)
	}
	t.Cleanup(func() {
		cleanupCtx := context.Background()
		testPool.Exec(cleanupCtx, `DELETE FROM channel_chat_session_binding WHERE chat_session_id = $1`, otherSessionID)
		testPool.Exec(cleanupCtx, `DELETE FROM channel_chat_context_generation WHERE chat_session_id = $1`, otherSessionID)
		testPool.Exec(cleanupCtx, `DELETE FROM chat_session WHERE id = $1`, otherSessionID)
	})

	w := attachChannelBindingRequest(t, testUserID, fixture.sessionID)
	if w.Code != http.StatusConflict {
		t.Fatalf("attach onto an occupied route: expected 409, got %d: %s", w.Code, w.Body.String())
	}

	// The other Chat keeps the route, and the Chat asking to attach stays
	// retired — a refused attach must not leave a half-installed route behind.
	var otherActive bool
	if err := testPool.QueryRow(ctx, `
		SELECT retired_at IS NULL FROM channel_chat_session_binding WHERE id = $1
	`, otherBindingID).Scan(&otherActive); err != nil {
		t.Fatalf("read forked binding: %v", err)
	}
	if !otherActive {
		t.Fatal("refused attach retired the Chat that owned the route")
	}
	var stillRetired bool
	if err := testPool.QueryRow(ctx, `
		SELECT retired_at IS NOT NULL FROM channel_chat_session_binding WHERE id = $1
	`, fixture.bindingID).Scan(&stillRetired); err != nil {
		t.Fatalf("read original binding: %v", err)
	}
	if !stillRetired {
		t.Fatal("refused attach reactivated the original binding")
	}
	if binding, err := testHandler.Queries.GetChannelChatSessionBinding(ctx, db.GetChannelChatSessionBindingParams{
		InstallationID: parseUUID(fixture.installationID),
		ChannelChatID:  fixture.channelChatID,
	}); err != nil {
		t.Fatalf("resolve inbound route: %v", err)
	} else if got := uuidToString(binding.ChatSessionID); got != otherSessionID {
		t.Fatalf("inbound route resolves to %s, want the forked chat %s", got, otherSessionID)
	}
}

// TestAttachChatSessionChannelBinding_RequiresAGuard covers the states the
// endpoint must refuse outright: no channel history, an archived Chat (the
// read-only projection may not become a route target), and a member who does
// not own the Chat.
func TestAttachChatSessionChannelBinding_RequiresAGuard(t *testing.T) {
	ctx := context.Background()

	webOnlyID := createHandlerTestChatSession(t, createHandlerTestAgent(t, "Rebind Guard Agent", []byte("[]")))
	if w := attachChannelBindingRequest(t, testUserID, webOnlyID); w.Code != http.StatusConflict {
		t.Fatalf("web-only chat: expected 409, got %d: %s", w.Code, w.Body.String())
	}

	fixture := newChannelRebindFixture(t, "Rebind Guard")
	if w := setChatSessionArchivedRequest(t, fixture.sessionID, true); w.Code != http.StatusOK {
		t.Fatalf("archive: expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if w := attachChannelBindingRequest(t, testUserID, fixture.sessionID); w.Code != http.StatusBadRequest {
		t.Fatalf("archived chat: expected 400, got %d: %s", w.Code, w.Body.String())
	}

	if w := setChatSessionArchivedRequest(t, fixture.sessionID, false); w.Code != http.StatusOK {
		t.Fatalf("unarchive: expected 200, got %d: %s", w.Code, w.Body.String())
	}
	plainUserID := createChatBindingUser(t, "member")
	if w := attachChannelBindingRequest(t, plainUserID, fixture.sessionID); w.Code != http.StatusForbidden {
		t.Fatalf("non-owner: expected 403, got %d: %s", w.Code, w.Body.String())
	}
	var activeCount int
	if err := testPool.QueryRow(ctx, `
		SELECT count(*) FROM channel_chat_session_binding
		WHERE chat_session_id = $1 AND retired_at IS NULL
	`, fixture.sessionID).Scan(&activeCount); err != nil {
		t.Fatalf("count active bindings: %v", err)
	}
	if activeCount != 0 {
		t.Fatal("a refused attach installed an active binding")
	}

	// Workspace admins may reattach any Chat, same as the switch endpoint.
	if w := attachChannelBindingRequest(t, createChatBindingUser(t, "admin"), fixture.sessionID); w.Code != http.StatusOK {
		t.Fatalf("admin attach: expected 200, got %d: %s", w.Code, w.Body.String())
	}
}

// TestChatSessionsReportDurableChannelOrigin covers the metadata half of AC5:
// a Chat whose binding row was hard-deleted by the pre-MP-70 archive path has no
// channel_source left, but its channel-ingested messages still prove where it
// came from, so the header must still offer to reattach it.
func TestChatSessionsReportDurableChannelOrigin(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	ctx := context.Background()
	agentID := createHandlerTestAgent(t, "Channel Origin Agent", []byte("[]"))

	legacyID := createHandlerTestChatSession(t, agentID)
	if _, err := testPool.Exec(ctx, `
		INSERT INTO chat_message (chat_session_id, role, content, channel_ingested)
		VALUES ($1, 'user', 'question asked in Feishu', TRUE)
	`, legacyID); err != nil {
		t.Fatalf("seed channel-ingested message: %v", err)
	}
	plainID := createHandlerTestChatSession(t, agentID)
	if _, err := testPool.Exec(ctx, `
		INSERT INTO chat_message (chat_session_id, role, content)
		VALUES ($1, 'user', 'question typed in the browser')
	`, plainID); err != nil {
		t.Fatalf("seed web message: %v", err)
	}

	legacy := getChatSessionRequest(t, testUserID, legacyID)
	if legacy.ChannelSource != nil {
		t.Fatalf("legacy chat unexpectedly has a channel source: %+v", legacy.ChannelSource)
	}
	if !legacy.ChannelOrigin {
		t.Fatal("a Chat with channel-ingested messages lost channel_origin; the rebind entry would never render")
	}
	if legacy.IsCurrentChannelRoute != nil {
		t.Fatalf("legacy chat current-route flag = %+v, want absent", legacy.IsCurrentChannelRoute)
	}

	plain := getChatSessionRequest(t, testUserID, plainID)
	if plain.ChannelOrigin {
		t.Fatal("a first-party Chat reported channel_origin")
	}
}

func assertNoActiveRoute(t *testing.T, installationID, channelChatID string) {
	t.Helper()
	_, err := testHandler.Queries.GetChannelChatSessionBinding(context.Background(), db.GetChannelChatSessionBindingParams{
		InstallationID: parseUUID(installationID),
		ChannelChatID:  channelChatID,
	})
	if !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("archived Chat still resolves inbound traffic: err=%v", err)
	}
}
