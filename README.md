# Copilot API Go

[English](#english) | [中文](#中文)

---

<a id="english"></a>

## English

A GitHub Copilot API proxy built on the **official [GitHub Copilot CLI SDK](https://github.com/github/copilot-sdk)**. Exposes Copilot as OpenAI and Anthropic compatible API services with a multi-account web console for management and load balancing.

> **Note**: Requires an active GitHub Copilot subscription. Please review [GitHub Copilot Terms](https://docs.github.com/en/site-policy/github-terms/github-copilot-product-specific-terms) before use.

### Features

- **Official SDK**: Uses the official GitHub Copilot CLI SDK — not a reverse-engineered proxy
- **Multi-Account Management**: Web console to add, remove, start, and stop multiple GitHub Copilot accounts
- **Pool Mode Load Balancing**: Distribute requests across accounts using Round-Robin, Priority, or Smart strategies
- **OpenAI Compatible API**: `/v1/chat/completions`, `/v1/models`, `/v1/embeddings`
- **Anthropic Compatible API**: `/v1/messages`, `/v1/messages/count_tokens` — automatic protocol translation
- **Model ID Mapping**: Bidirectional mapping between Copilot internal model IDs and standard display IDs
- **Streaming SSE**: Full support for streaming responses in both OpenAI and Anthropic formats
- **GitHub OAuth Device Flow**: Authenticate accounts directly from the web console
- **Admin Authentication**: Password-protected console with session management
- **Bilingual Web UI**: English and Chinese interface with auto-detection

### Quick Start

#### Docker (Recommended)

```bash
docker run -d \
  --name copilot-go \
  -p 3000:3000 \
  -p 4141:4141 \
  -v copilot-data:/root/.local/share/copilot-api \
  ghcr.io/starrykira/copilot2api-go
```

#### Docker Compose

```yaml
services:
  copilot-go:
    image: ghcr.io/starrykira/copilot2api-go:latest
    container_name: copilot-go
    restart: unless-stopped
    ports:
      - "3000:3000"
      - "4141:4141"
    volumes:
      - copilot-data:/root/.local/share/copilot-api
    environment:
      - TZ=Asia/Shanghai

volumes:
  copilot-data:
```

#### Build from Source

```bash
go build -o copilot-go .
./copilot-go
```

### Command Line Options

| Option | Default | Description |
|--------|---------|-------------|
| `--web-port` | `3000` | Web console port |
| `--proxy-port` | `4141` | Proxy API port |
| `--verbose` | `false` | Enable verbose logging |
| `--auto-start` | `true` | Auto-start enabled accounts on launch |

### Usage

1. Open `http://localhost:3000` — create an admin account on first visit
2. Add a GitHub Copilot account via OAuth device flow
3. Start the account instance
4. Use the account's API Key (or Pool Key) to call the proxy

### API Endpoints

#### OpenAI Compatible

| Endpoint | Method | Description |
|----------|--------|-------------|
| `/v1/chat/completions` | POST | Chat completions (streaming supported) |
| `/v1/models` | GET | List available models |
| `/v1/embeddings` | POST | Create embeddings |
| `/chat/completions` | POST | Alias without `/v1` prefix |
| `/models` | GET | Alias without `/v1` prefix |
| `/embeddings` | POST | Alias without `/v1` prefix |

#### Anthropic Compatible

| Endpoint | Method | Description |
|----------|--------|-------------|
| `/v1/messages` | POST | Messages API (streaming supported) |
| `/v1/messages/count_tokens` | POST | Token counting (estimation) |

#### Authentication

All proxy endpoints require a Bearer token:

```bash
# Using Authorization header (OpenAI style)
curl -H "Authorization: Bearer sk-your-api-key" ...

# Using x-api-key header (Anthropic style)
curl -H "x-api-key: sk-your-api-key" ...
```

### Examples

#### OpenAI Chat Completions

```bash
curl http://localhost:4141/v1/chat/completions \
  -H "Authorization: Bearer sk-your-api-key" \
  -H "Content-Type: application/json" \
  -d '{
    "model": "gpt-4o",
    "messages": [{"role": "user", "content": "Hello!"}],
    "stream": true
  }'
```

#### Anthropic Messages

```bash
curl http://localhost:4141/v1/messages \
  -H "x-api-key: sk-your-api-key" \
  -H "Content-Type: application/json" \
  -d '{
    "model": "claude-sonnet-4",
    "max_tokens": 1024,
    "messages": [{"role": "user", "content": "Hello!"}]
  }'
```

#### Claude Code Integration

```bash
ANTHROPIC_BASE_URL=http://localhost:4141 \
ANTHROPIC_API_KEY=sk-your-api-key \
claude
```

Or create `.claude/settings.json` in your project:

```json
{
  "env": {
    "ANTHROPIC_BASE_URL": "http://localhost:4141",
    "ANTHROPIC_AUTH_TOKEN": "sk-your-api-key",
    "ANTHROPIC_MODEL": "claude-sonnet-4",
    "ANTHROPIC_DEFAULT_SONNET_MODEL": "claude-sonnet-4",
    "ANTHROPIC_SMALL_FAST_MODEL": "gpt-4.1-mini",
    "ANTHROPIC_DEFAULT_HAIKU_MODEL": "gpt-4.1-mini",
    "DISABLE_NON_ESSENTIAL_MODEL_CALLS": "1",
    "CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC": "1"
  }
}
```

### Web Console API

#### Public Endpoints

| Endpoint | Method | Description |
|----------|--------|-------------|
| `/api/config` | GET | Server config (proxy port, setup status) |
| `/api/auth/setup` | POST | Initial admin setup |
| `/api/auth/login` | POST | Admin login |

#### Protected Endpoints (require admin session token)

| Endpoint | Method | Description |
|----------|--------|-------------|
| `/api/auth/check` | GET | Validate session |
| `/api/accounts` | GET | List all accounts with status |
| `/api/accounts/usage` | GET | Batch usage query |
| `/api/accounts/:id` | GET | Get single account |
| `/api/accounts` | POST | Add account |
| `/api/accounts/:id` | PUT | Update account |
| `/api/accounts/:id` | DELETE | Delete account |
| `/api/accounts/:id/regenerate-key` | POST | Regenerate API key |
| `/api/accounts/:id/start` | POST | Start instance |
| `/api/accounts/:id/stop` | POST | Stop instance |
| `/api/accounts/:id/usage` | GET | Get account usage |
| `/api/auth/device-code` | POST | Start GitHub OAuth flow |
| `/api/auth/poll/:sessionId` | GET | Poll OAuth status |
| `/api/auth/complete` | POST | Complete OAuth and create account |
| `/api/pool` | GET | Get pool config |
| `/api/pool` | PUT | Update pool config |
| `/api/pool/regenerate-key` | POST | Regenerate pool API key |
| `/api/model-map` | GET | Get model ID mappings |
| `/api/model-map` | PUT | Batch update mappings |
| `/api/model-map` | POST | Add single mapping |
| `/api/model-map/:copilotId` | DELETE | Delete mapping |

### Model ID Mapping

Copilot returns non-standard model IDs. The mapping feature lets you configure bidirectional translations:

- `/v1/models` returns mapped display IDs
- Incoming requests translate display IDs back to Copilot internal IDs
- Mappings are persisted to `~/.local/share/copilot-api/model_map.json`
- Configurable via the Web Console "Model ID Mapping" panel

### Project Structure

```
copilot-go/
├── main.go                      # Entry point, starts web console + proxy
├── config/config.go             # Constants, State, header builders
├── store/                       # JSON file persistence
│   ├── paths.go                 # Data directory management
│   ├── account.go               # Account CRUD
│   ├── admin.go                 # Admin auth + sessions
│   └── model_map.go             # Model ID mapping
├── auth/device_flow.go          # GitHub OAuth device flow
├── copilot/vscode_version.go    # VSCode version fetcher (used for model listing)
├── anthropic/                   # Anthropic ↔ OpenAI protocol translation
│   ├── types.go                 # All type definitions
│   ├── translate_request.go     # Anthropic → OpenAI request
│   ├── translate_response.go    # OpenAI → Anthropic response
│   ├── stream_translation.go    # Streaming SSE event translation
│   └── utils.go                 # Stop reason mapping
├── instance/                    # Instance lifecycle
│   ├── manager.go               # Start/stop, SDK client management, token refresh
│   ├── handler.go               # Proxy request handlers
│   ├── sdk_completions.go       # Official SDK completions bridge
│   └── load_balancer.go         # Round-robin / priority / smart selection
├── handler/                     # HTTP routing
│   ├── console_api.go           # Web Console API + static files
│   └── proxy.go                 # Proxy routes + auth middleware
└── web/                         # React frontend (Vite + TypeScript)
```

### Data Storage

All data is stored in `~/.local/share/copilot-api/`:

| File | Content |
|------|---------|
| `accounts.json` | Account list |
| `pool-config.json` | Pool mode settings |
| `admin.json` | Admin password hash |
| `model_map.json` | Model ID mappings |

### Credits

Based on [ericc-ch/copilot-api](https://github.com/ericc-ch/copilot-api) (TypeScript/Bun), rewritten in Go with multi-account console mode.

### License

MIT

---

<a id="中文"></a>

## 中文

基于官方 **[GitHub Copilot CLI SDK](https://github.com/github/copilot-sdk)** 构建的 Copilot API 代理服务，支持多账号 Web 管理、负载均衡，将 Copilot 转为 OpenAI/Anthropic 兼容接口。

> **说明**：需要有效的 GitHub Copilot 订阅。使用前请阅读 [GitHub Copilot 条款](https://docs.github.com/en/site-policy/github-terms/github-copilot-product-specific-terms)。

### 功能特性

- **官方 SDK**：使用官方 GitHub Copilot CLI SDK，非逆向工程代理
- **多账号管理**：Web 控制台添加、删除、启停多个 GitHub Copilot 账号
- **Pool 模式负载均衡**：轮询（Round-Robin）、优先级（Priority）或智能（Smart）策略
- **OpenAI 兼容接口**：`/v1/chat/completions`、`/v1/models`、`/v1/embeddings`
- **Anthropic 兼容接口**：`/v1/messages`、`/v1/messages/count_tokens` — 自动协议转换
- **模型 ID 映射**：Copilot 内部 ID 与标准 ID 双向映射
- **流式 SSE**：完整支持 OpenAI 和 Anthropic 格式的流式响应
- **GitHub OAuth 设备流**：在 Web 控制台直接完成账号认证
- **管理员认证**：密码保护的控制台，支持会话管理
- **中英文界面**：自动检测浏览器语言，支持手动切换

### 快速开始

#### Docker（推荐）

```bash
docker run -d \
  --name copilot-go \
  -p 3000:3000 \
  -p 4141:4141 \
  -v copilot-data:/root/.local/share/copilot-api \
  ghcr.io/starrykira/copilot2api-go
```

#### Docker Compose

```yaml
services:
  copilot-go:
    image: ghcr.io/starrykira/copilot2api-go:latest
    container_name: copilot-go
    restart: unless-stopped
    ports:
      - "3000:3000"
      - "4141:4141"
    volumes:
      - copilot-data:/root/.local/share/copilot-api
    environment:
      - TZ=Asia/Shanghai

volumes:
  copilot-data:
```

#### 源码编译

```bash
go build -o copilot-go .
./copilot-go
```

### 命令行参数

| 参数 | 默认值 | 说明 |
|------|--------|------|
| `--web-port` | `3000` | Web 控制台端口 |
| `--proxy-port` | `4141` | 代理 API 端口 |
| `--verbose` | `false` | 详细日志 |
| `--auto-start` | `true` | 启动时自动启动已启用的账号 |

### 使用方法

1. 访问 `http://localhost:3000`，首次使用创建管理员账号
2. 通过 GitHub OAuth 设备流添加 Copilot 账号
3. 启动账号实例
4. 使用账号 API Key 或 Pool Key 调用代理接口

### API 端点

#### OpenAI 兼容

| 端点 | 方法 | 说明 |
|------|------|------|
| `/v1/chat/completions` | POST | 对话补全（支持流式） |
| `/v1/models` | GET | 模型列表 |
| `/v1/embeddings` | POST | 文本嵌入 |

#### Anthropic 兼容

| 端点 | 方法 | 说明 |
|------|------|------|
| `/v1/messages` | POST | 消息 API（支持流式） |
| `/v1/messages/count_tokens` | POST | Token 计数（估算） |

#### 认证方式

所有代理端点需要 Bearer token：

```bash
# OpenAI 风格
curl -H "Authorization: Bearer sk-your-api-key" ...

# Anthropic 风格
curl -H "x-api-key: sk-your-api-key" ...
```

### 使用示例

#### OpenAI 对话补全

```bash
curl http://localhost:4141/v1/chat/completions \
  -H "Authorization: Bearer sk-your-api-key" \
  -H "Content-Type: application/json" \
  -d '{
    "model": "gpt-4o",
    "messages": [{"role": "user", "content": "你好！"}],
    "stream": true
  }'
```

#### Anthropic 消息

```bash
curl http://localhost:4141/v1/messages \
  -H "x-api-key: sk-your-api-key" \
  -H "Content-Type: application/json" \
  -d '{
    "model": "claude-sonnet-4",
    "max_tokens": 1024,
    "messages": [{"role": "user", "content": "你好！"}]
  }'
```

#### Claude Code 集成

```bash
ANTHROPIC_BASE_URL=http://localhost:4141 \
ANTHROPIC_API_KEY=sk-your-api-key \
claude
```

或在项目中创建 `.claude/settings.json`：

```json
{
  "env": {
    "ANTHROPIC_BASE_URL": "http://localhost:4141",
    "ANTHROPIC_AUTH_TOKEN": "sk-your-api-key",
    "ANTHROPIC_MODEL": "claude-sonnet-4",
    "ANTHROPIC_DEFAULT_SONNET_MODEL": "claude-sonnet-4",
    "ANTHROPIC_SMALL_FAST_MODEL": "gpt-4.1-mini",
    "ANTHROPIC_DEFAULT_HAIKU_MODEL": "gpt-4.1-mini",
    "DISABLE_NON_ESSENTIAL_MODEL_CALLS": "1",
    "CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC": "1"
  }
}
```

### 模型 ID 映射

Copilot 返回的模型 ID 不规范，映射功能支持双向转换：

- `/v1/models` 返回映射后的标准 ID
- 请求时自动将标准 ID 转回 Copilot 内部 ID
- 映射持久化到 `~/.local/share/copilot-api/model_map.json`
- 通过 Web 控制台「模型 ID 映射」面板配置

### 数据存储

所有数据存储在 `~/.local/share/copilot-api/`：

| 文件 | 内容 |
|------|------|
| `accounts.json` | 账号列表 |
| `pool-config.json` | Pool 模式配置 |
| `admin.json` | 管理员密码哈希 |
| `model_map.json` | 模型 ID 映射表 |

### 致谢

基于 [ericc-ch/copilot-api](https://github.com/ericc-ch/copilot-api)（TypeScript/Bun）重写为 Go，新增多账号控制台模式。

### 许可证

MIT
