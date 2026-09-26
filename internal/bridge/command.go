package bridge

import (
	"strings"

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
func standardButtons(bot *config.BotConfig, m *feishu.Message) []card.Button {
	thread := ""
	if m != nil {
		thread = m.ThreadID
	}
	return []card.Button{
		{Text: "🆕 新会话", Value: map[string]string{"action": "new", "thread": thread}},
		{Text: "⏹ 停止", Value: map[string]string{"action": "stop", "thread": thread}},
		{Text: "📊 状态", Value: map[string]string{"action": "status", "thread": thread}},
		{Text: "❓ 帮助", Value: map[string]string{"action": "help", "thread": thread}},
	}
}
