package lark

// inbox_message.go builds the Feishu message the bot pushes to a bound member
// when an inbox item is created. Kept separate from delivery so wording,
// truncation, and link building stay independently testable.

import (
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
)

// inboxBodyMaxLen bounds the fallback plain-text excerpt in runes. The card
// path carries the full body (up to inboxCardMaxBytes); this limit only keeps
// the legacy text fallback readable when the interactive card send fails.
const inboxBodyMaxLen = 300

// inboxCardMaxBytes bounds the JSON payload of an interactive card. Feishu
// rejects card bodies around 30 KB, so the builder trims the markdown body
// (never the trailing link) to stay safely below that limit.
const inboxCardMaxBytes = 24 * 1024

const inboxCardLinkLabel = "查看详情"

// inboxCardSummaryMaxLen keeps the chat-list preview to a single short line.
const inboxCardSummaryMaxLen = 120

// inboxCard is the content the card renderer needs. Markdown is the body with
// the detail link already appended; renderInboxCard trims it by rune count when
// the encoded card would exceed the transport budget.
type inboxCard struct {
	Title    string
	Markdown string
	Summary  string
}

// buildInboxCard renders the notification card content:
//
//	header:  {title}
//	body:    {body}
//	         [查看详情](link)
//
// The body is intentionally not capped at inboxBodyMaxLen: the card is the
// primary notification and carries the agent's full result. The size guard is
// applied to the encoded card by renderInboxCard. The link segment is omitted
// when no app URL is configured or the workspace slug is unknown, matching
// buildInboxText.
func buildInboxCard(item map[string]any, slug, appURL string) inboxCard {
	title := strings.TrimSpace(inboxItemString(item, "title"))
	body := strings.TrimSpace(inboxItemString(item, "body"))
	if title == "" && body == "" {
		return inboxCard{}
	}

	summary := title
	if summary == "" {
		summary = firstInboxLine(body)
	}
	summary = truncateInboxRunes(summary, inboxCardSummaryMaxLen)
	header := title
	if header == "" {
		header = summary
	}

	var markdown strings.Builder
	markdown.WriteString(body)
	if link := inboxItemLink(item, slug, appURL); link != "" {
		if markdown.Len() > 0 {
			markdown.WriteString("\n\n")
		}
		markdown.WriteString("[")
		markdown.WriteString(inboxCardLinkLabel)
		markdown.WriteString("](")
		markdown.WriteString(link)
		markdown.WriteString(")")
	}

	return inboxCard{Title: header, Markdown: markdown.String(), Summary: summary}
}

// renderInboxCard builds the schema-2.0 card JSON and takes care of the
// transport budget. When the card is too large it keeps the trailing detail
// link, truncates the body by runes, and appends an ellipsis.
func renderInboxCard(card inboxCard, maxBytes int) ([]byte, error) {
	raw, err := marshalInboxCard(card)
	if err != nil {
		return nil, err
	}
	if maxBytes <= 0 || len(raw) <= maxBytes {
		return raw, nil
	}

	body, link := splitInboxCardMarkdown(card.Markdown)
	runes := []rune(body)
	if len(runes) == 0 {
		return nil, fmt.Errorf("inbox card exceeds %d bytes without a truncatable body", maxBytes)
	}

	low, high := 0, len(runes)
	var best []byte
	for low <= high {
		mid := (low + high) / 2
		candidate := card
		candidate.Markdown = truncateInboxBody(runes, mid) + link
		encoded, err := marshalInboxCard(candidate)
		if err != nil {
			return nil, err
		}
		if len(encoded) <= maxBytes {
			best = encoded
			low = mid + 1
		} else {
			high = mid - 1
		}
	}
	if best == nil {
		return nil, fmt.Errorf("inbox card exceeds %d bytes even after truncation", maxBytes)
	}
	return best, nil
}

func marshalInboxCard(card inboxCard) ([]byte, error) {
	title := strings.TrimSpace(card.Title)
	summary := strings.TrimSpace(card.Summary)
	if title == "" {
		title = firstInboxLine(card.Markdown)
	}
	if summary == "" {
		summary = title
	}
	doc := map[string]any{
		"schema": "2.0",
		"header": map[string]any{
			"title": map[string]any{"tag": "plain_text", "content": title},
		},
		"body": map[string]any{
			"elements": []any{
				map[string]any{"tag": "markdown", "content": card.Markdown},
			},
		},
		"config": map[string]any{
			"summary": map[string]any{"content": summary},
		},
	}
	raw, err := json.Marshal(doc)
	if err != nil {
		return nil, fmt.Errorf("encode inbox card: %w", err)
	}
	return raw, nil
}

// splitInboxCardMarkdown separates the body from the detail link appended by
// buildInboxCard. The appended link is always the last occurrence, so a body
// that happens to contain the same markdown text stays intact.
func splitInboxCardMarkdown(markdown string) (body, link string) {
	separator := "\n\n[" + inboxCardLinkLabel + "]("
	if idx := strings.LastIndex(markdown, separator); idx >= 0 {
		return markdown[:idx], markdown[idx:]
	}
	if strings.HasPrefix(markdown, "["+inboxCardLinkLabel+"](") {
		return "", markdown
	}
	return markdown, ""
}

// truncateInboxBody keeps at most maxRunes runes and appends an ellipsis.
func truncateInboxBody(runes []rune, maxRunes int) string {
	if maxRunes >= len(runes) {
		return string(runes)
	}
	if maxRunes <= 0 {
		return "\u2026"
	}
	return string(runes[:maxRunes]) + "\u2026"
}

func firstInboxLine(s string) string {
	for _, line := range strings.Split(s, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			return line
		}
	}
	return ""
}

// buildInboxText renders the notification as:
//
//	{title}
//	{body excerpt}
//	{link}
//
// The link segment is omitted when no app URL is configured or the workspace
// slug is unknown: a title-only message is better than a broken link.
func buildInboxText(item map[string]any, slug, appURL string) string {
	title := strings.TrimSpace(inboxItemString(item, "title"))
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
	if link := inboxItemLink(item, slug, appURL); link != "" {
		b.WriteString("\n")
		b.WriteString(link)
	}
	return b.String()
}

// inboxItemLink builds {appURL}/{slug}/inbox?issue={issueID}. The web app
// routes by workspace slug only; a workspace UUID is not a valid segment, so
// an unknown slug drops the link instead of pointing at a 404. The query
// parameter is omitted for chat-only notifications, which carry no issue_id.
func inboxItemLink(item map[string]any, slug, appURL string) string {
	appURL = strings.TrimRight(strings.TrimSpace(appURL), "/")
	slug = strings.TrimSpace(slug)
	if appURL == "" || slug == "" {
		return ""
	}
	var b strings.Builder
	b.WriteString(appURL)
	b.WriteString("/")
	b.WriteString(url.PathEscape(slug))
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
// multi-byte character. The last rune is an ellipsis so the reader can tell
// the excerpt was cut.
func truncateInboxRunes(s string, maxRunes int) string {
	if maxRunes <= 0 {
		return ""
	}
	runes := []rune(s)
	if len(runes) <= maxRunes {
		return s
	}
	if maxRunes == 1 {
		return "\u2026"
	}
	return string(runes[:maxRunes-1]) + "\u2026"
}
