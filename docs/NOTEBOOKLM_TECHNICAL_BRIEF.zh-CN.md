# AgentMux 技术资料汇编

生成日期：2026-06-10

本文用于上传到 NotebookLM，作为 AgentMux 项目的技术背景资料。它以项目事实、架构边界、关键流程、协议、源码索引和未来技术方向为主，方便后续围绕项目进行问答、总结、路线规划和设计评审。

## 1. 项目一句话定义

AgentMux 是一个面向长期运行 coding-agent session 的远程控制平面。

它把 `hub`、`worker`、`control` 三个角色放在同一个 Go binary 中：

- `hub`：公网 HTTP/WSS 入口、身份认证、设备接入、Web Control 托管、WebSocket 路由。
- `worker`：运行在拥有本地 agent session 的机器上，主动连出到 Hub，管理本地 tmux 或内置 PTY session。
- `control`：控制端客户端，包括 CLI/TUI 和 Hub 托管的浏览器 Web Control。

AgentMux 的基本设计原则是：

```text
Agent 不需要知道 AgentMux 的存在。
Codex、Claude Code、Gemini、OpenCode、bash 或其他 CLI agent 都只是运行在普通 shell/tmux/PTY 中。
Worker 在 terminal 层进行控制、attach、输入转发和输出读取。
```

## 2. 核心目标

AgentMux 当前主要解决：

- 从浏览器或 CLI 远程查看和控制长期运行的 agent session。
- 让 Worker 以 outbound WebSocket 连接 Hub，避免 Worker 机器开放公网端口。
- 通过 tmux 或内置 PTY 保持 agent session 可 detach/re-attach。
- 通过 Hub 统一管理 Worker、Session、Control 连接和凭证。
- 支持一键生成 join signal，降低 Worker 和 Control 加入成本。
- 支持 Cloudflare Tunnel、Docker、systemd 等部署方式。

中长期目标是演进为 AgentOps control plane：

- 远程 agent session runtime。
- session snapshot、transcript、replay。
- agent 执行审计。
- 多 agent workspace。
- policy/approval。
- 与 AI Gateway 和模型成本日志集成。

## 3. 当前项目结构

重要目录和文件：

```text
cmd/agentmux/main.go                  CLI 入口，包含 hub/worker/control 三类命令
internal/hub/                         Hub server、HTTP API、WebSocket 路由、认证
internal/worker/                      Worker 连接 Hub、处理 session 和 terminal stream
internal/control/                     CLI/TUI Control 客户端、WebSocket stream client
internal/protocol/                    Hub/Worker/Control 之间的 JSON envelope 协议
internal/sessionbackend/              session backend 抽象接口
internal/tmux/                        tmux backend
internal/ptybackend/                  内置 PTY backend
internal/pty/                         Linux/macOS PTY 封装
internal/ws/                          项目内 WebSocket 实现
internal/terminalview/                TUI 侧 terminal view/emulator 支持
web/control/                          React + TypeScript Web Control 源码
internal/hub/webdist/                 Web Control 构建后嵌入 Hub 的静态产物
docs/                                 技术、部署、API、路线图文档
business/                             产品商业化方向和商业 roadmap
```

## 4. 运行角色

### 4.1 Hub

Hub 是唯一需要被其他设备访问的服务。

职责：

- 提供 landing page。
- 提供 Web Control 页面 `/control`。
- 提供 HTTP API。
- 接收 Worker outbound WebSocket：`/ws/worker`。
- 接收 Control WebSocket：`/ws/control`。
- 认证 token、signal、credential。
- 跟踪在线 Worker。
- 维护由 Worker 上报的 session snapshot。
- 把 Control 的 session 创建、kill、input、attach、resize 请求路由到对应 Worker。
- 把 Worker terminal output 路由回对应 Control stream。

Hub 支持两种状态存储：

- 无 `--data`：开发用内存认证存储。
- 有 `--data`：SQLite 持久化 signals、credentials、registered users。

Hub 不持久化：

- 在线 Worker。
- active WebSocket stream。
- live session snapshot。

这些运行态会在 Worker 重连并重新上报后恢复。

### 4.2 Worker

Worker 运行在拥有本地 coding-agent session 的机器上。

职责：

