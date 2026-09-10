# FreeToken

一个基于 Go 的**多账号 AI 模型聚合网关**——聚合多个上游账号的免费 Token，对外提供统一的 OpenAI 兼容 API。

自动在账号间负载均衡、自动熔断限流，让你在本地就能拥有源源不断的免费 AI 调用能力。

---

## 功能一览

| 功能 | 说明 |
|------|------|
| **多账号聚合** | 把多个上游账号的免费 Token 合并为一个池子，自动轮询调用 |
| **OpenAI 兼容 API** | 对外暴露 `/v1/chat/completions`、`/v1/images/generations`、`/v1/models`，与所有主流 AI 工具链无缝对接 |
| **负载均衡** | 多账号自动轮换，遇到限流自动切换到其他账号，实现高可用 |
| **熔断机制** | 某账号触发 429 / 错误时自动冷却，冷却后自动恢复，避免雪崩 |
| **模型支持** | 支持文本对话 + 图片生成，可自定义模型优先级 |
| **省钱统计** | 自动按各模型官网价格估算每次调用"省了多少钱"，实时可视化 |
| **Web 管理页** | 内置美观的管理面板，实时查看用量、模型状态、省钱金额、账号健康度 |
| **单文件部署** | 一个可执行文件 + 一个配置文件，零依赖、零安装 |
| **开机自启** | 附带 Windows 自启动脚本，双击即可后台运行 |

## 支持的模型

| 模型 | 类型 | 官网定价（输入/输出 每百万 Token） |
|------|------|--------------------------------------|
| deepseek-v4-flash | 文本 | ¥3 / ¥9 |
| deepseek-v4-pro | 文本 | ¥9 / ¥27 |
| glm-5.2 | 文本 | ¥8 / ¥28 |
| kimi-k3 | 文本 | ¥20 / ¥100 |
| 6.8-flash-lite | 文本 | ¥5 / ¥30 |
| u1-fast | 图片 | — |
| u1.5-lite | 图片 | — |

> 价格来源：各模型官网定价，用于"省钱金额"估算。可在 `config.json` 中自定义修改。

## 快速开始

### 前提

