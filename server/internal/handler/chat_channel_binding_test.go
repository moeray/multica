package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/multica-ai/multica/server/internal/integrations/channel"
	"github.com/multica-ai/multica/server/internal/integrations/channel/engine"
	"github.com/multica-ai/multica/server/internal/middleware"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

type channelBindingFixture struct {
	agentID        string
	installationID string
	sourceID       string
	targetID       string
	sourceBinding  string
	sourceRoute    string
}

func newChannelBindingFixture(t *testing.T, name string) channelBindingFixture {
	t.Helper()
	if testHandler == nil {
		t.Skip("database not available")
	}

	ctx := context.Background()
	agentID := createHandlerTestAgent(t, name+" agent", nil)
	var installationID string
	if err := testPool.QueryRow(ctx, `
		INSERT INTO channel_installation (
			workspace_id, agent_id, channel_type, config, status, purpose, installer_user_id
		) VALUES ($1, $2, 'feishu', '{}'::jsonb, 'active', 'agent', $3)
		RETURNING id
	`, testWorkspaceID, agentID, testUserID).Scan(&installationID); err != nil {
		t.Fatalf("create channel installation: %v", err)
	}

	createSession := func(title string) string {
		t.Helper()
		var id string
		if err := testPool.QueryRow(ctx, `
			INSERT INTO chat_session (
				workspace_id, agent_id, creator_id, title, status, explicitly_created_at, last_read_at
			) VALUES ($1, $2, $3, $4, 'active', now(), now() - interval '4 minutes')
			RETURNING id
		`, testWorkspaceID, agentID, testUserID, title).Scan(&id); err != nil {
			t.Fatalf("create chat session %q: %v", title, err)
		}
		return id
	}
	sourceID := createSession("Source chat")
	targetID := createSession("Existing chat")

	if _, err := testPool.Exec(ctx, `
		INSERT INTO chat_message (chat_session_id, role, content, created_at)
		VALUES ($1, 'user', 'Please answer the first question', now() - interval '3 minutes'),
		       ($1, 'assistant', 'The first answer', now() - interval '2 minutes')
	`, targetID); err != nil {
		t.Fatalf("seed target transcript: %v", err)
	}
	if _, err := testPool.Exec(ctx, `
		INSERT INTO chat_message (chat_session_id, role, content, channel_context_revision, channel_ingested, created_at)
		VALUES ($1, 'user', 'Pending Feishu question', 1, TRUE, now() - interval '1 minute')
	`, sourceID); err != nil {
		t.Fatalf("seed source message: %v", err)
	}

	var sourceBindingID string
	if err := testPool.QueryRow(ctx, `
		INSERT INTO channel_chat_session_binding (
			chat_session_id, installation_id, channel_type, channel_chat_id, chat_type,
			last_message_id, route_revision, context_revision, history_start_message_id
		) VALUES ($1, $2, 'feishu', $3, 'p2p', 'om_source_last', 1, 1, 'om_source_start')
		RETURNING id
	`, sourceID, installationID, "chat-"+name).Scan(&sourceBindingID); err != nil {
		t.Fatalf("create source binding: %v", err)
	}
	if _, err := testPool.Exec(ctx, `
		INSERT INTO channel_chat_context_generation (
			chat_session_id, revision, history_start_message_id
		) VALUES ($1, 1, 'om_source_start')
	`, sourceID); err != nil {
		t.Fatalf("create source generation: %v", err)
	}
	// The target already has route history. The switch must reuse this generation
	// instead of resetting the Chat's transcript/provider continuity.
	if _, err := testPool.Exec(ctx, `
		INSERT INTO channel_chat_session_binding (
			chat_session_id, installation_id, channel_type, channel_chat_id, chat_type,
			last_message_id, route_revision, context_revision, retired_at,
			history_start_message_id, history_end_message_id
		) VALUES ($1, $2, 'feishu', $3, 'p2p', 'om_target_last', 1, 2, now(),
		          'om_target_start', 'om_target_end')
	`, targetID, installationID, "old-"+name); err != nil {
		t.Fatalf("create target binding history: %v", err)
	}
	if _, err := testPool.Exec(ctx, `
		INSERT INTO channel_chat_context_generation (
			chat_session_id, revision, history_start_message_id, history_end_message_id
		) VALUES ($1, 2, 'om_target_start', 'om_target_end')
	`, targetID); err != nil {
		t.Fatalf("create target generation: %v", err)
	}

	t.Cleanup(func() {
		cleanupCtx := context.Background()
		testPool.Exec(cleanupCtx, `
			DELETE FROM channel_task_delivery
			WHERE installation_id = $1
		`, installationID)
		testPool.Exec(cleanupCtx, `
			DELETE FROM channel_outbound_message
			WHERE installation_id = $1
		`, installationID)
		testPool.Exec(cleanupCtx, `
			DELETE FROM agent_task_queue
			WHERE chat_session_id IN ($1, $2)
		`, sourceID, targetID)
		testPool.Exec(cleanupCtx, `
			DELETE FROM channel_chat_session_binding
			WHERE installation_id = $1
		`, installationID)
		testPool.Exec(cleanupCtx, `
			DELETE FROM channel_chat_context_generation
			WHERE chat_session_id IN ($1, $2)
		`, sourceID, targetID)
		testPool.Exec(cleanupCtx, `
			DELETE FROM chat_session WHERE id IN ($1, $2)
		`, sourceID, targetID)
		testPool.Exec(cleanupCtx, `DELETE FROM channel_installation WHERE id = $1`, installationID)
	})

	return channelBindingFixture{
		agentID:        agentID,
		installationID: installationID,
		sourceID:       sourceID,
		targetID:       targetID,
		sourceBinding:  sourceBindingID,
		sourceRoute:    "chat-" + name,
	}
}

