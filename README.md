# feishubridge

基于飞书 Golang SDK 的消息桥接：**飞书 WebSocket → Claude Code**。

在飞书里就能查看/切换工作目录、运行 `claude` / `codex`，并通过飞书消息与会话
交互。卡片流式更新（打字机效果），底部带交互按钮。

参考实现: `feishu-bridge/`（Python 版）。本实现只保留核心路径，
约 1200 行 Go，无 cgo 依赖。

## 架构

两层状态：

```
飞书消息（chat + thread 话题）
        │
        ▼
┌─────────────────────────────┐
│ Layer 1: 工作目录             │  ← 每个 chat 一个
│   /pwd /cd /ls              │     纯 Go 内存记录，不起进程
└──────────────┬──────────────┘
               │ 继承当前 cwd
               ▼
┌─────────────────────────────┐
│ Layer 2: Claude Code 会话     │  ← 每个 chat+thread 一个
│   claude -p --resume <id>   │     JSON 流式输出
│   保留 CLAUDE.md / hooks /   │     --resume 续接同一会话
│   skills（同一 claude 二进制） │
└─────────────────────────────┘
```

飞书「话题（thread）」→ Claude 会话；飞书「聊天（chat）」→ 工作目录。
私聊没有 thread，退化为一个会话。

**为什么不起 shell 进程**：`/pwd`、`/cd`、`/ls` 就是 `os.Getwd` / `os.ReadDir`，
标准库一行搞定。早期版本每个 chat 起一个常驻 `pwsh` 来跑 `Get-Location`
和 `Get-ChildItem`，代价是要求 `pwsh` 在 PATH 上、400 行进程管理代码、
每 30 分钟回收一次的 reaper，还引入一个跨 arch 不成立的外部依赖。现在
cwd 只是内存里的一个字符串，跨平台天然成立。

## 前置条件

- Go 1.22+
- Claude Code (`claude`) 在 PATH 中（`codex` 也可以）
- 飞书开放平台机器人，已开启「机器人接收消息」与「卡片回调」能力

**不需要** PowerShell。

## 安装

```bash
git clone <repo>
cd feishu-bridge-go
go mod tidy
go build -o feishubridge ./cmd/feishubridge
```

### 交叉编译（多平台）

```bash
make release          # 全矩阵：windows/linux/darwin × amd64/arm64 -> dist/
make release-windows  # 仅 windows
make release-linux    # 仅 linux
make test             # go test ./...
make vet              # go vet ./...
make clean            # 清理 dist/ 和本地二进制
```

版本号通过 `-ldflags` 注入，默认取 `git describe`，可用 `VERSION=v1.2.3 make release` 覆盖。

`build.bat` 是本地一键构建脚本（`vet` + `test` + `build`），里面写死了你机器上的
Go 工具链路径，所以已加入 `.gitignore`——不提交。跨平台构建用 Makefile。

> **Windows 用户**：本机 make 叫 `mingw32-make`（mingw64 自带），例如
> `C:\soft\mingw\mingw64\bin\mingw32-make.exe`。Makefile 已针对它验证过，
> 用法完全一样：`mingw32-make release`。`build.bat` 已把该路径加入 PATH。
>
> 没有 make 的话也可以直接跑等价的 go 命令：
>
> ```bash
> for p in windows/amd64 linux/amd64 linux/arm64; do
>   os=${p%%/*}; arch=${p##*/}
>   GOOS=$os GOARCH=$arch go build -trimpath -ldflags "-s -w" \
>     -o dist/feishubridge-$os-$arch ./cmd/feishubridge
> done
> ```

## 配置

复制示例配置并按注释填写：

```bash
cp config.example.json ~/.config/feishu-bridge/config.json
```

或 Windows：`%USERPROFILE%\.config\feishu-bridge\config.json`

凭证建议走环境变量，配置里写 `${FEISHU_APP_ID}` 占位符：

```powershell
$env:FEISHU_APP_ID = "cli_xxx"
$env:FEISHU_APP_SECRET = "xxx"
```

配置发现顺序：`-config` 参数 > `$FEISHU_BRIDGE_CONFIG` >
`~/.config/feishu-bridge/config.json`。

### 字段说明

| 字段 | 说明 |
|---|---|
| `app_id` / `app_secret` | 飞书应用凭证，支持 `${VAR}` 占位 |
| `domain` | `feishu`（国内）或 `lark`（海外 Lark） |
| `agent.command` | `claude` 或 `codex`，也可填绝对路径 |
| `agent.model` | 模型名，空则用 claude 默认 |
| `agent.timeout_seconds` | 单次会话硬超时（默认 300） |
| `agent.append_system_prompt` | 追加到系统提示词 |
| `bots[]` | 多 bot 列表，每个独立 workspace + ACL |
| `bots[].allowed_users` | open_id 白名单，`["*"]` 放行全部 |
| `bots[].group_mode` | `disabled` / `mention-all` / `owner-only` |
| `bots[].owner_open_id` | 机器人自己的 open_id，群聊 @ 判定用它（见下） |
| `bot_chats` | `chat_id` → `bot id` 映射，多 bot 时必填（见下） |
| `streaming.throttle_ms` | 卡片流式更新间隔（默认 800ms） |
| `streaming.session_idle_seconds` | Claude 会话记录闲置多久后丢弃（默认 604800s = 7 天） |

`bots[].shell` 仍能被解析（老配置不报错）但被忽略，可以删掉。

### 关于 `bot_chats`

