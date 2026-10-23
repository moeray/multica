package channel

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5/pgtype"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// ErrNoActiveBinding means the chat session no longer has an active external
// route. Callers should keep the message in Multica and use the existing issue
// notification path instead of guessing a destination.
var ErrNoActiveBinding = errors.New("chat session has no active channel binding")

// InstallationQuerier is the narrow DB seam the sender needs. *db.Queries
// satisfies it.
type InstallationQuerier interface {
	GetChannelInstallation(ctx context.Context, arg db.GetChannelInstallationParams) (db.ChannelInstallation, error)
}

// MessageSender delivers a persisted chat message to the active channel route
// behind a chat session. It builds the platform adapter from the installation
// config, so the service layer stays platform-agnostic.
type MessageSender struct {
	Registry *Registry
	Queries  InstallationQuerier
}

func NewMessageSender(registry *Registry, queries InstallationQuerier) *MessageSender {
	return &MessageSender{Registry: registry, Queries: queries}
}

func (s *MessageSender) Send(ctx context.Context, binding db.ChannelChatSessionBinding, msg db.ChatMessage) error {
	if s == nil || s.Registry == nil || s.Queries == nil {
		return errors.New("channel message sender is not configured")
	}
	if binding.RetiredAt.Valid {
		return ErrNoActiveBinding
	}

	installation, err := s.Queries.GetChannelInstallation(ctx, db.GetChannelInstallationParams{
		ID:          binding.InstallationID,
		ChannelType: binding.ChannelType,
	})
	if err != nil {
		return fmt.Errorf("load channel installation: %w", err)
	}
	if installation.Status != "active" {
		return fmt.Errorf("channel installation is %s", installation.Status)
	}

	adapter, err := s.Registry.Build(Config{
		Type: Type(binding.ChannelType),
		Raw:  installation.Config,
		ID:   installation.ID,
	})
	if err != nil {
		return fmt.Errorf("build channel adapter: %w", err)
	}

	out := OutboundMessage{
		ChatID:      bindingChatID(binding),
		ChatType:    binding.ChatType,
		Text:        msg.Content,
		RecipientID: bindingRecipientID(binding),
	}
	if binding.LastThreadID.Valid {
		out.ThreadID = binding.LastThreadID.String
		// Replying to the frozen trigger keeps topic-isolated group messages in
		// their thread; p2p bindings have no thread and stay chat-level. Slack
		// is the exception: its adapter turns ReplyTo into thread_ts, so the
		// last message would start a new thread instead of continuing the
		// binding's thread root. ThreadID alone already lands in that thread.
		if binding.ChannelType != "slack" {
			out.ReplyTo = textValue(binding.LastMessageID)
		}
	}
	if _, err := adapter.Send(ctx, out); err != nil {
		return fmt.Errorf("send channel message: %w", err)
	}
	return nil
}

func bindingRecipientID(binding db.ChannelChatSessionBinding) string {
	if len(binding.Config) == 0 {
		return ""
	}
	var cfg struct {
		StaffID string `json:"staff_id"`
	}
	if err := json.Unmarshal(binding.Config, &cfg); err != nil {
		return ""
	}
	return cfg.StaffID
}

// bindingChatID recovers the real platform conversation id when the binding
// key is a composite thread key. Every platform stores the real id under one
// of these keys in its opaque binding config.
func bindingChatID(binding db.ChannelChatSessionBinding) string {
	if len(binding.Config) > 0 {
		var cfg map[string]any
		if err := json.Unmarshal(binding.Config, &cfg); err == nil {
			for _, key := range []string{"chat_id", "channel_id", "conversation_id"} {
				if value, ok := cfg[key].(string); ok && value != "" {
					return value
				}
			}
		}
	}
	return binding.ChannelChatID
}

func textValue(value pgtype.Text) string {
	if !value.Valid {
		return ""
	}
	return value.String
}
