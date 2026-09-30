# cpa-plugin-opencodezen

<div align="center">

![logo](logo.svg)

**OpenCode Zen free-tier models as a native CLIProxyAPI provider**

[![Go](https://img.shields.io/badge/go-1.26-00ADD8?logo=go)](https://go.dev)
[![License: MIT](https://img.shields.io/badge/license-MIT-green.svg)](LICENSE)
[![Release](https://img.shields.io/github/v/release/Victor9578/cpa-plugin-opencodezen?include_prereleases)](https://github.com/Victor9578/cpa-plugin-opencodezen/releases)

</div>

用于 [CLIProxyAPI](https://github.com/router-for-me/CLIProxyAPI) 的 **原生执行器插件**，让你可以直接在 CLIProxyAPI 中无痛使用 [OpenCode Zen](https://opencode.ai/zen) 的免费系列模型（如 `mimo-v2.6-flash-free`、`muse-spark-1.3-contributor-free` 等），无需修改或重新编译 CPA 宿主二进制。

---

## 解决的问题

1. **端点路由自动分流（Endpoint Split）**：
   - OpenCode Zen 的部分模型（如 `muse-spark`）只响应 `/responses` 端点，而其余大部分免费模型响应 `/chat/completions`。
   - 插件内置智能分流规则：识别 `muse` 自动走 `/responses`，其他模型自动走 `/chat/completions`，无需手动在配置中区分。

2. **免费层门禁自动伪装（FreeTier Gate）**：
   - Zen 服务端对未通过官方 CLI 发起的请求有严格的校验（检查 `ses_` / `msg_` 规范格式会话头、客户端特征头 `cli`、`bash`/`read` 工具集、强制 `stream: true` 等），缺一不可，否则返回 `403 FreeTierError` 并封禁 Key 约 15 分钟。
   - 插件全自动伪装为官方 CLI 规范，确保百分百顺利通行。

3. **SSE 流式传输重拼与保活过滤（Stream Reassembly）**：
   - 修复上游分片造成的跨包 SSE 截断问题，确保下游收到的每个 `data: {...}` 均为完整合法的 JSON 对象。
   - 自动过滤上游 `: keep-alive` 心跳注释帧，避免 LobeHub、NextChat 等客户端因非法 JSON 报错崩溃。
   - 不转发上游 `[DONE]` 终止符（宿主会在插件流关闭后自动补齐，避免重复）。

4. **Responses → Chat 双向格式转换（Format Conversion）**：
   - 插件向宿主声明 `chat-completions` 输出格式，宿主据此为 responses/claude/gemini 客户端做翻译。
   - 走 `/responses` 上游的模型（如 `muse-spark`）返回的是 Responses API 事件流，插件会将其实时转换为 `chat.completion.chunk` 帧（含 role 首帧、增量 content、带 usage 的收尾帧），非流式请求则折叠为标准 `chat.completion` 对象。
   - 修复此前 muse 系列模型流式返回 `empty_stream`、非流式返回无法解析载荷的问题。

5. **无感集成，原生提供商体验**：
   - 配置完全脱离插件管理面板，用户只需在凭证目录或 Web 认证面板中添加一个标准的 `zen` 认证文件即可，服务地址与 API Key 支持动态感知生效。

---

## 安装方法

从 [Releases](https://github.com/Victor9578/cpa-plugin-opencodezen/releases) 下载适用于你系统架构的编译包（例如 `zen_0.3.0_linux_amd64.zip`）。

解压后将 `zen.so`（或 `zen.dylib` / `zen.dll`）放入 CPA 的插件目录（文件名必须为 `zen.so` / `zen.dylib` / `zen.dll`）：

```bash
/CLIProxyAPI/plugins/linux/amd64/zen.so
```

---

## 使用指南

### 1. 启用插件（`config.yaml`）

在 `config.yaml` 中启用插件即可：

```yaml
plugins:
  enabled: true
  dir: "plugins"
  configs:
    zen:
      enabled: true
      # 可选：直接在插件配置里添加 OpenCode key（逗号分隔，每个 key 自动成为一条虚拟凭证）
      # api-keys: "oc_sk_xxx,oc_sk_yyy"
      # 可选：从 /models 自动发现的模型中排除指定模型（逗号分隔，大小写不敏感）
      # exclude-models: "jev-1.13-free,space-bunny-free"
```

> `api-keys` 和 `exclude-models` 也可以在 CPA Web 管理中心的插件配置页直接填写。

### 2. 添加凭证（认证目录）

在 CPA 的 `auth-dir`（例如 `/root/.cli-proxy-api/`）目录下创建一个 JSON 文件（例如 `zen-key.json`），或通过 Web 管理中心添加：

```json
{
  "type": "zen",
  "provider": "zen",
  "api_key": "sk-your-zen-api-key-here",
  "base_url": "https://opencode.ai/zen/v1"
}
```

> **说明**：
> - 插件默认已注册以下免费模型：`mimo-v2.6-flash-free`、`mimo-v2.5-free`、`ling-3.0-flash-fin-free`、`nemotron-3-ultra-free`、`muse-spark-1.3-contributor-free`。
> - 插件会自动从 zen `/models` 接口发现新的免费模型（`*-free` 后缀，10 分钟缓存）并注册，无需重新编译；不想用的模型可用 `exclude-models` 排除。
> - 也可以在凭证 JSON 文件中添加 `"models": [{"name": "新模型名"}]` 手动指定。
> - 端点（chat / responses）自动识别：注册时按模型名推断，请求 400 时自动换端点重试并记住。

### 3. 模型能力元数据（上下文窗口等）

插件会向 CPA 宣告模型能力元数据（上下文窗口、输出上限、输入模态、推理档位），下游客户端（pi、Codex 等）通过 `/v1/models` 读取 `context_window` / `max_tokens` 等字段。内置默认值：mimo 系列为 1M 上下文。

如需覆盖或为其他模型声明能力，在 `config.yaml` 的 `plugins.configs.zen.models` 中配置：

```yaml
plugins:
  enabled: true
  dir: "plugins"
  configs:
    zen:
      enabled: true
      models:
        - model: nemotron-3-ultra-free
          context-length: 1000000        # 上下文窗口（token 数）
          max-completion-tokens: 32768   # 最大输出 token 数
          input-modalities:              # 输入模态：text / image
            - text
          reasoning-levels: none, low, medium, high  # 推理档位
```

---

## 验证与测试

```bash
# 重启 CPA
docker restart cli-proxy-api

# 测试聊天
curl -s http://localhost:8080/v1/chat/completions \
  -H "Authorization: Bearer $CPA_KEY" \
  -H "Content-Type: application/json" \
  -d '{"model":"mimo-v2.6-flash-free","stream":true,"messages":[{"role":"user","content":"Hello"}]}'
```

---

## 致谢与参考

- [cpa-plugin-opencode-session-mapper](https://github.com/ahoo/cpa-plugin-opencode-session-mapper) - 会话头映射插件的先行者，为会话 ID 规范化处理提供了思路。
- [OpenCode2API](https://github.com/TiaraBasori/OpenCode2API) - 早期的 OpenCode 接入探索，为 Zen 免费层伪装提供了宝贵参考。

---

## License

[MIT](LICENSE)