- 通过 outbound WebSocket 连接 Hub。
- 发送 `worker.hello`。
- 定期发送 heartbeat。
- 定期或在变化后发送 `session.snapshot`。
- 创建 session。
- kill session。
- 打开 terminal attach stream。
- 接收 input 并写入本地 PTY/tmux。
- 接收 resize 并调整本地 PTY。
- 读取 terminal output 并转发给 Hub。

Worker backend：

- `tmux`：优先 backend，用 tmux 作为 durable session source of truth。
- `pty`：内置 PTY backend，适合没有 tmux 的环境，但 Worker 进程停止后 session 会丢失。
- `auto`：优先 tmux，缺失时 fallback 到 pty。

### 4.3 Control

Control 是用户操作入口。

当前 Control 形态：

- CLI：`agentmux control list/create/send/stop/attach`。
- TUI：`agentmux-tui` 或 `agentmux control app`。
- Web Control：Hub 托管的 React/xterm.js 浏览器控制台。

Control 能做：

- 查看 Worker 列表。
- 查看 Session 列表。
- 创建 Session。
- attach Session。
- 发送 terminal input。
- resize terminal。
- detach 当前 attach stream。
- stop session。

## 5. Agent 透明性

AgentMux 的重要产品和技术原则：

```text
AgentMux 不修改 agent binary。
AgentMux 不要求 agent 安装插件。
AgentMux 不依赖 agent 自己的远程控制 API。
AgentMux 不要求 agent 知道自己被远程控制。
```

典型 session 命令：

```bash
tmux new-session -d -s demo -c /repo 'codex'
tmux new-session -d -s demo -c /repo 'claude'
tmux new-session -d -s demo -c /repo 'bash'
```

对 agent 来说，它只是在一个普通 shell 中运行。

## 6. 通信拓扑

典型部署拓扑：

```text
Browser / CLI Control
        |
        | HTTPS / WSS
        v
      Hub
        ^
        | outbound WSS
        |
      Worker
        |
        | tmux / PTY
        v
  coding agent / shell
```

关键点：

- Worker 只需要主动连出，不需要公网入口。
- Hub 可以放在 Cloudflare Tunnel、ngrok、Nginx、Caddy 或其他 HTTPS/WSS 反代后面。
- Control 和 Worker 都通过 Hub 的 credential 认证。
- Hub 是路由层，不是 terminal state 的长期所有者。

## 7. 认证和接入模型

### 7.1 开发 token

开发环境可以使用共享 token：

```bash
agentmux hub --token dev-token
agentmux worker --hub ws://127.0.0.1:8080 --token dev-token --name local
agentmux control list --hub http://127.0.0.1:8080 --token dev-token
```

### 7.2 Signal

Hub landing page 可以生成短期 join signal：

```text
amx_sig_...
```

Signal 是 bootstrap material，不能直接访问普通 API/WebSocket。

Worker 或 Control 必须调用：

```text
POST /api/exchange
```

把 signal 换成 scoped credential。

### 7.3 Credential

Signal exchange 返回：

```text
amx_cred_...
```

Credential 具有：

- role：`worker` 或 `control`
- tenant_id
- device_id
- expires_at
- scopes

HTTP 请求使用：

```text
Authorization: Bearer <token>
```

WebSocket 可以使用 header，也可以用：

```text
?token=<token>
```

### 7.4 Registered User

Web Control 包含注册/登录入口。

Hub 支持：

- `/api/auth/register`
- `/api/auth/login`
- `/api/auth/oauth/{github|google}`

当前 OAuth endpoint 已有稳定接口，但 provider 尚未配置完整登录流程。

当 Hub 使用 `--data` 时，注册用户、signals、credentials 持久化到 SQLite。

## 8. HTTP API 摘要

### 8.1 页面和健康检查

```text
GET /
GET /control
GET /install.sh
GET /health
```

- `/`：Hub landing page，可生成 signal 和命令。
- `/control`：浏览器 Web Control。
- `/install.sh`：一键安装/启动 Worker 或 Control 的 bootstrap script。
- `/health`：返回 Hub liveness。

### 8.2 Signal 和 Credential

```text
POST /api/signals
POST /api/join-tokens      兼容 alias
POST /api/exchange
```

### 8.3 Worker 和 Session

```text
GET    /api/workers
GET    /api/sessions
POST   /api/sessions
POST   /api/sessions/{worker}/{name}/input
DELETE /api/sessions/{worker}/{name}
```

## 9. WebSocket 协议