单 bot 时不需要填——所有会话都路由给它。多 bot 时它是唯一的分配依据：
消息里没有任何信号能说明这个会话属于哪个工作区，所以必须由你显式声明。

- 未映射的 `chat_id` **不会** fallback 到第一个 bot，而是直接丢弃并记日志。
  多 bot 场景下每个 bot 有自己的 workspace 和凭证，猜错比不响应危险。
- `bot_chats` 里的 `bot id` 写错会在启动时报配置错误，不会静默走兜底。
- 取 `chat_id`：把 bot 拉进目标群，发任意消息，日志里的 `ChatID` 字段就是。

### 关于 `owner_open_id`

飞书 SDK 没有"查询当前机器人自己 open_id"的接口，所以必须手动填。
取法：把 bot 私聊一遍，日志里会打印对方的 `open_id` —— 那行就是 bot 自己的
open_id；或者在飞书开放平台的"凭证与基础信息"页查。

- `mention-all` 下留空可运行，但会退化为"群里所有白名单用户的消息都响应"，
  @ 判定失效。
- `owner-only` 下留空则 bot 完全不响应群消息。

### 卡片按钮

结果卡片底部有「新会话 / 停止 / 状态 / 帮助」四个按钮，点击等价于输入对应
命令，并且只在被点击人通过 `allowed_users` 校验时才生效。

### 工作目录

`/cd` 记录的目录只存在进程内存里，**bridge 重启后回到默认 workspace**。
这是取舍：进程内的状态重启必然丢失，而持久化它需要一套写入/恢复/校验的
代码，对一个目录值来说不值当。Claude 会话记录有持久化（见下），因为
`session_id` 的价值高得多。

### Claude 会话持久化

`session_id` ↔ 飞书话题的映射写在 `<workspace>/.feishu-bridge-sessions.json`，
每条记录带 `bot_id`、`chat_id`、`thread_id`、`last_seen`、`turns`。

- **重启后能接回旧会话**。这是唯一的持久化收益：bridge 重启后，同一个话题
  发的第一条消息会用 `--resume` 续上之前的 Claude 上下文，而不是从头开始。
- 文件损坏不会阻断启动——映射丢失，但 Claude 自己的 transcript 文件还在磁盘上。
- 闲置超过 `session_idle_seconds`（默认 7 天）的记录被 reaper 丢弃，
  Claude 本地的 transcript 不受影响，只是 bridge 不再知道它属于哪个话题。
- `/new` 删除当前话题的记录；`/status` 显示总会话数。

### 多实例部署

一个 bridge 独占一个 workspace 和一套飞书凭证。**同一个 app_id 不要连两台机器**
——飞书长连接会被抢占。多机部署的正确做法是每台机器一个飞书应用，各自配
`bot_chats` 指向自己负责的群，天然隔离。

## 运行

```bash
feishubridge -config ~/.config/feishu-bridge/config.json -loglevel debug
```

## 在飞书里使用

| 命令 | 作用 |
|---|---|
| `/help` | 帮助 |
| `/pwd` | 当前工作目录 |
| `/cd <路径>` | 切换目录（相对路径基于当前目录） |
| `/ls` | 列出文件 |
| `/new` | 新 Claude 会话（目录保留） |
| `/stop` | 取消当前任务 |
| `/status` | 运行时间、工作目录、会话信息 |
| `/model [名称]` | 查看/切换模型 |
| 其他任意文本 | 作为提示词发给 Claude |

群聊需要 @机器人 才会响应（`group_mode: mention-all`）。

## 与参考实现的差异

参考项目 20000+ 行 Python，本实现刻意省略了：

- **后台任务**（`bg_supervisor` / `bg_tasks.db` / wake socket）— 需要独立的
  reconciler 进程，不是核心交互路径
- **配额控制**（`quota.py`）— 依赖飞书计费 API
- **多 runtime**（`runtime_pi` / `runtime_omp`）— 只保留 claude/codex
- **session journal / resume index**（`session_journal.py` / `session_resume.py`）
  — 直接用 `--resume`，不做本地索引
- **memory 注入**（`compact-context.md` / `MEMORY.md` 解析）
- **消息去重 TTL**（`MessageDedup`）
- **合并转发展开**（`merge_forward` 需要额外的批量 GET API）
- **图片/文件消息**（只处理 `text` 与 `interactive`）
- **常驻 shell 进程**（用 `os.ReadDir` 替代，见架构说明）

保留的核心行为：

- 单进程 WS → 进程 → `claude -p`，不引入中间层
- 同一 `claude` 二进制、同一 `cwd`，CLAUDE.md / hooks / skills 全部生效
- 流式卡片 + 交互按钮 + 卡片回调
- 超时预算、静默超时、取消语义
- 群聊门禁、用户白名单
- 卡片体积限制（28KB payload / 10000 div chars）

## 目录结构

```
cmd/feishubridge/main.go        入口
internal/config/                配置加载与校验
internal/feishu/                飞书 SDK 封装（WS / 发消息 / 卡片 / 回调）
internal/agent/                 claude -p 调用 + stream-json 解析
internal/card/                  卡片构建（流式 + 交互按钮）
internal/bridge/                组装层：消息路由、命令、会话管理、工作目录
```

## 已验证

- `gofmt` / `go vet` / `go test ./...` 通过
- 飞书端到端：`/pwd`、`/cd`（含相对路径与不存在路径报错）、`/ls`、
  `/status`、`/help` 均验证
- 卡片 schema 2.0 按钮渲染验证（不再触发 ErrCode 200861）
