package service

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/events"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/dbid"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

// ErrNoSourceChatSession means the issue has no unambiguous source chat. The
// caller should keep the existing issue/inbox notification path.
var ErrNoSourceChatSession = errors.New("issue has no source chat session")

// ErrReviewResultForbidden means the caller is not an agent reviewer.
var ErrReviewResultForbidden = errors.New("only an agent reviewer can send a review result")

var issueChatOriginTypes = map[string]bool{
	"lark_chat":     true,
	"slack_chat":    true,
	"dingtalk_chat": true,
	"wecom_chat":    true,
	"telegram_chat": true,
}

type issueChatSessionQueries interface {
	GetAgentTask(ctx context.Context, id pgtype.UUID) (db.AgentTaskQueue, error)
	GetChatSession(ctx context.Context, id pgtype.UUID) (db.ChatSession, error)
}

// ResolveIssueChatSession maps an issue back to the chat conversation that
// created it. Chat-origin issues point origin_id at a chat_session; issues
// created by an agent task point origin_id at that task and inherit its
// chat_session_id. Anything else returns (nil, nil): callers must not guess.
func ResolveIssueChatSession(ctx context.Context, q issueChatSessionQueries, issue db.Issue) (*db.ChatSession, error) {
	if q == nil || !issue.OriginType.Valid || !issue.OriginID.Valid {
		return nil, nil
	}

	var sessionID pgtype.UUID
	switch {
	case issueChatOriginTypes[issue.OriginType.String]:
		sessionID = issue.OriginID
	case issue.OriginType.String == "agent_create" || issue.OriginType.String == "quick_create":
		task, err := q.GetAgentTask(ctx, issue.OriginID)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		if err != nil {
			return nil, fmt.Errorf("load origin task: %w", err)
		}
		if !task.ChatSessionID.Valid {
			return nil, nil
		}
		sessionID = task.ChatSessionID
	default:
		return nil, nil
	}

	session, err := q.GetChatSession(ctx, sessionID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("load origin chat session: %w", err)
	}
	if session.WorkspaceID != issue.WorkspaceID {
		return nil, nil
	}
	return &session, nil
}

// AgentMessageSender delivers a persisted agent message to the active channel
// route behind a chat session. The implementation is platform-agnostic; nil
// disables external delivery and leaves the message in Multica chat.
type AgentMessageSender interface {
	Send(ctx context.Context, binding db.ChannelChatSessionBinding, msg db.ChatMessage) error
}

// AgentMessageService persists server-authored assistant messages and mirrors
// them to the source channel when one is still active.
type AgentMessageService struct {
	Queries *db.Queries
	Bus     *events.Bus
	Sender  AgentMessageSender
	AppURL  string
}

func NewAgentMessageService(q *db.Queries, bus *events.Bus, sender AgentMessageSender, appURL string) *AgentMessageService {
	return &AgentMessageService{
		Queries: q,
		Bus:     bus,
		Sender:  sender,
		AppURL:  strings.TrimRight(strings.TrimSpace(appURL), "/"),
	}
}

type ReviewResultRequest struct {
	Issue     db.Issue
	ActorType string
	ActorID   string
	DedupeKey string
	Passed    bool
}

type ReviewResultResult struct {
	Message  db.ChatMessage
	Inserted bool
	// Delivered is true when the message already existed or was mirrored to
	// an active source chat. The caller suppresses the generic issue status
	// notification in that case.
	Delivered bool
}

// SendReviewResult writes one idempotent review_result message into the issue's
// source chat. The reviewer's latest issue comment supplies the conclusion
// body; the issue link stays clickable in the chat.
func (s *AgentMessageService) SendReviewResult(ctx context.Context, req ReviewResultRequest) (ReviewResultResult, error) {
	if req.ActorType != "agent" {
		return ReviewResultResult{}, ErrReviewResultForbidden
	}
	actorID, err := util.ParseUUID(req.ActorID)
	if err != nil || !actorID.Valid {
		return ReviewResultResult{}, ErrReviewResultForbidden
	}
	if s == nil || s.Queries == nil {
		return ReviewResultResult{}, errors.New("agent message service is not configured")
	}

	session, err := ResolveIssueChatSession(ctx, s.Queries, req.Issue)
	if err != nil {
		return ReviewResultResult{}, err
	}
	if session == nil {
		return ReviewResultResult{}, ErrNoSourceChatSession
	}

	dedupeKey := strings.TrimSpace(req.DedupeKey)
	if dedupeKey == "" {
		dedupeKey = fmt.Sprintf("review_result:%s:%d", util.UUIDToString(req.Issue.ID), req.Issue.Revision)
	}

	content, err := s.reviewResultContent(ctx, req)
	if err != nil {
		return ReviewResultResult{}, err
	}
	passed := req.Passed
	return s.send(ctx, *session, protocol.ChatMessageKindReviewResult, content, actorID, &passed, dedupeKey)
}