- [Go 1.21+](https://go.dev/dl/)（仅编译时需要）
- 若干上游平台 API Key（每个账号一个，可在模型开放平台注册获取，需替换为你的上游地址）

### 1. 编译

```bash
go build -o freetoken-gateway.exe .
```

### 2. 配置

复制示例配置，填入你自己的 API Key：

```bash
copy config.example.json config.json
```

编辑 `config.json`：

```json
{
  "listen": "127.0.0.1:18888",
  "upstream_base": "https://api.example.com/v1",
  "timeout_seconds": 30,
  "cooldown_seconds": 300,
  "cooldown_429_seconds": 600,
  "disable_stream": false,
  "max_tokens_limit": 16384,
  "priority_models": [
    "deepseek-v4-flash",
    "glm-5.2",
    "deepseek-v4-pro",
    "kimi-k3",
    "6.8-flash-lite"
  ],
  "model_prices": {
    "deepseek-v4-flash": { "input": 3, "output": 9 },
    "deepseek-v4-pro": { "input": 9, "output": 27 },
    "glm-5.2": { "input": 8, "output": 28 },
    "kimi-k3": { "input": 20, "output": 100 },
    "6.8-flash-lite": { "input": 5, "output": 30 }
  },
  "accounts": [
    {
      "alias": "账号1",
      "api_key": "sk-你的第一个APIKey",
      "models": ["deepseek-v4-flash", "glm-5.2"]
    },
    {
      "alias": "账号2",
      "api_key": "sk-你的第二个APIKey",
      "models": ["deepseek-v4-flash", "glm-5.2"]
    }
  ]
}
```

**配置项说明**：

| 字段 | 说明 |
|------|------|
| `listen` | 网关监听地址，默认 `127.0.0.1:18888` |
| `upstream_base` | 上游 API 地址 |
| `timeout_seconds` | 单次请求超时时间（秒） |
| `cooldown_seconds` | 账号出错后的冷却时间（秒） |
| `cooldown_429_seconds` | 账号被限流（429）后的冷却时间（秒），建议比普通冷却更长 |
| `disable_stream` | 是否禁用流式输出（SSE） |
| `max_tokens_limit` | 单次请求最大 token 数限制 |
| `priority_models` | 模型优先级（数组越靠前越优先使用） |
| `model_prices` | 各模型官方定价（用于省钱金额估算） |
| `model_quotas` | 各模型每日配额限制（留空为不限制） |
| `accounts` | 账号列表，每个账号一个 `api_key` |

### 3. 启动

```bash
# 直接启动（前台，方便看日志）
./freetoken-gateway.exe

# 或使用配置文件指定路径
./freetoken-gateway.exe -config config.json

# Windows 后台静默启动（开机自启/挂机场景）
# 双击 start_hidden.vbs 即可
```

### 4. 访问

| 地址 | 说明 |
|------|------|
| `http://127.0.0.1:18888` | 重定向到管理面板 |
| `http://127.0.0.1:18888/admin` | Web 管理面板（可视化配置、用量统计、省钱金额） |
| `http://127.0.0.1:18888/v1/models` | 模型列表 |
| `http://127.0.0.1:18888/v1/chat/completions` | 文本对话 API |
| `http://127.0.0.1:18888/v1/images/generations` | 图片生成 API |

## 接入到你的应用

### OpenAI SDK（Python）

```python
from openai import OpenAI

client = OpenAI(
    base_url="http://127.0.0.1:18888/v1",
    api_key="sk-随便填"  # 网关不校验，转发时用真实账号的 key
)

response = client.chat.completions.create(
    model="deepseek-v4-flash",
    messages=[{"role": "user", "content": "你好"}],
    stream=True
)

for chunk in response:
    print(chunk.choices[0].delta.content or "", end="", flush=True)
```

### cURL

```bash
curl -X POST http://127.0.0.1:18888/v1/chat/completions \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer sk-随便填" \
  -d '{
    "model": "deepseek-v4-flash",
    "messages": [{"role": "user", "content": "你好"}]
  }'
```

### Node.js

```javascript
import OpenAI from 'openai';

const client = new OpenAI({
  baseURL: 'http://127.0.0.1:18888/v1',
  apiKey: 'sk-随便填'
});

const stream = await client.chat.completions.create({
  model: 'deepseek-v4-flash',
  messages: [{ role: 'user', content: '你好' }],
  stream: true
});

for await (const chunk of stream) {
  process.stdout.write(chunk.choices[0]?.delta?.content || '');
}
```

## Web 管理面板

打开 `http://127.0.0.1:18888/admin` 可看到实时管理面板：

- **用量统计** — 今日 / 本月 / 今年 / 累计 Token 消耗，近 30 天趋势图
- **省钱金额** — 按官方定价估算每次调用"省了多少钱"
- **模型状态** — 每个模型当前的可用账号数、冷却状态、RPM 指标
- **账号管理** — 增删改查账号，实时生效
- **配置管理** — 在线修改监听地址、超时、冷却时间等，保存后重启生效
- **图片历史** — 浏览通过网关生成的历史图片

## 工作原理

```
┌──────────┐      ┌──────────────────┐      ┌──────────────┐
│  你的应用  │ ──► │   FreeToken 网关  │ ──► │  上游 API   │
│          │ ◄── │                  │ ◄── │              │
└──────────┘      └──────────────────┘      └──────────────┘
                        │
                   ┌────┴────┐
                   │ 账号池   │  自动轮换 + 熔断恢复
                   │  - 账号1 │
                   │  - 账号2 │
                   │  - 账号3 │
                   │  ...    │
                   └─────────┘
```

1. 你的应用发起 OpenAI 格式请求到网关
2. 网关从账号池中选取一个**健康**的账号（自动跳过冷却中的）
3. 将请求转发到上游 API，使用选中账号的真实 API Key
4. 返回结果给应用，同时记录 token 用量用于省钱统计
5. 若请求失败（限流/超时/错误），自动切换下一个账号重试，直到成功或全部冷却

## 省钱计算原理

网关每天按日统计各模型的 token 消耗量，根据 `model_prices` 中配置的官方定价，计算出"如果直接用付费 API 需要花多少钱"。

- 省钱金额按**每日实际模型分布**精确计算（不混合不同天的模型比例）
- 按日/月/年/累计四个维度统计
- 所有数据持久化到 `usage.json` 和 `stats.json`，重启不丢失

## 文件说明

```
FreeToken/
├── main.go                 # 网关主程序（Go 单文件，内嵌管理页面）
├── go.mod                  # Go 模块定义
├── admin.html              # Web 管理面板（内嵌在二进制中）
├── config.example.json     # 配置示例（复制为 config.json 后使用）
├── config.json             # 实际配置（.gitignore 排除，不会提交）
├── LICENSE                 # MIT 开源协议
├── README.md               # 本文件
├── .gitignore              # Git 忽略规则
├── start_hidden.vbs        # Windows 后台静默启动脚本
├── restart_gw.vbs          # Windows 一键重启脚本（需管理员）
├── usage.json              # Token 用量数据（运行时自动生成）
├── stats.json              # 模型用量明细（运行时自动生成）
├── cooldown.json           # 账号冷却状态（运行时自动生成）
├── image_history.json      # 图片生成历史（运行时自动生成）
├── images/                 # 生成的图片存储目录（运行时自动创建）
└── gateway.log             # 网关日志（运行时自动生成）
```

## 常见问题

**Q: API Key 从哪获取？**

A: 在模型开放平台注册账号即可获取（替换为你的上游地址）。每个账号一个 Key，多个账号就可以组成免费 Token 池。

**Q: 为什么有的账号显示冷却中？**

A: 当某个账号触发了上游的限流（HTTP 429）或返回错误时，网关会自动让它冷却一段时间（`cooldown_seconds` / `cooldown_429_seconds`），冷却后自动恢复。冷却中的账号会被自动跳过，不影响整体可用性。

**Q: 省钱金额为什么有时会波动？**

A: 本项目按每日实际模型分布精确计算省钱金额，与当天实际使用的模型比例保持一致。如果某天大量使用了高价模型（如 kimi-k3），那天的省钱金额就会显著更高。这是正常的，不是 bug。

**Q: 如何修改监听端口？**

A: 编辑 `config.json` 中的 `listen` 字段，或在 Web 管理面板中修改后保存重启。

**Q: 可以在 Linux / macOS 上运行吗？**

A: 可以。Go 代码是跨平台的，编译时指定目标平台即可：

```bash
# Linux x64
GOOS=linux GOARCH=amd64 go build -o freetoken-gateway .

# macOS (Apple Silicon)
GOOS=darwin GOARCH=arm64 go build -o freetoken-gateway .
```

VBS 启动脚本仅适用于 Windows。

## 安全说明

- `config.json` 包含 API Key 等敏感信息，**不要提交到 Git 仓库**（`.gitignore` 已自动排除）
- 网关默认只监听 `127.0.0.1`，仅本机可访问。如需对外提供服务，请务必配置鉴权或仅暴露到内网
- 建议定期备份 `usage.json` 和 `stats.json`，它们记录了你所有的用量统计

## 许可证

本项目基于 [MIT License](LICENSE) 开源。

**Copyright (c) 2026 白师兄**

你可以自由地使用、修改、分发本项目，包括用于商业用途。唯一的要求是在你的项目中保留原始的版权声明和许可证文本。

## 免责声明

- 本项目仅供学习和技术研究使用
- 请遵守上游平台的使用条款和频率限制
- 作者不对因使用本项目导致的任何直接或间接损失负责
- 请在合法合规的前提下合理使用免费 Token 资源

---

**如果这个项目对你有帮助，欢迎给个 Star ⭐**