所有 WebSocket 消息都使用 JSON envelope：

```json
{
  "type": "terminal.output",
  "id": "optional-request-id",
  "stream_id": "optional-terminal-stream-id",
  "worker_id": "local",
  "session_id": "local/demo",
  "payload": {}
}
```

Envelope 字段：

- `type`：消息类型，必填。
- `id`：可选请求 ID。
- `stream_id`：具体 terminal attach stream ID。
- `worker_id`：Worker ID。
- `session_id`：全局 session ID，格式是 `<worker_id>/<session_name>`。
- `payload`：类型相关 JSON payload。

核心消息类型：

```text
worker.hello
worker.heartbeat
session.snapshot
session.sync
session.preview
session.create
session.kill
terminal.open
terminal.close
terminal.input
terminal.output
terminal.resize
control.open
control.input
error
```

协议定义源码：

```text
internal/protocol/protocol.go
```

## 10. Worker WebSocket 流程

Endpoint：

```text
GET /ws/worker?token=<token>
```

Worker 连接后发送：

```json
{
  "type": "worker.hello",
  "worker_id": "local",
  "payload": {
    "name": "local",
    "version": "dev",
    "backend": "tmux"
  }
}
```

Hub 记录 Worker online 状态。

Worker 周期性发送：

```json
{
  "type": "worker.heartbeat",
  "worker_id": "local"
}
```

Worker 上报 session：

```json
{
  "type": "session.snapshot",
  "worker_id": "local",
  "payload": {
    "sessions": [
      {
        "name": "demo",
        "cwd": "/repo",
        "command": "bash",
        "status": "idle",
        "backend": "tmux"
      }
    ]
  }
}
```

Hub 可发送给 Worker：

- `session.create`
- `session.kill`
- `session.preview`
- `terminal.open`
- `terminal.input`
- `terminal.resize`
- `terminal.close`

## 11. Control WebSocket 流程

Endpoint：

```text
GET /ws/control?token=<token>
```

Control attach 某个 session 时发送：

```json
{
  "type": "control.open",
  "session_id": "local/demo",
  "stream_id": "web:...",
  "payload": {
    "cols": 120,
    "rows": 36
  }
}
```

Hub 验证：

- session_id 格式。
- Worker 是否存在。
- Credential tenant 是否允许访问 Worker。
- Worker 是否 enabled。

Hub 将其转换并转发给 Worker：

```json
{
  "type": "terminal.open",
  "session_id": "local/demo",
  "stream_id": "web:...",
  "payload": {
    "cols": 120,
    "rows": 36
  }
}
```

后续输入：

```json
{
  "type": "control.input",
  "session_id": "local/demo",
  "stream_id": "web:...",
  "payload": {
    "data": "pwd\r"
  }
}
```

Hub 转发为：

```json
{
  "type": "terminal.input",
  "session_id": "local/demo",
  "stream_id": "web:...",
  "payload": {
    "data": "pwd\r"
  }
}
```

Resize：

```json
{
  "type": "terminal.resize",
  "session_id": "local/demo",
  "stream_id": "web:...",
  "payload": {
    "cols": 160,
    "rows": 48
  }
}
```

Worker 输出：

```json
{
  "type": "terminal.output",
  "session_id": "local/demo",
  "stream_id": "web:...",
  "payload": {
    "data": "...",
    "encoding": "base64"
  }
}
```

## 12. Terminal attach 当前实现

### 12.1 tmux backend

tmux backend 的 `Open` 当前实现是：

```text
启动一个 PTY
在 PTY 中运行 tmux attach-session -t <session>
Worker 读取 PTY 输出
Worker 将输出编码成 terminal.output 转发
Control 将输出写入 xterm.js 或 CLI stdout
```

这意味着：

- 每个 Control attach 是一个独立 tmux client。
- detach 只关闭这个 Control 的 attach stream，不 kill 底层 tmux session。
- tmux session 是 durable source of truth。
- attach 时 tmux 会向新 client 绘制当前可见屏幕。
- resize 时 tmux 或前台程序可能重绘完整可见区域。

相关源码：

```text
internal/tmux/tmux.go
internal/pty/pty_linux.go
internal/pty/pty_darwin.go
internal/worker/worker.go
```

### 12.2 内置 PTY backend

内置 PTY backend 直接启动 shell/command，并保存最近历史：