func (s *AgentMessageService) reviewResultContent(ctx context.Context, req ReviewResultRequest) (string, error) {
	actorID := util.MustParseUUID(req.ActorID)
	comment, err := s.Queries.GetLatestCommentByAuthor(ctx, db.GetLatestCommentByAuthorParams{
		IssueID:     req.Issue.ID,
		WorkspaceID: req.Issue.WorkspaceID,
		AuthorType:  "agent",
		AuthorID:    actorID,
	})
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return "", fmt.Errorf("load reviewer comment: %w", err)
	}

	verdict := "通过"
	if !req.Passed {
		verdict = "不通过"
	}
	body := strings.TrimSpace(comment.Content)
	if body == "" {
		body = "评审结论已记录在 issue 中。"
	}

	identifier := fmt.Sprintf("ISSUE-%d", req.Issue.Number)
	link := identifier
	if s.AppURL != "" {
		if workspace, err := s.Queries.GetWorkspace(ctx, req.Issue.WorkspaceID); err == nil && workspace.Slug != "" {
			identifier = fmt.Sprintf("%s-%d", workspace.IssuePrefix, req.Issue.Number)
			link = fmt.Sprintf("[%s](%s/%s/issues/%s)", identifier, s.AppURL, workspace.Slug, identifier)
		}
	}

	var b strings.Builder
	fmt.Fprintf(&b, "评审结果：%s\n\n", verdict)
	b.WriteString(body)
	b.WriteString("\n\n")
	b.WriteString(link)
	return b.String(), nil
}

func (s *AgentMessageService) send(
	ctx context.Context,
	session db.ChatSession,
	kind string,
	content string,
	actorID pgtype.UUID,
	reviewPassed *bool,
	dedupeKey string,
) (ReviewResultResult, error) {
	params := db.CreateAgentChatMessageParams{
		ChatSessionID: session.ID,
		Role:          "assistant",
		Content:       content,
		MessageKind:   kind,
		ActorAgentID:  actorID,
		DedupeKey:     pgtype.Text{String: dedupeKey, Valid: dedupeKey != ""},
		ID:            dbid.NewV7(),
	}
	if reviewPassed != nil {
		params.ReviewPassed = pgtype.Bool{Bool: *reviewPassed, Valid: true}
	}

	msg, err := s.Queries.CreateAgentChatMessage(ctx, params)
	inserted := true
	if errors.Is(err, pgx.ErrNoRows) {
		inserted = false
		msg, err = s.Queries.GetChatMessageByDedupeKey(ctx, db.GetChatMessageByDedupeKeyParams{
			ChatSessionID: session.ID,
			DedupeKey:     params.DedupeKey,
		})
	}
	if err != nil {
		return ReviewResultResult{}, fmt.Errorf("write agent chat message: %w", err)
	}

	if inserted {
		s.publishChatMessage(session, msg)
	}

	// A retry returns the original row and must not post a second external
	// message. Treat the existing row as delivered so the caller does not add a
	// duplicate generic issue notification.
	if !inserted {
		return ReviewResultResult{Message: msg, Inserted: false, Delivered: true}, nil
	}
	if s.Sender == nil {
		return ReviewResultResult{Message: msg, Inserted: true}, nil
	}

	binding, err := s.Queries.GetChannelChatSessionBindingBySessionAny(ctx, session.ID)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && binding.RetiredAt.Valid) {
		return ReviewResultResult{Message: msg, Inserted: true}, nil
	}
	if err != nil {
		return ReviewResultResult{Message: msg, Inserted: true}, fmt.Errorf("load source chat binding: %w", err)
	}
	if err := s.Sender.Send(ctx, binding, msg); err != nil {
		return ReviewResultResult{Message: msg, Inserted: true}, fmt.Errorf("deliver agent chat message: %w", err)
	}
	return ReviewResultResult{Message: msg, Inserted: true, Delivered: true}, nil
}

func (s *AgentMessageService) publishChatMessage(session db.ChatSession, msg db.ChatMessage) {
	if s.Bus == nil {
		return
	}
	payload := protocol.ChatMessagePayload{
		ChatSessionID: util.UUIDToString(session.ID),
		MessageID:     util.UUIDToString(msg.ID),
		Role:          msg.Role,
		Content:       msg.Content,
		CreatedAt:     util.TimestampToString(msg.CreatedAt),
		MessageKind:   msg.MessageKind,
		ActorAgentID:  util.UUIDToString(msg.ActorAgentID),
	}
	if msg.ReviewPassed.Valid {
		passed := msg.ReviewPassed.Bool
		payload.ReviewPassed = &passed
	}
	s.Bus.Publish(events.Event{
		Type:          protocol.EventChatMessage,
		WorkspaceID:   util.UUIDToString(session.WorkspaceID),
		ActorType:     "agent",
		ActorID:       util.UUIDToString(msg.ActorAgentID),
		ChatSessionID: util.UUIDToString(session.ID),
		Payload:       payload,
	})
}
