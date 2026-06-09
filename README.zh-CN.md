<p align="center">
  <img src="docs/assets/brand/agentmux-logo.svg" alt="AgentMux" width="520">
</p>

# AgentMux

[English](README.md) | [中文](README.zh-CN.md)

AgentMux 是一个以 tmux 为底座的远程控制平面，用来管理长期运行的 coding agent 会话。
它保持单个 Go 二进制发布，但拆分为三个角色：

- `hub`：公网 HTTPS/WSS 入口、身份与路由层。
- `worker`：只发起出站连接，负责管理本地 tmux 会话。
- `control`：CLI 客户端，用于列出、创建、attach 和发送输入。
  主要产品化控制面是 Hub 托管的浏览器页面 `/control`。

AgentMux 的核心设计是让 agent 无感。Codex、Claude、Gemini、OpenCode 或普通 shell
只是运行在 tmux 里；Worker 在 shell/tmux 层观察和控制会话。

![AgentMux 系统架构](docs/assets/visuals/agentmux-1-system-architecture-v1.png)

## 快速开始

本地一键验证：

```bash
./scripts/dev-tmux.sh
```

这会创建一个名为 `agentmux-dev` 的 tmux session，并在同一个窗口中分出三个 pane：

- `hub`
- `worker`
- `control`

默认端口是 `8081`，默认 token 是 `dev-token`。

浏览器控制台：

```text
http://127.0.0.1:8081/control?token=dev-token
```

项目 Go 版本由 mise 管理：

```bash
mise install
mise exec -- go test ./...
```

Web Control 使用 xterm.js 作为终端区域，并把会话身份、连接状态和 detach 控件放在远程终端缓冲区之外。
现代 Web 控制台源码位于 `web/control`，构建后嵌入到 `internal/hub/webdist`。

![AgentMux Web Control 工作区](docs/assets/visuals/agentmux-4-web-control-workspace-v1.png)

## SQLite 持久化

持久化 Hub 模式：

```bash
agentmux hub \
  --addr 0.0.0.0:8080 \
  --data ./agentmux.db \
  --public-url https://hub.example.com
```

`--data` 会持久化信令、凭证和注册用户。在线 Worker、实时 WebSocket 流和会话快照仍然属于运行态，
会在 Worker 重连后重建。`--public-url` 用于在 Hub 位于 Cloudflare Tunnel、Nginx、Caddy
或其他反向代理之后时生成正确的 worker/control/落地页命令。

Cloudflare Tunnel 示例：

```bash
agentmux hub --addr 127.0.0.1:8080 --data ./agentmux.db --public-url https://hub.example.com
cloudflared tunnel --url http://127.0.0.1:8080
```

Cloudflare 负责 HTTPS 终止并把 WebSocket upgrade 转发给 Go Hub。当 public URL 是 HTTPS 时，
Hub 会自动生成 `wss://...` 的 Worker 接入地址。

![AgentMux Cloudflare 部署](docs/assets/visuals/agentmux-3-cloudflare-deployment-v1.png)

## 信令接入

1. 打开 `http://127.0.0.1:8081/`。
2. 生成一个 signal。
3. 运行页面生成的 worker 命令。
4. 打开页面生成的 Web Control URL。

Signal 形如 `amx_sig_...`，正常 API 或 WebSocket 访问前会交换成受限的 `amx_cred_...` 凭证。

![AgentMux 信令接入](docs/assets/visuals/agentmux-2-zero-tier-style-onboarding-v1.png)

## 手动三终端验证

Terminal 1：

```bash
go run ./cmd/agentmux hub --addr 127.0.0.1:8080 --token dev-token
```

Terminal 2：

```bash
go run ./cmd/agentmux worker --hub ws://127.0.0.1:8080 --token dev-token --name local
```

Terminal 3：

```bash
go run ./cmd/agentmux control list --hub http://127.0.0.1:8080 --token dev-token
go run ./cmd/agentmux control create --hub http://127.0.0.1:8080 --token dev-token --worker local --name demo --cwd "$PWD" --command bash
go run ./cmd/agentmux control attach --hub ws://127.0.0.1:8080 --token dev-token --session local/demo
```

在 `control attach` 中按 `Ctrl-]` 可以从本地 control 会话 detach。
这只会关闭本地控制连接，Worker 侧 tmux 会话会继续存活。`Ctrl-C` 会转发给远程 agent。

## 构建与发布

CI 会通过 GitHub Actions 运行 Go 测试、构建 Web Control，并构建 CLI 二进制。
推送类似 `v0.1.0` 的 tag 会触发跨平台 release assets。

本地 release 风格构建：

```bash
cd web/control
npm ci
npm run build
cd ../..
rm -rf internal/hub/webdist
mkdir -p internal/hub/webdist
cp -a web/control/dist/. internal/hub/webdist/
mise exec -- go build -o dist/agentmux ./cmd/agentmux
```

## 文档

- [Design](docs/DESIGN.md)
- [Product Architecture](docs/PRODUCT_ARCHITECTURE.md)
- [API](docs/API.md)
- [Deployment](docs/DEPLOYMENT.md)
- [Roadmap](docs/ROADMAP.md)
- [Visual Prompt Library](docs/visual-prompts/README.md)
- [GitHub Repository Metadata](docs/GITHUB.md)
