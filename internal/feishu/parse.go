package feishu

import (
	"encoding/json"
	"fmt"
	"strings"

	larkim "github.com/larksuite/oapi-sdk-go/v3/service/im/v1"
)

// parseMessage flattens an SDK event into our normalised Message.
//
// The SDK nests the payload under evt.Event (a *P2MessageReceiveV1Data), and
// the actual content arrives in msg.Content as a JSON *string*, so we decode
// twice.
func (c *Client) parseMessage(evt *larkim.P2MessageReceiveV1) (*Message, error) {
	data := evt.Event
	if data == nil || data.Message == nil {
		return nil, fmt.Errorf("empty event")
	}
	msg := data.Message

	m := &Message{
		MessageID:    strval(msg.MessageId),
		ChatID:       strval(msg.ChatId),
		ThreadID:     strval(msg.ThreadId),
		ParentID:     strval(msg.ParentId),
		RootID:       strval(msg.RootId),
		ChatType:     strval(msg.ChatType),
		MessageType:  strval(msg.MessageType),
		CreateTimeMS: parseMS(strval(msg.CreateTime)),
	}
	if data.Sender != nil {
		m.SenderType = strval(data.Sender.SenderType)
		if id := data.Sender.SenderId; id != nil {
			m.SenderID = strval(id.OpenId)
		}
	}

	content := strval(msg.Content)
	switch m.MessageType {
	case "text":
		m.RawText = c.parseText(content, msg.Mentions, &m.Mentions)

	case "interactive":
		var card map[string]any
		if err := json.Unmarshal([]byte(content), &card); err != nil {
			return nil, fmt.Errorf("interactive content: %w", err)
		}
		m.Card = card
		if t, ok := extractCardText(card); ok {
			m.RawText = strings.TrimSpace(t)
		}

	case "merge_forward":
		// Children live behind a batch-GET API call; not wired yet. Surface
		// the fact that something was forwarded rather than dropping it.
		m.RawText = "[合并转发消息 — 逐条转发以处理]"

	default:
		// picture / media / file / sticker / post / audio — record the type
		// so the caller can decide. Only text + interactive are handled.
		m.RawText = fmt.Sprintf("[%s 消息]", m.MessageType)
	}

	return m, nil
}

// parseText decodes a text message and strips @mention placeholders.
func (c *Client) parseText(content string, mentions []*larkim.MentionEvent, out *[]Mention) string {
	var payload struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal([]byte(content), &payload); err != nil {
		c.L().Printf("text content decode failed: %v", err)
		return ""
	}
	return stripMentionKeys(payload.Text, mentions, out)
}

// stripMentionKeys removes @mention placeholders and records the mentions.
func stripMentionKeys(raw string, mentions []*larkim.MentionEvent, out *[]Mention) string {
	for _, mn := range mentions {
		if mn == nil {
			continue
		}
		key := strval(mn.Key)
		if key == "" {
			continue
		}
		raw = strings.ReplaceAll(raw, key, "")
		if out != nil {
			openID := ""
			if id := mn.Id; id != nil {
				openID = strval(id.OpenId)
			}
			*out = append(*out, Mention{Key: key, OpenID: openID, Name: strval(mn.Name)})
		}
	}
	return strings.TrimSpace(raw)
}

// extractCardText pulls a best-effort string out of an interactive card so the
// bot can treat "card containing text" like text.
func extractCardText(card map[string]any) (string, bool) {
	parts := walkCard(card, nil)
	if len(parts) == 0 {
		return "", false
	}
	return strings.Join(parts, "\n"), true
}

// walkCard collects markdown/div text from anywhere in a card structure.
func walkCard(node any, parts []string) []string {
	switch v := node.(type) {
	case map[string]any:
		if tag, _ := v["tag"].(string); tag == "markdown" {
			if s, ok := v["content"].(string); ok {
				return append(parts, s)
			}
		}
		if tag, _ := v["tag"].(string); tag == "div" {
			if txt, ok := v["text"].(map[string]any); ok {
				if s, ok := txt["content"].(string); ok {
					return append(parts, s)
				}
			}
		}
		for _, val := range v {
			parts = walkCard(val, parts)
		}
	case []any:
		for _, val := range v {
			parts = walkCard(val, parts)
		}
	}
	return parts
}

// parseMS converts a millisecond timestamp string to an int64, tolerating
// empty or malformed input.
func parseMS(s string) int64 {
	if s == "" {
		return 0
	}
	var n int64
	for _, r := range s {
		if r < '0' || r > '9' {
			return 0
		}
		n = n*10 + int64(r-'0')
	}
	return n
}
