// Package card builds Feishu interactive cards.
//
// Cards are built as plain maps so they can be serialised directly and also
// fed to the CardKit streaming API. The v2 schema (schema: "2.0") is used
// because it is the only one that supports streaming markdown elements.
package card

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// Limits mirror Feishu's documented card constraints.
const (
	MaxDivChars  = 10_000
	MaxCardBytes = 28 * 1024
)

// StreamElementID is the element_id of the streaming markdown block. CardKit
// requires a stable id to patch a single element.
const StreamElementID = "streaming_output"

// Status is a card lifecycle state.
type Status string

const (
	StatusThinking Status = "thinking"
	StatusRunning  Status = "running"
	StatusDone     Status = "done"
	StatusError    Status = "error"
	StatusStopped  Status = "stopped"
)

// Button is one interactive action button.
type Button struct {
	Text string
	// Value is sent back verbatim in the card action callback.
	Value map[string]string
	// Style: primary | default | danger
	Style string
}

// Builder assembles a card incrementally.
type Builder struct {
	name     string
	title    string
	template string
	markdown []string
	buttons  []Button
	footer   string
	cardID   string
}

// New starts an empty builder.
func New(name string) *Builder {
	return &Builder{name: name, template: "blue", title: "🤔 处理中..."}
}

// SetTitle overrides the header.
func (b *Builder) SetTitle(title, template string) *Builder {
	b.title = title
	b.template = template
	return b
}

// Markdown appends a markdown block.
func (b *Builder) Markdown(content string) *Builder {
	b.markdown = append(b.markdown, content)
	return b
}

// Buttons appends action buttons.
func (b *Builder) Buttons(btns ...Button) *Builder {
	b.buttons = append(b.buttons, btns...)
	return b
}

// Footer adds a small usage/status line at the bottom.
func (b *Builder) Footer(s string) *Builder {
	b.footer = s
	return b
}

// CardID stamps an opaque id used to correlate action callbacks.
func (b *Builder) CardID(id string) *Builder {
	b.cardID = id
	return b
}

// Build renders the card.
func (b *Builder) Build() map[string]any {
	elements := make([]map[string]any, 0, len(b.markdown)+4)

	first := true
	for _, chunk := range b.markdown {
		content := chunk
		if len(content) > MaxDivChars {
			content = "…（前文已省略）\n\n" + content[len(content)-MaxDivChars:]
		}
		elem := map[string]any{
			"tag":     "markdown",
			"content": Sanitize(content),
		}
		// Only the primary markdown block is a CardKit streaming target.
		if first {
			elem["element_id"] = StreamElementID
			first = false
		}
		elements = append(elements, elem)
	}
	if len(b.markdown) == 0 {
		elements = append(elements, map[string]any{
			"tag": "markdown", "element_id": StreamElementID, "content": "_（空回复）_",
		})
	}

	if len(b.buttons) > 0 {
		elements = append(elements, map[string]any{
			"tag":     "action",
			"actions": buildActions(b.buttons, b.cardID),
		})
	}

	if b.footer != "" {
		elements = append(elements, map[string]any{
			"tag":  "div",
			"text": map[string]any{"tag": "lark_md", "content": b.footer},
		})
	}

	card := map[string]any{
		"schema": "2.0",
		"config": map[string]any{
			"wide_screen_mode": true,
			"update_multi":     true,
			"streaming_mode":   true,
			"summary":          map[string]any{"content": b.title},
		},
		"header": map[string]any{
			"template": b.template,
			"title":    map[string]any{"tag": "plain_text", "content": b.title},
		},
		"body": map[string]any{"elements": elements},
	}
	if b.cardID != "" {
		card["card_id"] = b.cardID
	}
	return card
}

// Bytes returns the JSON payload size, used to enforce MaxCardBytes.
func (b *Builder) Bytes() int { return len(mustJSONBytes(b.Build())) }

// mustJSONBytes marshals without escaping HTML, matching how Feishu expects
// card payloads on the wire.
func mustJSONBytes(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		return nil
	}
	return b
}

// buildActions converts Buttons into feishu action objects.
func buildActions(btns []Button, cardID string) []map[string]any {
	out := make([]map[string]any, 0, len(btns))
	for _, bt := range btns {
		style := bt.Style
		if style == "" {
			style = "default"
		}
		val := map[string]any{}
		for k, v := range bt.Value {
			val[k] = v
		}
		if cardID != "" {
			val["card_id"] = cardID
		}
		out = append(out, map[string]any{
			"tag":   "button",
			"text":  map[string]any{"tag": "plain_text", "content": bt.Text},
			"type":  style,
			"value": val,
		})
	}
	return out
}

