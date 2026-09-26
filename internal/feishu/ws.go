package feishu

import (
	"context"
	"fmt"

	"github.com/larksuite/oapi-sdk-go/v3/event/dispatcher"
	"github.com/larksuite/oapi-sdk-go/v3/event/dispatcher/callback"
	larkim "github.com/larksuite/oapi-sdk-go/v3/service/im/v1"
	larkws "github.com/larksuite/oapi-sdk-go/v3/ws"
)

// StartWS blocks, dispatching events from the Feishu long-connection gateway.
// The SDK handles reconnects; this only returns on fatal error.
func (c *Client) StartWS(ctx context.Context) error {
	// Empty verification token / encrypt key: WS mode performs no signature
	// verification, unlike the HTTP callback mode.
	d := dispatcher.NewEventDispatcher("", "").
		OnP2MessageReceiveV1(c.onReceiveV1).
		OnP2CardActionTrigger(c.onCardAction)

	wsClient := larkws.NewClient(c.appID, c.appSecret,
		larkws.WithEventHandler(d),
		larkws.WithAutoReconnect(true),
	)

	c.L().Printf("connecting to Feishu websocket (domain=%s)...", c.domain)
	if err := wsClient.Start(ctx); err != nil {
		return fmt.Errorf("websocket: %w", err)
	}
	return nil
}

func (c *Client) onReceiveV1(ctx context.Context, evt *larkim.P2MessageReceiveV1) error {
	m, err := c.parseMessage(evt)
	if err != nil {
		c.L().Printf("parse message: %v", err)
		return nil // never let a parse failure kill the connection
	}
	if h := c.handler; h != nil && h.OnMessage != nil {
		return h.OnMessage(ctx, m)
	}
	return nil
}

func (c *Client) onCardAction(ctx context.Context, evt *callback.CardActionTriggerEvent) (*callback.CardActionTriggerResponse, error) {
	if h := c.handler; h == nil || h.OnCardAction == nil || evt == nil || evt.Event == nil {
		return nil, nil
	}
	handler := c.handler
	req := evt.Event

	ca := &CardAction{
		Operator:  req.Operator.OpenID,
		MessageID: "",
		ChatID:    "",
		Token:     req.Token,
	}
	if req.Context != nil {
		ca.MessageID = req.Context.OpenMessageID
		ca.ChatID = req.Context.OpenChatID
	}
	if req.Action != nil {
		ca.Action = req.Action.Value
	}

	out, err := handler.OnCardAction(ctx, ca)
	if err != nil || out == nil {
		return nil, nil
	}

	resp := &callback.CardActionTriggerResponse{}
	if out.Toast != "" {
		resp.Toast = &callback.Toast{Type: "info", Content: out.Toast}
	}
	if out.Card != nil {
		resp.Card = &callback.Card{Type: "raw", Data: out.Card}
	}
	return resp, nil
}