- 最大历史：`256KB`
- attach 时推送最近 200 行
- Worker 进程存活期间 session 可 detach/re-attach
- Worker 停止后 session 丢失

相关源码：

```text
internal/ptybackend/backend.go
```

## 13. Web Control 当前实现

Web Control 使用：

- React
- TypeScript
- xterm.js
- `@xterm/addon-fit`
- Tailwind CSS
- resizable pane layout
- lucide-react icons

源目录：

```text
web/control/
```

构建产物嵌入：

```text
internal/hub/webdist/
```

Web Control terminal attach 关键逻辑：

1. 创建 xterm.js `Terminal`。
2. 加载 `FitAddon`。
3. 建立 `/ws/control` WebSocket。
4. WebSocket open 后发送 `control.open`。
5. 收到 `terminal.output` 后 `term.write(...)`。
6. `term.onData(...)` 捕获输入并发送 `control.input`。
7. `ResizeObserver` 触发 fit，并在 cols/rows 变化时发送 `terminal.resize`。
8. 组件卸载时发送 `terminal.close` 并关闭 WebSocket。

Web Control 当前支持：

- sidebar session 列表。
- worker/session filter。
- create session modal。
- multi-pane workspace。
- pane split。
- session drag/drop。
- tmux prefix button。
- token/sign-in/register flow。

## 14. CLI Control attach 当前实现

CLI attach 流程：

1. 获取本地 terminal size。
2. 打开 Control WebSocket stream。
3. 发送 `control.open`。
4. 本地进入 alternate screen。
5. 本地 stdin 进入 raw mode。
6. 一条 goroutine 读 remote output 写 stdout。
7. 一条 goroutine 读本地 stdin 发 `control.input`。
8. 监听 `SIGWINCH` 并发送 `terminal.resize`。
9. `Ctrl-]` detach。

相关源码：

```text
internal/control/attach_cli.go
internal/control/stream.go
```

## 15. Session backend 抽象

Backend interface：

```go
type Backend interface {
    Name() string
    List(ctx context.Context) ([]Session, error)
    Create(ctx context.Context, name, cwd, command string) error
    Kill(ctx context.Context, name string) error
    SendTerminalInput(ctx context.Context, name, data string) error
    Capture(ctx context.Context, name string, lines int) (string, error)
    Open(ctx context.Context, name string, cols int, rows int) (Stream, error)
}
```

Stream interface：

```go
type Stream interface {
    Read([]byte) (int, error)
    Write([]byte) (int, error)
    Resize(cols int, rows int) error
    Close() error
}
```

设计意义：

- Worker 不直接依赖 tmux。
- tmux 和内置 PTY 都可以作为 backend。
- 未来可以新增 Docker、SSH、Kubernetes pod、remote devbox 等 backend。

## 16. 数据持久化边界

SQLite 持久化：

- signals
- credentials
- registered users

运行态不持久化：

- online workers
- active WebSocket streams
- live session snapshots
- terminal stream output
- tmux session 本身

tmux session 的持久性由 Worker 所在机器上的 tmux server 提供。

内置 PTY session 的持久性只存在于 Worker 进程生命周期内。

## 17. 部署模式

### 17.1 本地开发

推荐 smoke test：

```bash
./scripts/dev-tmux.sh
```

默认：

```text
Hub:     127.0.0.1:8081
Token:   dev-token
Worker:  local
Session: demo
```

### 17.2 手动开发命令

Hub：

```bash
go run ./cmd/agentmux hub --addr 127.0.0.1:8080 --token dev-token
```

Worker：

```bash
go run ./cmd/agentmux worker --hub ws://127.0.0.1:8080 --token dev-token --name local
```

Control：

```bash
go run ./cmd/agentmux control list --hub http://127.0.0.1:8080 --token dev-token
go run ./cmd/agentmux control create --hub http://127.0.0.1:8080 --token dev-token --worker local --name demo --cwd "$PWD" --command bash
go run ./cmd/agentmux control attach --hub ws://127.0.0.1:8080 --token dev-token --session local/demo
```

### 17.3 Docker

镜像：

```text
ghcr.io/kinboyw/agentmux:latest
```

启动：

```bash
docker run -d --name agentmux --restart unless-stopped -p 8081:8081 ghcr.io/kinboyw/agentmux:latest
```

默认使用：

```text
hub --addr 0.0.0.0:8081 --data /var/lib/agentmux/agentmux.db
```