func withChatMemberContext(t *testing.T, req *http.Request, userID string) *http.Request {
	t.Helper()
	member, err := testHandler.Queries.GetMemberByUserAndWorkspace(context.Background(), db.GetMemberByUserAndWorkspaceParams{
		UserID:      util.MustParseUUID(userID),
		WorkspaceID: util.MustParseUUID(testWorkspaceID),
	})
	if err != nil {
		t.Fatalf("load member %s: %v", userID, err)
	}
	return req.WithContext(middleware.SetMemberContext(req.Context(), testWorkspaceID, member))
}

func switchChannelBindingRequest(
	t *testing.T,
	userID, targetID, installationID, channelChatID string,
) *httptest.ResponseRecorder {
	t.Helper()
	req := newRequestAs(userID, http.MethodPut, "/api/chat/sessions/"+targetID+"/channel-binding", map[string]any{
		"installation_id": installationID,
		"channel_chat_id": channelChatID,
	})
	req = withURLParam(req, "sessionId", targetID)
	req = withChatMemberContext(t, req, userID)
	w := httptest.NewRecorder()
	testHandler.SwitchChatSessionChannelBinding(w, req)
	return w
}

func TestSwitchChatSessionChannelBinding_MovesRouteAndPreservesTarget(t *testing.T) {
	fixture := newChannelBindingFixture(t, "success")

	var titleBefore string
	var unreadBefore int
	var lastReadBefore time.Time
	if err := testPool.QueryRow(context.Background(), `
		SELECT title, last_read_at,
		       (SELECT count(*) FROM chat_message
		         WHERE chat_session_id = $1 AND role = 'assistant'
		           AND created_at > chat_session.last_read_at)
		FROM chat_session WHERE id = $1
	`, fixture.targetID).Scan(&titleBefore, &lastReadBefore, &unreadBefore); err != nil {
		t.Fatalf("read target before switch: %v", err)
	}

	w := switchChannelBindingRequest(t, testUserID, fixture.targetID, fixture.installationID, fixture.sourceRoute)
	if w.Code != http.StatusOK {
		t.Fatalf("SwitchChatSessionChannelBinding: expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var response ChatSessionResponse
	if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode switch response: %v", err)
	}
	if response.ID != fixture.targetID || response.ChannelSource == nil ||
		response.ChannelSource.ChatType != "p2p" ||
		response.ChannelSource.ChannelChatID != fixture.sourceRoute ||
		response.ChannelSource.RouteRevision != 2 ||
		response.IsCurrentChannelRoute == nil || !*response.IsCurrentChannelRoute {
		t.Fatalf("switch response metadata = %+v", response)
	}

	var (
		titleAfter     string
		unreadAfter    int
		lastReadAfter  time.Time
		sourceRetired  bool
		sourceHistory  string
		targetRevision int64
		targetContext  int64
		bindingCount   int
	)
	if err := testPool.QueryRow(context.Background(), `
		SELECT title, last_read_at,
		       (SELECT count(*) FROM chat_message
		         WHERE chat_session_id = $1 AND role = 'assistant'
		           AND created_at > chat_session.last_read_at)
		FROM chat_session WHERE id = $1
	`, fixture.targetID).Scan(&titleAfter, &lastReadAfter, &unreadAfter); err != nil {
		t.Fatalf("read target after switch: %v", err)
	}
	if titleAfter != titleBefore || unreadAfter != unreadBefore || !lastReadAfter.Equal(lastReadBefore) {
		t.Fatalf("target changed: before=(%q,%d,%s) after=(%q,%d,%s)",
			titleBefore, unreadBefore, lastReadBefore, titleAfter, unreadAfter, lastReadAfter)
	}
	if err := testPool.QueryRow(context.Background(), `
		SELECT retired_at IS NOT NULL, history_end_message_id::text
		FROM channel_chat_session_binding WHERE id = $1
	`, fixture.sourceBinding).Scan(&sourceRetired, &sourceHistory); err != nil {
		t.Fatalf("read retired source binding: %v", err)
	}
	if !sourceRetired || sourceHistory != "om_source_last" {
		t.Fatalf("source binding retired=%v history_end=%v", sourceRetired, sourceHistory)
	}
	if err := testPool.QueryRow(context.Background(), `
		SELECT route_revision, context_revision
		FROM channel_chat_session_binding
		WHERE chat_session_id = $1 AND retired_at IS NULL
	`, fixture.targetID).Scan(&targetRevision, &targetContext); err != nil {
		t.Fatalf("read active target binding: %v", err)
	}
	if targetRevision != 2 || targetContext != 2 {
		t.Fatalf("target binding revision/context = %d/%d, want 2/2", targetRevision, targetContext)
	}
	if err := testPool.QueryRow(context.Background(), `
		SELECT count(*) FROM channel_chat_session_binding
		WHERE chat_session_id = $1
	`, fixture.targetID).Scan(&bindingCount); err != nil {
		t.Fatalf("count target bindings: %v", err)
	}
	if bindingCount != 2 {
		t.Fatalf("target binding count = %d, want active + one historical", bindingCount)
	}

	// Repeating the same request is a successful no-op.
	w = switchChannelBindingRequest(t, testUserID, fixture.targetID, fixture.installationID, fixture.sourceRoute)
	if w.Code != http.StatusOK {
		t.Fatalf("idempotent switch: expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var afterIdempotentRevision int64
	if err := testPool.QueryRow(context.Background(), `
		SELECT route_revision FROM channel_chat_session_binding
		WHERE chat_session_id = $1 AND retired_at IS NULL
	`, fixture.targetID).Scan(&afterIdempotentRevision); err != nil {
		t.Fatalf("read revision after idempotent switch: %v", err)
	}
	if afterIdempotentRevision != 2 {
		t.Fatalf("idempotent switch advanced revision to %d", afterIdempotentRevision)
	}

	// The list projection must now mark B current and A historical.
	req := newRequest(http.MethodGet, "/api/chat/sessions?status=all", nil)
	req = withChatTestWorkspaceCtx(t, req)
	listW := httptest.NewRecorder()
	testHandler.ListChatSessions(listW, req)
	if listW.Code != http.StatusOK {
		t.Fatalf("ListChatSessions: expected 200, got %d: %s", listW.Code, listW.Body.String())
	}
	var sessions []ChatSessionResponse
	if err := json.Unmarshal(listW.Body.Bytes(), &sessions); err != nil {
		t.Fatalf("decode sessions: %v", err)
	}
	byID := make(map[string]ChatSessionResponse, len(sessions))
	for _, session := range sessions {
		byID[session.ID] = session
	}
	source, sourceOK := byID[fixture.sourceID]
	target, targetOK := byID[fixture.targetID]
	if !sourceOK || !targetOK {
		t.Fatalf("list missing source/target: %+v", byID)
	}
	if source.IsCurrentChannelRoute == nil || *source.IsCurrentChannelRoute {
		t.Fatalf("source route current flag = %+v", source.IsCurrentChannelRoute)
	}
	if source.ChannelSource == nil || source.ChannelSource.ChannelChatID != fixture.sourceRoute {
		t.Fatalf("source channel source = %+v", source.ChannelSource)
	}
	if target.IsCurrentChannelRoute == nil || !*target.IsCurrentChannelRoute {
		t.Fatalf("target route current flag = %+v", target.IsCurrentChannelRoute)
	}
	if target.ChannelSource == nil || target.ChannelSource.ChannelChatID != fixture.sourceRoute {
		t.Fatalf("target channel source = %+v", target.ChannelSource)
	}
}

func TestSwitchChatSessionChannelBinding_AllowsRetiredRouteTaskToFinish(t *testing.T) {
	fixture := newChannelBindingFixture(t, "pending")
	if w := switchChannelBindingRequest(t, testUserID, fixture.targetID, fixture.installationID, fixture.sourceRoute); w.Code != http.StatusOK {
		t.Fatalf("switch: %d %s", w.Code, w.Body.String())
	}

	sourceSession, err := testHandler.Queries.GetChatSession(context.Background(), util.MustParseUUID(fixture.sourceID))
	if err != nil {
		t.Fatalf("reload source session: %v", err)
	}
	task, err := testHandler.TaskService.EnqueueChannelChatTask(
		context.Background(),
		sourceSession,
		util.MustParseUUID(testUserID),
		false,
		1,
		util.MustParseUUID(fixture.sourceBinding),
		1,
	)
	if err != nil {
		t.Fatalf("enqueue retired-route task: %v", err)
	}
	var deliveryRoute string
	var deliveryRevision int64
	if err := testPool.QueryRow(context.Background(), `
		SELECT channel_chat_id, route_revision
		FROM channel_task_delivery WHERE task_id = $1
	`, uuidToString(task.ID)).Scan(&deliveryRoute, &deliveryRevision); err != nil {
		t.Fatalf("read task delivery: %v", err)
	}
	if deliveryRoute != fixture.sourceRoute || deliveryRevision != 1 {
		t.Fatalf("delivery route = %s@%d, want %s@1", deliveryRoute, deliveryRevision, fixture.sourceRoute)
	}
}

func TestSwitchChatSessionChannelBinding_ThenClearAdvancesOnlyTheActiveRoute(t *testing.T) {
	fixture := newChannelBindingFixture(t, "clear-after-switch")
	if w := switchChannelBindingRequest(t, testUserID, fixture.targetID, fixture.installationID, fixture.sourceRoute); w.Code != http.StatusOK {
		t.Fatalf("switch: %d %s", w.Code, w.Body.String())
	}

	// The target now has an active and a retired binding on the same context
	// revision. A /clear must advance only the active route, or the generation
	// INSERT produces two rows for the same (chat_session_id, revision).
	session := engine.NewChatSession(testHandler.Queries, testHandler.TxStarter, channel.Type("feishu"), engine.SessionTitles{})
	if _, err := session.AppendUserMessage(context.Background(), engine.AppendInput{
		SessionID:      util.MustParseUUID(fixture.targetID),
		Sender:         util.MustParseUUID(testUserID),
		InstallationID: util.MustParseUUID(fixture.installationID),
		Body:           "/clear",
		CommandText:    "/clear",
		ForceFresh:     true,
		MessageID:      "om_clear_after_switch",
	}); err != nil {
		t.Fatalf("clear after switch: %v", err)
	}

	var activeRevision, retiredRevision, generationCount int64
	if err := testPool.QueryRow(context.Background(), `
		SELECT
			(SELECT context_revision FROM channel_chat_session_binding
			  WHERE chat_session_id = $1 AND retired_at IS NULL),
			(SELECT context_revision FROM channel_chat_session_binding
			  WHERE chat_session_id = $1 AND retired_at IS NOT NULL
			  ORDER BY route_revision DESC LIMIT 1),
			(SELECT count(*) FROM channel_chat_context_generation
			  WHERE chat_session_id = $1 AND revision = 3)
	`, fixture.targetID).Scan(&activeRevision, &retiredRevision, &generationCount); err != nil {
		t.Fatalf("read context after clear: %v", err)
	}
	if activeRevision != 3 {
		t.Errorf("active binding context revision = %d, want 3", activeRevision)
	}
	if retiredRevision != 2 {
		t.Errorf("retired binding context revision = %d, want 2", retiredRevision)
	}
	if generationCount != 1 {
		t.Errorf("generation rows for revision 3 = %d, want 1", generationCount)
	}
}

func TestSwitchChatSessionChannelBinding_RejectsConflictsAndUnauthorizedCallers(t *testing.T) {
	fixture := newChannelBindingFixture(t, "reject")
	ctx := context.Background()

	plainUserID := createChatBindingUser(t, "member")
	adminUserID := createChatBindingUser(t, "admin")

	// Plain member with ordinary agent access cannot move someone else's binding.
	if w := switchChannelBindingRequest(t, plainUserID, fixture.targetID, fixture.installationID, fixture.sourceRoute); w.Code != http.StatusForbidden {
		t.Fatalf("plain member: expected 403, got %d: %s", w.Code, w.Body.String())
	}
	var stillSourceActive bool
	if err := testPool.QueryRow(ctx, `
		SELECT retired_at IS NULL FROM channel_chat_session_binding WHERE id = $1
	`, fixture.sourceBinding).Scan(&stillSourceActive); err != nil {
		t.Fatalf("read binding after denied request: %v", err)
	}
	if !stillSourceActive {
		t.Fatal("denied request retired the source binding")
	}

	// Workspace admin can move it.
	if w := switchChannelBindingRequest(t, adminUserID, fixture.targetID, fixture.installationID, fixture.sourceRoute); w.Code != http.StatusOK {
		t.Fatalf("admin switch: expected 200, got %d: %s", w.Code, w.Body.String())
	}

	// A second target that already owns another active route is a conflict.
	otherAgentID := createHandlerTestAgent(t, "reject other agent", nil)
	var conflictingSessionID string
	if err := testPool.QueryRow(ctx, `
		INSERT INTO chat_session (workspace_id, agent_id, creator_id, title, status, explicitly_created_at)
		VALUES ($1, $2, $3, 'Conflicting chat', 'active', now()) RETURNING id
	`, testWorkspaceID, fixture.agentID, testUserID).Scan(&conflictingSessionID); err != nil {
		t.Fatalf("create conflict session: %v", err)
	}
	if _, err := testPool.Exec(ctx, `
		INSERT INTO channel_chat_session_binding (
			chat_session_id, installation_id, channel_type, channel_chat_id, chat_type,
			route_revision, context_revision
		) VALUES ($1, $2, 'feishu', $3, 'p2p', 1, 1)
	`, conflictingSessionID, fixture.installationID, "other-route"); err != nil {
		t.Fatalf("create conflict binding: %v", err)
	}
	if _, err := testPool.Exec(ctx, `
		INSERT INTO channel_chat_context_generation (chat_session_id, revision)
		VALUES ($1, 1)
	`, conflictingSessionID); err != nil {
		t.Fatalf("create conflict generation: %v", err)
	}
	t.Cleanup(func() {
		testPool.Exec(context.Background(), `DELETE FROM channel_chat_session_binding WHERE chat_session_id = $1`, conflictingSessionID)
		testPool.Exec(context.Background(), `DELETE FROM channel_chat_context_generation WHERE chat_session_id = $1`, conflictingSessionID)
		testPool.Exec(context.Background(), `DELETE FROM chat_session WHERE id = $1`, conflictingSessionID)
	})
	if w := switchChannelBindingRequest(t, testUserID, conflictingSessionID, fixture.installationID, fixture.sourceRoute); w.Code != http.StatusConflict {
		t.Fatalf("target conflict: expected 409, got %d: %s", w.Code, w.Body.String())
	}

	// Wrong agent and inactive installation are both refused before mutation.
	wrongAgentSessionID := createHandlerTestChatSession(t, otherAgentID)
	if w := switchChannelBindingRequest(t, testUserID, wrongAgentSessionID, fixture.installationID, fixture.sourceRoute); w.Code != http.StatusBadRequest {
		t.Fatalf("wrong agent: expected 400, got %d: %s", w.Code, w.Body.String())
	}

	// A target from another workspace is invisible to this workspace's route.
	var otherWorkspaceID, foreignSessionID string
	if err := testPool.QueryRow(ctx, `
		INSERT INTO workspace (name, slug, description, issue_prefix)
		VALUES ('Binding foreign workspace', $1, '', 'BND')
		RETURNING id
	`, fmt.Sprintf("binding-foreign-%d", time.Now().UnixNano())).Scan(&otherWorkspaceID); err != nil {
		t.Fatalf("create foreign workspace: %v", err)
	}
	if err := testPool.QueryRow(ctx, `
		INSERT INTO chat_session (workspace_id, agent_id, creator_id, title, status, explicitly_created_at)
		VALUES ($1, $2, $3, 'Foreign chat', 'active', now()) RETURNING id
	`, otherWorkspaceID, fixture.agentID, testUserID).Scan(&foreignSessionID); err != nil {
		t.Fatalf("create foreign session: %v", err)
	}
	t.Cleanup(func() {
		testPool.Exec(context.Background(), `DELETE FROM chat_session WHERE id = $1`, foreignSessionID)
		testPool.Exec(context.Background(), `DELETE FROM workspace WHERE id = $1`, otherWorkspaceID)
	})
	if w := switchChannelBindingRequest(t, testUserID, foreignSessionID, fixture.installationID, fixture.sourceRoute); w.Code != http.StatusNotFound {
		t.Fatalf("cross-workspace target: expected 404, got %d: %s", w.Code, w.Body.String())
	}

	archivedSessionID := createHandlerTestChatSession(t, fixture.agentID)
	if _, err := testPool.Exec(ctx, `UPDATE chat_session SET status = 'archived' WHERE id = $1`, archivedSessionID); err != nil {
		t.Fatalf("archive target: %v", err)
	}
	if w := switchChannelBindingRequest(t, testUserID, archivedSessionID, fixture.installationID, fixture.sourceRoute); w.Code != http.StatusBadRequest {
		t.Fatalf("archived target: expected 400, got %d: %s", w.Code, w.Body.String())
	}

	if _, err := testPool.Exec(ctx, `UPDATE channel_installation SET status = 'revoked' WHERE id = $1`, fixture.installationID); err != nil {
		t.Fatalf("revoke installation: %v", err)
	}
	if w := switchChannelBindingRequest(t, testUserID, fixture.targetID, fixture.installationID, fixture.sourceRoute); w.Code != http.StatusBadRequest {
		t.Fatalf("inactive installation: expected 400, got %d: %s", w.Code, w.Body.String())
	}
}

func TestSwitchChatSessionChannelBinding_RequiresOwnershipOfBothSessions(t *testing.T) {
	t.Run("source creator can move their own route to their own chat", func(t *testing.T) {
		fixture := newChannelBindingFixture(t, "owner-both")
		memberID := createChatBindingUser(t, "member")
		ctx := context.Background()
		if _, err := testPool.Exec(ctx, `
			UPDATE chat_session SET creator_id = $2 WHERE id IN ($1, $3)
		`, fixture.sourceID, memberID, fixture.targetID); err != nil {
			t.Fatalf("assign both chats to member: %v", err)
		}

		if w := switchChannelBindingRequest(t, memberID, fixture.targetID, fixture.installationID, fixture.sourceRoute); w.Code != http.StatusOK {
			t.Fatalf("source creator switch: expected 200, got %d: %s", w.Code, w.Body.String())
		}
	})

	t.Run("source creator cannot move the route into another member's chat", func(t *testing.T) {
		fixture := newChannelBindingFixture(t, "owner-source-only")
		memberID := createChatBindingUser(t, "member")
		ctx := context.Background()
		if _, err := testPool.Exec(ctx, `
			UPDATE chat_session SET creator_id = $2 WHERE id = $1
		`, fixture.sourceID, memberID); err != nil {
			t.Fatalf("assign source chat to member: %v", err)
		}

		if w := switchChannelBindingRequest(t, memberID, fixture.targetID, fixture.installationID, fixture.sourceRoute); w.Code != http.StatusForbidden {
			t.Fatalf("source creator with foreign target: expected 403, got %d: %s", w.Code, w.Body.String())
		}
		assertChannelBindingStillActive(t, fixture.sourceBinding)
	})

	t.Run("target creator cannot take another member's route", func(t *testing.T) {
		fixture := newChannelBindingFixture(t, "owner-target-only")
		memberID := createChatBindingUser(t, "member")
		ctx := context.Background()
		if _, err := testPool.Exec(ctx, `
			UPDATE chat_session SET creator_id = $2 WHERE id = $1
		`, fixture.targetID, memberID); err != nil {
			t.Fatalf("assign target chat to member: %v", err)
		}

		if w := switchChannelBindingRequest(t, memberID, fixture.targetID, fixture.installationID, fixture.sourceRoute); w.Code != http.StatusForbidden {
			t.Fatalf("target creator with foreign source: expected 403, got %d: %s", w.Code, w.Body.String())
		}
		assertChannelBindingStillActive(t, fixture.sourceBinding)
	})
}

func assertChannelBindingStillActive(t *testing.T, bindingID string) {
	t.Helper()
	var active bool
	if err := testPool.QueryRow(context.Background(), `
		SELECT retired_at IS NULL FROM channel_chat_session_binding WHERE id = $1
	`, bindingID).Scan(&active); err != nil {
		t.Fatalf("read binding after denied switch: %v", err)
	}
	if !active {
		t.Fatal("denied switch retired the source binding")
	}
}

func createChatBindingUser(t *testing.T, role string) string {
	t.Helper()
	ctx := context.Background()
	email := fmt.Sprintf("chat-binding-%s-%d@multica.test", strings.ReplaceAll(t.Name(), "/", "-"), time.Now().UnixNano())
	var userID string
	if err := testPool.QueryRow(ctx, `
		INSERT INTO "user" (name, email) VALUES ($1, $2) RETURNING id
	`, "Chat binding "+role, email).Scan(&userID); err != nil {
		t.Fatalf("create %s user: %v", role, err)
	}
	if _, err := testPool.Exec(ctx, `
		INSERT INTO member (workspace_id, user_id, role) VALUES ($1, $2, $3)
	`, testWorkspaceID, userID, role); err != nil {
		t.Fatalf("create %s member: %v", role, err)
	}
	t.Cleanup(func() {
		testPool.Exec(context.Background(), `DELETE FROM "user" WHERE id = $1`, userID)
	})
	return userID
}
