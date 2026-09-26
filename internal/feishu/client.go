// Package feishu wraps the lark SDK for the small surface the bridge needs:
// websocket event subscription, message create/reply/patch and the card-action
// callback.
//
// All SDK calls are built against github.com/larksuite/oapi-sdk-go/v3 v3.5.3.
package feishu

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"sync"
	"time"

	lark "github.com/larksuite/oapi-sdk-go/v3"
	larkcore "github.com/larksuite/oapi-sdk-go/v3/core"
	larkim "github.com/larksuite/oapi-sdk-go/v3/service/im/v1"
)

// Domain selects the API host family.
type Domain string

const (
	DomainFeishu Domain = "feishu"
	DomainLark   Domain = "lark"
)

// Message is the normalised form of an inbound IM message.
type Message struct {
	MessageID    string
	ChatID       string
	ThreadID     string // empty for non-topic messages
	RootID       string
	ChatType     string // p2p | group | topic_group
	CreateTimeMS int64
	MessageType  string
	SenderID     string // open_id of the sender
	SenderType   string // "user" or "app"
	RawText      string // text with @mention placeholders stripped
	Mentions     []Mention
	Card         map[string]any // non-nil when the message is an interactive card
}

// Mention is a resolved @-mention.
type Mention struct {
	Key    string // placeholder key in RawText
	OpenID string
	Name   string
}

// EventHandlers receives inbound events.
type EventHandlers struct {
	// OnMessage is invoked for every IM message v1 event.
	OnMessage func(ctx context.Context, m *Message) error
	// OnCardAction is invoked for interactive card callbacks.
	OnCardAction func(ctx context.Context, req *CardAction) (*CardResponse, error)
}

// CardAction is a card interaction callback.
type CardAction struct {
	Action    map[string]any // the clicked button's value map
	Operator  string         // open_id of who clicked
	MessageID string
	ChatID    string
	Token     string
}

// CardResponse is returned synchronously to the card framework.
// A non-nil Card replaces the clicked card in place.
type CardResponse struct {
	Card  map[string]any
	Toast string
}

// Client is the shared lark client plus bridge-side state.
type Client struct {
	lark      *lark.Client
	appID     string
	appSecret string
	domain    Domain
	log       *log.Logger

	handler *EventHandlers

	mu        sync.Mutex
	botOpenID string

	startupMS int64
}

// Options configures the client.
type Options struct {
	// LogLevel maps to larkcore.LogLevel; 0 leaves the SDK default.
	LogLevel larkcore.LogLevel
}

// New builds a client for the given app credentials.
func New(appID, appSecret string, domain Domain, opts Options) *Client {
	if domain == "" {
		domain = DomainFeishu
	}
	base := lark.FeishuBaseUrl
	if domain == DomainLark {
		base = lark.LarkBaseUrl
	}

	cli := lark.NewClient(appID, appSecret,
		lark.WithOpenBaseUrl(base),
	)
	return &Client{
		lark:      cli,
		appID:     appID,
		appSecret: appSecret,
		domain:    domain,
		log:       log.Default(),
		startupMS: time.Now().UnixMilli(),
	}
}

// Lark exposes the underlying SDK client for callers that need raw access.
func (c *Client) Lark() *lark.Client { return c.lark }

// AppID returns the configured app id.
func (c *Client) AppID() string { return c.appID }

func (c *Client) L() *log.Logger { return c.log }

// SetBotOpenID seeds the bot's own open_id so group gates can match @-mentions.
// The SDK ships no bot-self endpoint, so this must come from config: send the
// bot a DM once, note the `open_id` in the logs, and fill it in.
func (c *Client) SetBotOpenID(openID string) {
	if openID != "" {
		c.mu.Lock()
		defer c.mu.Unlock()
		c.botOpenID = openID
	}
}

// BotOpenID returns the configured bot open_id, or "" if not set.
func (c *Client) BotOpenID() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.botOpenID
}

// SetHandlers installs the inbound event callbacks. Must be called before
// StartWS; handlers are set once at construction and never replaced, so no
// synchronization is needed.
func (c *Client) SetHandlers(h *EventHandlers) { c.handler = h }

// IsStartupMessage reports whether an event was created before the bridge
// connected, i.e. it is a replay rather than something the user just sent.
func (c *Client) IsStartupMessage(eventCreateTimeMS int64) bool {
	if c.startupMS == 0 || eventCreateTimeMS == 0 {
		return false
	}
	return eventCreateTimeMS < c.startupMS
}

// ---- outbound ------------------------------------------------------------