### 17.4 Cloudflare Tunnel

生产路径建议：

```text
Browser/Worker/Control -> Cloudflare HTTPS/WSS -> cloudflared tunnel -> Go Hub -> SQLite
```

Hub 启动时必须设置正确的：

```text
--public-url https://hub.example.com
```

否则生成的 Worker 和 Control 命令可能指向错误的本地地址。

## 18. 当前已知技术限制

### 18.1 Terminal 状态不是 Worker 侧模型

当前 tmux backend 是 raw PTY byte stream。

Worker 没有维护完整 terminal state model：

- 没有 screen cell buffer。
- 没有 cursor/mode/attribute state。
- 没有结构化 transcript。
- 没有 diff protocol。
- 没有 snapshot attach。

结果：

- attach 时依赖 tmux 新 client 的初始绘制。
- resize 后由 tmux/前台程序重绘。
- reconnect 不能直接从 Worker snapshot 恢复。
- session replay 不能准确从结构化状态生成。

### 18.2 Input 是远端回显模型

当前输入路径：

```text
Control input -> Hub -> Worker -> PTY/tmux -> remote echo -> Worker output -> Hub -> Control display
```

Control 不做本地即时回显。

这保证远端权威，但在网络延迟下体验类似高延迟 SSH。

### 18.3 Hub 不存 terminal history

Hub 只做路由和运行态 session view，不保存 terminal transcript。

### 18.4 tmux sizing 语义复杂

每个 attach stream 是一个 tmux client。

tmux 多 client、window size、pane size、foreground TUI resize repaint 的行为需要谨慎处理。

## 19. Terminal State Optimization 技术方向

项目已经沉淀独立技术方案：

```text
docs/TERMINAL_STATE_OPTIMIZATION.md
```

核心方向：

```text
PTY/tmux output
  -> Worker terminal parser
  -> screen buffer + modes + cursor + transcript
  -> snapshot/diff/history protocol
  -> Hub relay
  -> Control renderer
```

关键拆分：

```text
current screen snapshot != historical transcript != raw terminal bytes
```

目标：

- attach 时发送当前 screen snapshot。
- 历史 transcript 按需分页加载。
- live output 变成 structured diff。
- resize 后用 generation + snapshot 表达状态切换。
- input 使用 input_seq 和 ack。
- 简单 shell 输入可支持安全的 optimistic echo。
- 复杂 TUI/editor/password 模式保持远端权威。

实施阶段：

1. 当前行为观测：attach bytes、resize bytes、input RTT。
2. Control resize hygiene。
3. Worker terminal parser prototype。
4. snapshot attach。
5. transcript history API。
6. structured diffs。
7. input ack 和 optimistic echo。
8. tmux headless/state feed 方案评估。

## 20. 商业化方向

项目商业化思考见：

```text
business/ROADMAP.md
```

核心判断：

```text
AgentMux 不应成为另一个 coding agent、IDE 或 AI Gateway。
更有机会的方向是 AgentOps control plane for coding agents。
```

定位：

```text
AgentMux = secure runtime control plane for code agent sessions
```

长期产品支柱：

- Agent Session Runtime
- Agent Session Recorder
- Policy And Approval
- Multi-Agent Workspace
- Agent-Agnostic Integrations

与 AI Gateway 的关系：

```text
AI Gateway: model-call control plane
AgentMux: agent-execution control plane
```

潜在商业版功能：

- SSO/SAML/OIDC
- RBAC
- audit log retention
- encrypted transcript storage
- policy engine
- approval workflow
- team workspace
- gateway cost analytics
- hosted control plane
- enterprise support

## 21. 关键源码索引

### 21.1 CLI 入口

```text
cmd/agentmux/main.go
```

包含：

- `agentmux hub`
- `agentmux worker`
- `agentmux control`
- `agentmux-tui`
- backend selection
- auth resolve
- debug flags

### 21.2 Protocol

```text
internal/protocol/protocol.go
```

包含：

- Envelope
- message type constants
- WorkerHello
- Session
- SessionView
- TerminalInput
- TerminalSize
- TerminalOutput
- WorkerView
- SessionID/SplitSessionID

### 21.3 Hub

```text
internal/hub/hub.go
internal/hub/auth.go
internal/hub/auth_sqlite.go
```

关注：

