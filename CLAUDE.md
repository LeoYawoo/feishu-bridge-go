# feishu-bridge-go

飞书机器人 -> Claude Code 桥接。飞书里发消息/点卡片按钮，本机拉起 `claude` 子进程执行，结果以交互卡片回飞书。

## 构建 / 测试

Go 不在 PATH 上，它是 module cache 里的 toolchain。用 `build.bat` 里的路径：

```powershell
$env:PATH = "C:\Users\liuyawu\go\pkg\mod\golang.org\toolchain@v0.0.1-go1.26.5.windows-amd64\bin;C:\soft\mingw\mingw64\bin;$env:PATH"
$env:GOPATH = "C:\Users\liuyawu\go"
$env:GOMODCACHE = "C:\Users\liuyawu\go\pkg\mod"
go vet ./... ; go test ./... ; go build -trimpath -ldflags "-s -w" -o feishubridge.exe ./cmd/feishubridge
```

`build.bat` 是 gitignored（路径是机器相关的）；跨平台构建走 `Makefile` (`make release`)。gofmt 也要带同样的 PATH。

## 运行

```powershell
$old = Get-Process -Name feishubridge -ErrorAction SilentlyContinue
$old | Stop-Process -Force -Confirm:$false
Start-Process .\feishubridge.exe -ArgumentList '-config','.\config.json','-loglevel','debug' `
  -WorkingDirectory . -RedirectStandardOutput .\bridge.log -RedirectStandardError .\bridge.err.log -PassThru
```

要点：
- **必须带 `-config .\config.json`**。不带就走默认路径 `~/.config/feishu-bridge/config.json`，不存在，进程秒退，`bridge.log` 根本不生成（只有 `.err.log` 里有"配置错误"）。
- **必须带 `-loglevel debug`**，否则 `log.Lshortfile` 不会加上，排查时看不到 `bridge.go:529` 这类行号。
- 日志走 stdout → `bridge.log`；SDK 自己的连接日志（`[Info] connected to wss://...`）也在这里。
- 杀进程用 `Stop-Process -Id X -Force -Confirm:$false`，`$p.Stop()` 在 Windows 上不可用。

## 安全 / 不要碰

- `config.json` 里是**真实 app_secret**，gitignored，不要提交、不要从 `.gitignore` 移除、不要贴进聊天或日志。凭据用 `${FEISHU_APP_ID}`/`${FEISHU_APP_SECRET}` 占位符。
- `*.log` 在 `.gitignore` 里是有原因的：`bridge.log` 含 websocket URL 里的一次性 `access_key`/`ticket`。
- 无 cgo。飞书侧长连接是唯一入站通道。
- 浏览器自动化用隔离的 `BridgeTest` profile，不要碰已登录的真实 `User Data` profile。
- commit message 结尾加 `Co-Authored-By: Claude Code <noreply@anthropic.com>`。

## 关键架构知识（踩过的坑，别重新踩）

**1. 卡片回调的上下文是贫瘠的。** `event/dispatcher/callback/model.go` 的 `Context` 只有 `OpenMessageID` + `OpenChatID` —— **没有 thread_id，没有 parent_id**。凡是需要知道"在哪个话题里"的信息，必须在构造卡片时写进按钮的 `value` map（`threadButtons(thread, root)` 就是这么干的），回调时再取出来。`onCardAction` 里 `root` 为空就回落到 `req.MessageID`（旧卡片的兼容路径）。

**2. `reply_in_thread=true` 的锚点语义。** 只有当锚点消息**本身不在话题里**时，它才会 fork 出新话题，该消息成为话题根。锚点已经是话题回复时，回复会掉回主会话。所以话题内的后续回复要锚在**话题根消息 id** 上，不能锚在被点的那张卡片上。

**3. 显式传 `reply_in_thread=false` 必须省略，不是传 false。** 锚点已经是精确的 message id 时再传一个显式 false，会让飞书重新评估线程归属而不是认锚点。见 `client.go:replyCard` 的注释。

**4. 飞书没有"创建话题"接口。** 一条 `reply_in_thread=true` 的消息发出去，话题就诞生了，那条消息是根。`ReplyMessageRespData` 返回 `MessageId`/`ThreadId`/`RootId`/`ParentId` 四个字段，全可用于日志。

**5. cwd 按话题隔离，不是按会话。** `topicCwdKey(bot, chat, thread)` 三段式；session 记录也是 `sessionKey` 三段式。两边必须对称 —— 曾经 cwd 用两段、session 用三段，导致新话题回落到默认 workspace。`newTopic` 里要在回复**之前**抓 `inherit`，因为新话题此刻还没有 cwd 记录。

**6. 锁顺序。** `currentCwd` 自己拿 `b.mu`。在已经持锁的函数里调它会死锁；`handleStatus` 要先取 `dir` 再 `Lock()`。

**7. 飞书话题在会话列表里是折叠的。** 话题真的建出来了，但要展开才看得到内容。别因为列表里没有就断定失败。

**8. 卡片 schema 2.0 没有 `"action"` 元素。** 按钮是顶层 `{"tag":"button"}`，包在 `"action"`/`"actions"` 里会报 `200861 unsupported tag action`。

## 排查

收发消息全量日志已在 `internal/feishu/` 里落盘：

- 入站：`recv <msg> type=.. sender=.. chat=.. thread=".." root=".." parent=".." text=".."` —— `thread`/`root`/`parent` 三段是话题路由的全部真相。
- 卡片回调：`card action <action> by <user>: message=.. chat=.. host=.. value={..}` —— `value=` 就是按钮里 stamp 的 `thread`/`root`。
- 出站：`send reply anchor=.. thread=.. -> msg=.. thread_id=".." root=".." parent=".."` —— `thread_id` 决定它落在哪。

判断"回复没进话题"只看出站行的 `thread_id` 是否为空。

本机安全软件会拦**临时编译出来的** Go 进程访问 `open.feishu.cn`（`connectex: access forbidden`），但已经跑起来的 `feishubridge.exe` 不受影响。要临时调飞书 API 的话，把它做成 bridge 自身的命令，别单独 `go run`。

浏览器（chrome-devtools MCP）操作飞书网页版时会话列表/消息流经常渲染慢或卡住：reload 经常超时（30s 内不完成但页面其实在加载），take_snapshot 有时拿到骨架屏。截图确认后再动手，别连点。

## 目录

- `internal/feishu/` — lark SDK 封装：ws 入站、message 出站、卡片回调。所有收发消息日志在这里。
- `internal/bridge/` — 命令分发、cwd 表、session 表、Claude 子进程编排。
- `internal/agent/` — Claude Code 子进程（`claude -p --output-format stream-json`）。
- `internal/card/` — schema 2.0 卡片构造器。
- `internal/config/` — 配置加载 + `${ENV}` 展开。

`D:\workcode\work` 是 `/cd` 的常规测试目录；`D:\workcode\work\test_claude` 用来测 Claude 会话基础功能。