// ---- purpose-built card factories ----------------------------------------

// Processing is the "thinking" placeholder sent before work starts.
func Processing(botName string) map[string]any {
	return New(botName).
		SetTitle("🤔 处理中...", "blue").
		Markdown("正在处理你的请求，请稍候...").
		Build()
}

// Streaming renders an in-progress answer.
func Streaming(botName, text string) map[string]any {
	return New(botName).
		SetTitle("⏳ 生成中...", "blue").
		Markdown(text).
		Build()
}

// Done is a finished answer with optional action buttons.
func Done(botName, text, footer string, btns ...Button) map[string]any {
	b := New(botName).SetTitle("✅ "+botName, "green").Markdown(text)
	if footer != "" {
		b.Footer(footer)
	}
	if len(btns) > 0 {
		b.Buttons(btns...)
	}
	return b.Build()
}

// Error renders a failed turn.
func Error(botName, text string) map[string]any {
	return New(botName).SetTitle("❌ 错误", "red").Markdown(text).Build()
}

// Stopped marks a cancelled turn.
func Stopped(botName, text string) map[string]any {
	return New(botName).SetTitle("⏹ 已停止", "orange").Markdown(text).Build()
}

// CommandCard is a short informational card (e.g. /pwd, /status).
func CommandCard(botName, title, text string, btns ...Button) map[string]any {
	b := New(botName).SetTitle("ℹ️ "+title, "indigo").Markdown(text)
	if len(btns) > 0 {
		b.Buttons(btns...)
	}
	return b.Build()
}

// ---- markdown helpers ----------------------------------------------------

// Sanitize applies the minimal rendering fixes needed for feishu markdown.
func Sanitize(s string) string {
	if s == "" {
		return s
	}
	s = strings.TrimSpace(s)
	// Ensure fenced code blocks are terminated before rendering.
	if strings.Count(s, "```")%2 == 1 {
		s += "\n```"
	}
	return s
}

// FormatUsage renders a compact token/cost footer.
func FormatUsage(usage map[string]any, modelUsage map[string]any, costUSD float64, elapsed time.Duration) string {
	var parts []string
	if usage != nil {
		in := num64(usage["input_tokens"])
		cr := num64(usage["cache_read_input_tokens"])
		cc := num64(usage["cache_creation_input_tokens"])
		out := num64(usage["output_tokens"])
		totalIn := in + cr + cc
		if totalIn > 0 {
			s := fmtTokens(totalIn) + " in"
			if cr > 0 {
				s += fmt.Sprintf(" (%d%% ⚡)", int(cr/totalIn*100))
			}
			parts = append(parts, s)
		}
		if out > 0 {
			parts = append(parts, fmtTokens(out)+" out")
		}
	}
	if modelUsage != nil && len(modelUsage) > 0 {
		if m, ok := primaryModel(modelUsage); ok {
			parts = append(parts, m)
		}
	}
	if costUSD > 0 {
		parts = append(parts, fmt.Sprintf("$%.4f", costUSD))
	}
	if elapsed > 0 {
		parts = append(parts, fmtElapsed(elapsed))
	}
	return strings.Join(parts, " · ")
}

func fmtTokens(n float64) string {
	switch {
	case n >= 1_000_000:
		return fmt.Sprintf("%.1fM", n/1_000_000)
	case n >= 1000:
		return fmt.Sprintf("%.1fk", n/1000)
	}
	return fmt.Sprintf("%.0f", n)
}

func fmtElapsed(d time.Duration) string {
	if d < time.Minute {
		return fmt.Sprintf("%.1fs", d.Seconds())
	}
	m := int(d.Minutes())
	s := int(d.Seconds()) - m*60
	return fmt.Sprintf("%dm%ds", m, s)
}

// primaryModel picks the highest-usage entry from a modelUsage map.
func primaryModel(mu map[string]any) (string, bool) {
	var best string
	var bestTok float64
	for k, v := range mu {
		m, ok := v.(map[string]any)
		if !ok {
			continue
		}
		tok := num64(m["inputTokens"]) + num64(m["outputTokens"]) +
			num64(m["cacheReadInputTokens"]) + num64(m["cacheCreationInputTokens"])
		if tok > bestTok {
			bestTok, best = tok, k
		}
	}
	return best, best != ""
}

func num64(v any) float64 {
	switch n := v.(type) {
	case float64:
		return n
	case int:
		return float64(n)
	case int64:
		return float64(n)
	}
	return 0
}
