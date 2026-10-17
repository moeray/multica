package lark

// inbox_message.go builds the plain text message the Feishu bot pushes to a
// bound member when an inbox item is created. Kept separate from delivery so
// wording, truncation, and link building stay independently testable.

import (
	"net/url"
	"strings"
	"unicode/utf8"
)

// inboxBodyMaxLen bounds the body excerpt in runes. An inbox card is a
// pointer to the item, not the item itself, so a long comment or task
// summary is truncated rather than pasted whole into the chat.
const inboxBodyMaxLen = 300

// buildInboxText renders the notification as:
//
//	{title}
//	{body excerpt}
//	{link}
//
// The link segment is omitted when no app URL is configured, matching the
// WeCom inbox push: a title-only message is better than a broken link.
func buildInboxText(item map[string]any, workspaceID, slug, appURL string) string {
	title := strings.TrimSpace(inboxItemString(item, "title"))
	if title == "" {
		title = strings.TrimSpace(inboxItemString(item, "type"))
	}
	body := strings.TrimSpace(inboxItemString(item, "body"))
	if title == "" && body == "" {
		return ""
	}
	body = truncateInboxRunes(body, inboxBodyMaxLen)

	var b strings.Builder
	b.WriteString(title)
	if body != "" {
		if title != "" {
			b.WriteString("\n")
		}
		b.WriteString(body)
	}
	if link := inboxItemLink(item, workspaceID, slug, appURL); link != "" {
		b.WriteString("\n")
		b.WriteString(link)
	}
	return b.String()
}

// inboxItemLink builds {appURL}/{slug|workspaceID}/inbox?issue={issueID}.
// The query parameter is omitted for chat-only notifications, which carry no
// issue_id.
func inboxItemLink(item map[string]any, workspaceID, slug, appURL string) string {
	appURL = strings.TrimRight(strings.TrimSpace(appURL), "/")
	if appURL == "" {
		return ""
	}
	segment := slug
	if segment == "" {
		segment = workspaceID
	}
	var b strings.Builder
	b.WriteString(appURL)
	b.WriteString("/")
	b.WriteString(url.PathEscape(segment))
	b.WriteString("/inbox")
	if issueID := inboxItemString(item, "issue_id"); issueID != "" {
		b.WriteString("?issue=")
		b.WriteString(url.QueryEscape(issueID))
	}
	return b.String()
}

// inboxItemString reads a string field from the inbox payload. The in-process
// event carries *string for nullable columns; a JSON round trip carries
// string or nil.
func inboxItemString(item map[string]any, key string) string {
	switch v := item[key].(type) {
	case string:
		return v
	case *string:
		if v != nil {
			return *v
		}
	}
	return ""
}

// truncateInboxRunes trims s to at most maxRunes runes, never splitting a
// multi-byte character.
func truncateInboxRunes(s string, maxRunes int) string {
	if maxRunes <= 0 {
		return ""
	}
	if utf8.RuneCountInString(s) <= maxRunes {
		return s
	}
	i := 0
	for pos := range s {
		if i == maxRunes {
			return s[:pos]
		}
		i++
	}
	return s
}