- HTTP route
- `/ws/worker`
- `/ws/control`
- credential validation
- tenant enforcement
- worker registry
- session registry
- stream subscriber routing
- signal exchange

### 21.4 Worker

```text
internal/worker/worker.go
```

关注：

- Worker connect/reconnect loop
- worker.hello
- heartbeat
- session snapshot
- control message handling
- startStream
- writeTerminal
- resizeTerminal
- stream cleanup

### 21.5 Control

```text
internal/control/stream.go
internal/control/attach_cli.go
internal/control/app.go
```

关注：

- OpenStream
- ReadEvent
- Input
- Resize
- CLI attach raw mode
- TUI app

### 21.6 tmux backend

```text
internal/tmux/tmux.go
```

关注：

- tmux list-panes
- new-session
- kill-session
- send-keys
- capture-pane
- tmux attach-session PTY open

### 21.7 PTY backend

```text
internal/ptybackend/backend.go
internal/pty/pty_linux.go
internal/pty/pty_darwin.go
```

关注：

- built-in session map
- PTY command start
- history ring
- Open/Read/Write/Resize
- platform PTY syscalls

### 21.8 Web Control

```text
web/control/src/main.tsx
web/control/src/styles.css
web/control/src/lib/utils.ts
```

关注：

- React app state
- auth state
- worker/session polling
- workspace tabs
- pane layout
- xterm.js attach
- WebSocket input/output
- resize observer

## 22. 常见问题和答案

### Q1: AgentMux 是不是 agent 框架？

不是。AgentMux 不实现 agent 推理、规划或工具调用。它是运行和控制 coding-agent session 的 shell-layer control plane。

### Q2: AgentMux 是否绑定 Claude Code 或 Codex？

不绑定。任何能在 shell/tmux/PTY 中运行的 CLI agent 都可以被 AgentMux 管理。

### Q3: Hub 是否保存 terminal 历史？

当前不保存。Hub 只保存身份相关状态和运行态 session view。terminal 历史/状态的长期方向是由 Worker 侧 terminal state model 生成 snapshot/transcript/replay。

### Q4: 为什么 Worker 用 outbound WebSocket？

为了让 Worker 所在机器不需要公网 IP、不需要打开入站端口，适合个人 laptop、内网 devbox、CI runner、Cloudflare Tunnel 场景。

### Q5: tmux backend 和 pty backend 区别是什么？

tmux backend 使用本机 tmux server 作为 durable session source of truth，Worker 重启后 tmux session 仍可存在。pty backend 是 Worker 进程内管理的 PTY session，Worker 停止后 session 丢失。

### Q6: 当前 input 为什么有延迟？

因为当前是远端权威回显模型。Control 发送 input 到 Worker，Worker 写入 PTY/tmux，远端 shell 或应用回显后 Control 才显示。

### Q7: 未来如何降低 input 延迟？

需要 Worker 侧 terminal state model、input sequence、ack 和保守 optimistic echo。简单 shell 可预测显示，复杂 TUI/editor/password 模式保持远端权威。

### Q8: 当前 attach 为什么像重新加载整个屏幕？

tmux backend 每次 attach 都启动一个新的 `tmux attach-session` client。tmux 会给新 client 绘制当前可见屏幕。Worker 只是把 PTY 字节转发给 Control。

### Q9: 能不能从尾部加载历史？

历史 transcript 可以未来按需分页加载，但当前 terminal 可见屏幕不能简单从尾部字节倒推。terminal output 是状态机，必须有 snapshot 或完整状态模型。

### Q10: 为什么 Worker-side terminal state 是关键技术赌注？

因为它能解锁 fast attach、reconnect restore、transcript paging、session replay、structured diff、multi-control fanout、input ack 和审计快照。

## 23. 推荐 NotebookLM 提问方向

上传本文后，可以向 NotebookLM 提问：

- AgentMux 当前架构中 Hub、Worker、Control 的职责分别是什么？
- AgentMux 的 WebSocket 协议有哪些消息类型？
- 当前 attach session 的完整链路是什么？
- tmux backend 和 pty backend 的差异是什么？
- 为什么当前输入有网络延迟感？
- Worker-side terminal state model 要解决什么问题？
- snapshot、transcript、raw terminal bytes 三者有什么区别？
- AgentMux 与 AI Gateway 的边界是什么？
- AgentMux 商业化方向为什么更适合作为 AgentOps control plane？
- 下一阶段技术路线应该如何排序？