// SendCard posts a new interactive card message to a chat. Returns the new
// message_id.
func (c *Client) SendCard(ctx context.Context, chatID string, card map[string]any) (string, error) {
	return c.SendCardInThread(ctx, chatID, "", card)
}

// SendCardInThread posts a card; when threadID is non-empty it is a topic
// group, but Feishu addresses threads through the reply API rather than on
// create, so callers should use ReplyCard for topic replies.
func (c *Client) SendCardInThread(ctx context.Context, chatID, threadID string, card map[string]any) (string, error) {
	body, err := json.Marshal(card)
	if err != nil {
		return "", fmt.Errorf("marshal card: %w", err)
	}

	req := larkim.NewCreateMessageReqBuilder().
		ReceiveIdType("chat_id").
		Body(larkim.NewCreateMessageReqBodyBuilder().
			ReceiveId(chatID).
			MsgType("interactive").
			Content(string(body)).
			Build()).
		Build()

	resp, err := c.lark.Im.V1.Message.Create(ctx, req)
	if err != nil {
		return "", fmt.Errorf("create message: %w", err)
	}
	if !resp.Success() {
		return "", fmt.Errorf("create message: code=%d msg=%s", resp.Code, resp.Msg)
	}
	return strval(resp.Data.MessageId), nil
}

// PatchCard replaces the content of an existing interactive message.
func (c *Client) PatchCard(ctx context.Context, messageID string, card map[string]any) error {
	body, err := json.Marshal(card)
	if err != nil {
		return fmt.Errorf("marshal card: %w", err)
	}
	req := larkim.NewPatchMessageReqBuilder().
		MessageId(messageID).
		Body(larkim.NewPatchMessageReqBodyBuilder().Content(string(body)).Build()).
		Build()

	resp, err := c.lark.Im.V1.Message.Patch(ctx, req)
	if err != nil {
		return fmt.Errorf("patch message: %w", err)
	}
	if !resp.Success() {
		return fmt.Errorf("patch message: code=%d msg=%s", resp.Code, resp.Msg)
	}
	return nil
}

// SendText posts a plain text message. Used for cheap error replies.
func (c *Client) SendText(ctx context.Context, chatID, text string) (string, error) {
	content, _ := json.Marshal(map[string]string{"text": text})
	req := larkim.NewCreateMessageReqBuilder().
		ReceiveIdType("chat_id").
		Body(larkim.NewCreateMessageReqBodyBuilder().
			ReceiveId(chatID).
			MsgType("text").
			Content(string(content)).
			Build()).
		Build()
	resp, err := c.lark.Im.V1.Message.Create(ctx, req)
	if err != nil {
		return "", fmt.Errorf("create message: %w", err)
	}
	if !resp.Success() {
		return "", fmt.Errorf("create message: code=%d msg=%s", resp.Code, resp.Msg)
	}
	return strval(resp.Data.MessageId), nil
}

// ReplyCard replies to a specific message with a card, threading when asked.
// This is the only supported way to post into a topic/thread.
func (c *Client) ReplyCard(ctx context.Context, sourceMessageID string, card map[string]any, thread bool) (string, error) {
	body, err := json.Marshal(card)
	if err != nil {
		return "", fmt.Errorf("marshal card: %w", err)
	}
	req := larkim.NewReplyMessageReqBuilder().
		MessageId(sourceMessageID).
		Body(larkim.NewReplyMessageReqBodyBuilder().
			MsgType("interactive").
			Content(string(body)).
			ReplyInThread(thread).
			Build()).
		Build()
	resp, err := c.lark.Im.V1.Message.Reply(ctx, req)
	if err != nil {
		return "", fmt.Errorf("reply message: %w", err)
	}
	if !resp.Success() {
		return "", fmt.Errorf("reply message: code=%d msg=%s", resp.Code, resp.Msg)
	}
	return strval(resp.Data.MessageId), nil
}

// ReplyText replies to a message with plain text.
func (c *Client) ReplyText(ctx context.Context, sourceMessageID, text string) (string, error) {
	content, _ := json.Marshal(map[string]string{"text": text})
	req := larkim.NewReplyMessageReqBuilder().
		MessageId(sourceMessageID).
		Body(larkim.NewReplyMessageReqBodyBuilder().
			MsgType("text").
			Content(string(content)).
			Build()).
		Build()
	resp, err := c.lark.Im.V1.Message.Reply(ctx, req)
	if err != nil {
		return "", fmt.Errorf("reply message: %w", err)
	}
	if !resp.Success() {
		return "", fmt.Errorf("reply message: code=%d msg=%s", resp.Code, resp.Msg)
	}
	return strval(resp.Data.MessageId), nil
}

// strval dereferences the SDK's *string pointers, tolerating nil.
func strval(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
