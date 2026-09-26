package bridge

import (
	"fmt"
	"strings"
	"time"

	"feishubridge/internal/card"
	"feishubridge/internal/config"
	"feishubridge/internal/feishu"
)

// isBridgeCommand reports whether text starts with a known bridge command.
// Only exact first-token matches are treated as commands, so that a user
// typed message like "/tmp/foo" is never swallowed.
func isBridgeCommand(text string) bool {
	t := strings.ToLower(strings.TrimSpace(text))
	first := strings.Fields(t)
	if len(first) == 0 {
		return false
	}
	switch first[0] {
	case "/help", "/h", "/new", "/reset", "/stop", "/cancel",
		"/status", "/pwd", "/cd", "/ls", "/model":
		return true
	}
	return false
}

// helpText is the /help body.
const helpText = `**会话控制**
- /new — 开启新的 Claude 会话（保留目录）
- /stop — 取消当前任务
- /status — 查看 shell、会话与运行时间
- /model [名称] — 查看或切换模型

**目录**（本进程内记录，无需外部 shell）
- /pwd — 当前工作目录
- /cd <路径> — 切换目录（相对路径基于当前目录）
- /ls — 列出文件

**其他**
- /help — 本帮助

其他任意消息都会作为提示词发送给 Claude。
/cd 切换的目录对后续命令和 Claude 会话都生效；它只记录在本进程内存里，重启后回到默认工作目录。`

// standardButtons returns the action buttons appended to most cards.
//
// A card already sitting inside a topic must not offer "new session": a new
// session is materialised as a new topic, so offering it again would fork the
// conversation. The topic-scoped set keeps only actions that make sense in
// place.
//
// The root message id is stamped into every topic button. The callback only
// carries the clicked card's own id, and reply_in_thread=true forks a topic
// only when the anchor is not itself inside one - so a reply to a card that
// is already a topic reply would leave the topic entirely. Anchoring on the
// root keeps every follow-up inside it.
func standardButtons(bot *config.BotConfig, m *feishu.Message) []card.Button {
	if m != nil && m.ThreadID != "" {
		return threadButtons(m.ThreadID, m.RootID)
	}
	return chatButtons()
}

// chatButtons is the set offered outside a topic, where forking a topic is
// the point.
func chatButtons() []card.Button {
	return []card.Button{
		{Text: "🆕 新会话", Value: map[string]string{"action": "new"}},
		{Text: "⏹ 停止", Value: map[string]string{"action": "stop"}},
		{Text: "📊 状态", Value: map[string]string{"action": "status"}},
		{Text: "❓ 帮助", Value: map[string]string{"action": "help"}},
	}
}

// threadButtons is the set offered inside an existing topic.
func threadButtons(thread, root string) []card.Button {
	v := func(a string) map[string]string {
		return map[string]string{"action": a, "thread": thread, "root": root}
	}
	return []card.Button{
		{Text: "⏹ 停止", Value: v("stop")},
		{Text: "📊 状态", Value: v("status")},
		{Text: "❓ 帮助", Value: v("help")},
	}
}

// threadContext renders the per-topic scope: which directory the agent runs
// in, and which Claude session this topic is bound to.
//
// The session id is the only durable handle on a conversation. Showing it
// lets the user cross-check this topic against a local `claude --resume`
// session, so a mismatch is diagnosable instead of mysterious.
func threadContext(s *session, workspace string) string {
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("**工作目录**: `%s`\n\n", workspace))
	if s != nil {
		sb.WriteString(fmt.Sprintf("**Claude 会话**: `%s`\n\n", shortID(string(s.ID))))
		sb.WriteString(fmt.Sprintf("已处理 %d 轮 · 最后活动 %s 前\n",
			s.Turns, time.Since(s.LastSeen).Round(time.Second)))
	} else {
		sb.WriteString("尚未运行会话 — 本话题下一条消息会创建 Claude 会话。\n")
	}
	return sb.String()
}
