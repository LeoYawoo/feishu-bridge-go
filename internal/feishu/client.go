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
	"strings"
	"time"

	lark "github.com/larksuite/oapi-sdk-go/v3"
	larkcore "github.com/larksuite/oapi-sdk-go/v3/core"
	"github.com/larksuite/oapi-sdk-go/v3/event/dispatcher/callback"
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
	MessageID string
	ChatID    string
	ThreadID  string // empty for non-topic messages
	// ParentID is the message being replied to. Inside a topic the bridge
	// must reply to this id, not ThreadID: ThreadID only identifies which
	// topic, ParentID identifies where inside it.
	ParentID     string
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
//
// Feishu's callback context carries only the clicked card's open_message_id
// and the chat id - no thread id and no parent id. Anything else must be
// stamped into the button value at build time.
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
	debugCard bool

	handler *EventHandlers

	startupMS int64
}

// Options configures the client.
type Options struct {
	// LogLevel maps to larkcore.LogLevel; 0 leaves the SDK default.
	LogLevel larkcore.LogLevel
	// DebugCard logs the full card JSON of every outbound message. Off by
	// default: a card is a large nested blob and the log is meant to stay
	// scannable. Turn it on with -loglevel debug to diff what was actually
	// sent against what the Feishu webview is showing.
	DebugCard bool
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
		debugCard: opts.DebugCard,
	}
}

// Lark exposes the underlying SDK client for callers that need raw access.
func (c *Client) Lark() *lark.Client { return c.lark }

// AppID returns the configured app id.
func (c *Client) AppID() string { return c.appID }

func (c *Client) L() *log.Logger { return c.log }

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
	if c.debugCard {
		c.logCard("chat="+short(chatID)+" thread="+short(threadID), string(body))
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
	if c.debugCard {
		c.logCard("patch "+short(messageID), string(body))
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
	id, _, err := c.replyCard(ctx, sourceMessageID, card, thread)
	return id, err
}

// ReplyCardThreaded is ReplyCard plus the thread id the reply landed in.
//
// Feishu has no "create topic" endpoint: a topic comes into existence the
// moment a message is posted with reply_in_thread=true, and that message is
// its root. This is what /new uses to start a fresh topic.
func (c *Client) ReplyCardThreaded(ctx context.Context, sourceMessageID string, card map[string]any, thread bool) (messageID, threadID string, err error) {
	return c.replyCard(ctx, sourceMessageID, card, thread)
}

func (c *Client) replyCard(ctx context.Context, sourceMessageID string, card map[string]any, thread bool) (string, string, error) {
	body, err := json.Marshal(card)
	if err != nil {
		return "", "", fmt.Errorf("marshal card: %w", err)
	}
	if c.debugCard {
		c.logCard(short(sourceMessageID), string(body))
	}
	// reply_in_thread is deliberately omitted rather than passed as false:
	// with a message id as the anchor the request already targets an exact
	// message, and sending an explicit false makes Feishu re-evaluate
	// threading instead of honouring the anchor.
	bodyBuilder := larkim.NewReplyMessageReqBodyBuilder().
		MsgType("interactive").
		Content(string(body))
	if thread {
		bodyBuilder.ReplyInThread(true)
	}
	req := larkim.NewReplyMessageReqBuilder().
		MessageId(sourceMessageID).
		Body(bodyBuilder.Build()).
		Build()
	resp, err := c.lark.Im.V1.Message.Reply(ctx, req)
	if err != nil {
		return "", "", fmt.Errorf("reply message: %w", err)
	}
	if !resp.Success() {
		return "", "", fmt.Errorf("reply message: code=%d msg=%s", resp.Code, resp.Msg)
	}
	if resp.Data == nil {
		return "", "", nil
	}
	id := strval(resp.Data.MessageId)
	thr := strval(resp.Data.ThreadId)
	c.L().Printf("send reply anchor=%s thread=%v -> msg=%s thread_id=%q root=%q parent=%q",
		short(sourceMessageID), thread, short(id), thr,
		strval(resp.Data.RootId), strval(resp.Data.ParentId))
	return id, thr, nil
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

// short keeps an id readable on one log line without losing which one it is.
func short(id string) string {
	if len(id) <= 14 {
		return id
	}
	return id[:14]
}

// logCard writes one line for an outbound card. The full JSON is logged because
// the bridge's whole theory of topic routing lives inside the button value
// maps - a card that renders right can still carry the wrong anchor.
func (c *Client) logCard(where, body string) {
	c.L().Printf("send card %s buttons=%s", where, cardButtons(body))
	c.L().Printf("send card %s json=%s", where, strings.ReplaceAll(body, "\n", " "))
}

// cardButtons pulls every button's text+value out of a card JSON blob so the
// line is readable without a JSON viewer.
func cardButtons(body string) string {
	var card map[string]any
	if err := json.Unmarshal([]byte(body), &card); err != nil {
		return "<unmarshal>"
	}
	b, _ := card["body"].(map[string]any)
	elems, _ := b["elements"].([]any)
	out := make([]string, 0, len(elems))
	for _, e := range elems {
		el, _ := e.(map[string]any)
		if el == nil || el["tag"] != "button" {
			continue
		}
		t := el["text"]
		if s, ok := t.(map[string]any); ok {
			if c, ok := s["content"].(string); ok {
				t = c
			}
		}
		v, _ := json.Marshal(el["value"])
		out = append(out, fmt.Sprintf("%s=%s", t, string(v)))
	}
	if len(out) == 0 {
		return "[]"
	}
	return "{" + strings.Join(out, " ") + "}"
}

// clip bounds a logged text payload so one message cannot flood the log.
func clip(s string) string {
	s = strings.ReplaceAll(strings.ReplaceAll(s, "\n", "\\n"), "\r", "")
	if len(s) <= 120 {
		return s
	}
	return s[:120] + "…"
}

// actionName pulls the "action" key out of a card callback's value map.
func actionName(a *callback.CallBackAction) string {
	if a == nil {
		return "nil"
	}
	if v, ok := a.Value["action"].(string); ok {
		return v
	}
	return a.Tag
}

// actionValues renders a callback's value map for the log line.
func actionValues(a *callback.CallBackAction) string {
	if a == nil || a.Value == nil {
		return "nil"
	}
	b, err := json.Marshal(a.Value)
	if err != nil {
		return fmt.Sprintf("<%v>", err)
	}
	return clip(string(b))
}
