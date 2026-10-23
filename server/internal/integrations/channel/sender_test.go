package channel

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

type senderTestChannel struct {
	sent *OutboundMessage
}

func (c *senderTestChannel) Type() Type                    { return Type("test") }
func (c *senderTestChannel) Connect(context.Context) error { return nil }
func (c *senderTestChannel) Disconnect(context.Context) error {
	return nil
}
func (c *senderTestChannel) Capabilities() Capability { return CapText }
func (c *senderTestChannel) Send(_ context.Context, out OutboundMessage) (SendResult, error) {
	c.sent = &out
	return SendResult{MessageID: "sent"}, nil
}

type senderTestInstallations struct {
	installation db.ChannelInstallation
	err          error
}

func (q senderTestInstallations) GetChannelInstallation(context.Context, db.GetChannelInstallationParams) (db.ChannelInstallation, error) {
	return q.installation, q.err
}

func TestMessageSenderRoutesBindingToChannel(t *testing.T) {
	ch := &senderTestChannel{}
	registry := NewRegistry()
	registry.Register(Type("test"), func(Config) (Channel, error) { return ch, nil })

	sender := NewMessageSender(registry, senderTestInstallations{installation: db.ChannelInstallation{
		ID:     pgtype.UUID{Bytes: [16]byte{1}, Valid: true},
		Status: "active",
		Config: []byte(`{"app_id":"test"}`),
	}})
	binding := db.ChannelChatSessionBinding{
		ChannelType:   "test",
		ChannelChatID: "chat:thread",
		ChatType:      string(ChatTypeGroup),
		Config:        []byte(`{"chat_id":"real-chat"}`),
		LastMessageID: pgtype.Text{String: "message-1", Valid: true},
		LastThreadID:  pgtype.Text{String: "thread-1", Valid: true},
	}

	if err := sender.Send(context.Background(), binding, db.ChatMessage{Content: "review result"}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if ch.sent == nil {
		t.Fatal("channel did not receive the message")
	}
	if ch.sent.ChatID != "real-chat" || ch.sent.ThreadID != "thread-1" || ch.sent.ReplyTo != "message-1" {
		t.Fatalf("outbound target = %+v, want real-chat/thread-1/message-1", *ch.sent)
	}
	if ch.sent.ChatType != string(ChatTypeGroup) || ch.sent.Text != "review result" {
		t.Fatalf("outbound message = %+v", *ch.sent)
	}
}

func TestMessageSenderRejectsRetiredBinding(t *testing.T) {
	sender := NewMessageSender(NewRegistry(), senderTestInstallations{})
	err := sender.Send(context.Background(), db.ChannelChatSessionBinding{
		RetiredAt: pgtype.Timestamptz{Valid: true},
	}, db.ChatMessage{Content: "review result"})
	if !errors.Is(err, ErrNoActiveBinding) {
		t.Fatalf("err = %v, want ErrNoActiveBinding", err)
	}
}

func TestMessageSenderSlackThreadUsesThreadRoot(t *testing.T) {
	ch := &senderTestChannel{}
	registry := NewRegistry()
	registry.Register(Type("slack"), func(Config) (Channel, error) { return ch, nil })

	sender := NewMessageSender(registry, senderTestInstallations{installation: db.ChannelInstallation{
		ID:     pgtype.UUID{Bytes: [16]byte{1}, Valid: true},
		Status: "active",
		Config: []byte(`{"app_id":"test"}`),
	}})
	binding := db.ChannelChatSessionBinding{
		ChannelType:   "slack",
		ChannelChatID: "C1",
		ChatType:      string(ChatTypeGroup),
		LastMessageID: pgtype.Text{String: "last-reply-ts", Valid: true},
		LastThreadID:  pgtype.Text{String: "thread-root-ts", Valid: true},
	}

	if err := sender.Send(context.Background(), binding, db.ChatMessage{Content: "review result"}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if ch.sent == nil {
		t.Fatal("channel did not receive the message")
	}
	if ch.sent.ThreadID != "thread-root-ts" {
		t.Fatalf("ThreadID = %q, want the thread root", ch.sent.ThreadID)
	}
	if ch.sent.ReplyTo != "" {
		t.Fatalf("ReplyTo = %q, want empty: Slack turns it into thread_ts", ch.sent.ReplyTo)
	}
}
