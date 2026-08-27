# cobot 需求文档（v0.1）

> 状态：已评审待开发
> 决策日期：2026-08-28
> 本文档是 cobot 的唯一需求来源（Source of Truth）。开发前必读。
> 修订：2026-08-28 项目由 agentcore 更名为 cobot（module/仓库/文档，commit 726cdae），
> 本文模块路径与仓库目录已同步更新。

---

## 1. 项目定位

**cobot** 是一个**纯 Go、零第三方运行时依赖**的 Agent SDK，从 gline
（`C:/Users/22569/Documents/20-Projects/gline`，Cline 血统）的
`internal/agent` 模块提取重构而来，发布为独立 GitHub 包：

- 模块路径：`github.com/liup215/cobot`
- 仓库目录：`C:/Users/22569/Documents/20-Projects/cobot`
- License：MIT
- Go 版本：go 1.25（向上兼容 maestro 的 1.26）

### 1.1 背景：为什么做这个

Maestro（个人 Agent OS，Go daemon）当前通过子进程调用 Claude Code CLI
执行任务，存在系统级外部依赖（Node.js/claude CLI）。目标是让 Maestro
成为**纯 Go 单二进制**应用：Worker/评审/需求发起等角色全部由进程内
Go agent 驱动。

前期调研（2026-08-28）评估过三条路线：

| 路线 | 结论 |
|---|---|
| pi agent（TS）RPC 子进程 | 体验好但保留外部运行时依赖，违背纯 Go 目标 |
| 移植 pi 到 Go | MIT 允许，但 Tier-1 需 6-10k LOC、1-2 个月，且追着上游跑 |
| **提取 gline agent → cobot（选定）** | 代码自有（MIT）、Go 原生、约 1.5-2.5 周；Cline 的"领任务→自主执行→attempt_completion 交付"哲学与 Maestro Worker 场景天然匹配 |

### 1.2 与 gline / pi 的设计哲学关系

- **继承 gline（Cline）**：`attempt_completion` 终结协议、Plan/Act 双模式、
  原生 function calling 为主 + XML 工具调用兜底、无工具调用纠错环
  （`noToolsUsedMsg` 注入 + 连续错误熔断）、流式期间预派发只读工具、
  reasoning_content 独立通道。
- **借鉴 pi（仅两处）**：① LLM 摘要式 compaction（gline 只有水位线裁剪，有损）；
  ② 流式请求失败自动重试（指数退避）。其余 pi 能力（会话树、TUI、
  steer/followUp、extensions）**明确不做**——Maestro 是无头场景。

---

## 2. 目标与非目标

### 2.1 目标（v0.1）

1. 从 gline 提取 agent 内核，形成独立可发布的 Go module。
2. 运行时依赖**为零**（仅标准库）；测试依赖也为零（用标准库 testing）。
3. 新增原生 Anthropic Messages 适配器（流式 SSE + thinking + cache_control）。
4. 提供 PreToolUse 审批门（ToolGuard），供 Maestro 实现人工审批/策略拦截。
5. 提供 LLM 摘要式 compaction 策略（可选，与水位线裁剪并存）。
6. 提供流式请求失败自动重试（指数退避 + Retry-After）。
7. 发布 v0.1.0 到 github.com/liup215/cobot，Maestro 以 embedded 后端接入。

### 2.2 非目标（明确不做）

- ❌ TUI / 交互式界面
- ❌ extensions 动态加载系统（TS/进程内插件）
- ❌ 会话树与分支（session JSONL trees）
- ❌ OAuth 订阅认证（Claude/Codex 订阅登录）——v0.1 只做 API Key
- ❌ MCP client / server（Maestro 已有自己的 MCP server；cobot 工具是原生 Go 注册）
- ❌ 记忆引擎（facts/RAG）、浏览器自动化（go-rod）、Excel/网页抓取工具
- ❌ 多 agent 编排 / subagents（Maestro 自己就是编排层）
- ❌ Skills 系统（Maestro 用自己的角色提示词体系）

以上均为 v0.2+ 的候选扩展点，v0.1 一律不做。

---

## 3. 架构与包结构

```
github.com/liup215/cobot
├── types/        # Message / ToolCall / TokenUsage / Conversation（收编 gline pkg/types，裁剪）
├── provider/     # Provider 接口 + StreamChunk + openai 兼容实现(迁移) + anthropic 实现(新写) + mock
├── tools/        # Tool / Registry 接口层 + v1 内置工具 + ToolGuard
├── loop/         # agent loop（gline RunWithCallback 拆分重构）+ Callback 事件接口
├── compact/      # Compactor 接口 + Trim 策略(移植) + Summarize 策略(新写)
└── examples/     # basic / streaming / custom-tool / guard
```

**依赖方向**（禁止反向）：

```
loop ──► provider ──► types
  │        ▲
  └──► tools ──► types
  └──► compact ──► types
```

**依赖政策（硬性约束）**：

- `cobot` 根模块 go.mod 的 `require` 中**不得出现任何第三方运行时依赖**（仅 stdlib）。
- 日志统一走 `log/slog`（gline 的 zerolog 不迁移）。
- 持久化不落依赖：`loop` 只定义 `Store` 接口，SQLite 等实现由使用方（Maestro/gline）提供。
- 无 CGO；Windows / Linux / macOS 三平台可编译、可测试（`go test ./...`）。

---

## 4. 功能需求

### 4.1 types 包

- 收编 gline `pkg/types` 中的 `Message`、`ToolCall`、`TokenUsage`、`Conversation`
  及 `EstimateTokens` 估算器。
- **保留**：`Role*` 常量、`ToolName` 枚举（裁剪到 v1 工具集）、
  `TrimToMaxTokens` / 水位线逻辑、token 估算（chars/4 启发式）。
- **增加**：`Estimator` 注入点——`type TokenEstimator func(string) int`，
  默认启发式，允许使用方按模型注入精确计数器。
- **剔除**：`render_strategy.go`（GUI 渲染）、slash_command、GUI 专用字段
  （`AvailableTools` 诊断字段可保留为可选 json.RawMessage）。

### 4.2 provider 包

接口与 gline 保持概念一致（降低迁移成本）：

```go
type Provider interface {
    CreateMessage(ctx context.Context, req *Request) (*Response, error)
    CreateMessageStream(ctx context.Context, req *Request) (<-chan StreamChunk, error)
    SupportsTools() bool
    GetModel() string
    GetProviderName() string
}
```

- `StreamChunk` 字段照搬 gline：`Content / ReasoningContent / ToolCall /
  IsPartial / FinishReason / Usage / Error / Done`。
- `Request`：`Messages / Tools / SystemPrompt / MaxTokens / Temperature / ToolChoice`。

**FR-P1 OpenAI 兼容 provider（移植 gline internal/api/openai.go）**
- chat/completions + SSE 流式解析、tool_calls 增量聚合、
  DeepSeek/Kimi 式 `reasoning_content` 透传、baseURL 可配置。
- 保留 gline 的 JSON 参数校验失败回环（错误信息含原始输入，让模型自纠）。

**FR-P2 Anthropic provider（新写，v0.1 最大增量）**
- POST /v1/messages，SSE 流式（`message_start` / `content_block_start|delta|stop` /
  `message_delta` / `message_stop`）。
- system prompt 走顶层 `system` 字段；工具用原生 `tools` + `input_schema`；
  tool 结果以 `tool_result` content block 回传。
- thinking 块：`ReasoningContent` 通道承载（与 gline 约定一致，不混入正文）。
- `cache_control`：system prompt 默认打 ephemeral 缓存点。
- `stop_reason` 映射：`end_turn→完成`、`tool_use→继续循环`、`max_tokens→截断标记`。
- 鉴权：显式 APIKey 或 `ANTHROPIC_API_KEY` 环境变量。baseURL 可配置（代理/中转）。
- 错误处理：识别 4xx/5xx、`Retry-After` 头、overloaded_error。

**FR-P3 mock provider**：脚本化回放（指定轮次返回指定 tool_calls/文本），
供 loop 单元测试与使用方演示，不依赖网络。

### 4.3 tools 包

- `Tool` 接口照搬 gline：`Name/Description/InputSchema/Execute(ctx, json.RawMessage) (string, error)`。
- `Registry`：注册/注销/按名获取/按模式过滤（`GetForMode` / `IsAllowed`）。
- **v1 内置工具集**（从 gline 裁剪，全部 stdlib 实现）：
  - `read_file`（大文件截断 + 行号）
  - `write_to_file`
  - `replace_in_file`（含 diff 计算与错误反馈，保留 gline 的容错逻辑）
  - `execute_command`（跨平台 shell 选择；超时与输出截断；**输出上限可配置**）
  - `search_files`（glob + 内容 grep 二合一）
  - `list_files`
  - `attempt_completion`（终结工具，保留）
  - `ask_followup_question`（可选启用；headless 场景由使用方注入应答 handler）
- **剔除**：browser_copy / kb_ingest / memory_note / use_skill / use_subagents /
  summarize_file / use_mcp_tool / access_mcp_resource。
- **FR-T1 ToolGuard（审批门，新增）**：

```go
type ToolGuard func(ctx context.Context, call ToolCall) (allow bool, reason string)

type Registry struct { /* ... */ Guard ToolGuard }
```

  - Guard 返回 false 时：该次调用产生一条工具结果消息
    `[DENIED] <reason>`，循环继续（模型可换方案）——对应 Maestro 人工审批门。
  - 哨兵错误 `ErrAbortRun` 可要求整体中止。
  - 副作用工具清单（write/replace/execute）默认建议配合 guard 使用。

- **FR-T2 XML 兜底解析（保留）**：无原生 tool_calls 时从正文解析
  `<tool_name>...参数...</tool_name>`（gline xml_tools.go 逻辑 + 测试）。
- 工具注册支持**使用方自定义工具**（Maestro 的 MCP 工具将以原生 Go tool 注册）。

### 4.4 loop 包（核心重构）

将 gline 1173 行的 `RunWithCallback` 拆为三段：**主循环 / 流处理 / 工具执行**。
对外行为保持 gline 语义：

- 主循环：构建 system prompt（工具描述注入）→ token 预算检查（60% 触发压缩、
  80% 强制裁剪）→ 请求 LLM → 处理流 → 执行工具 → 终结判断。
- **FR-L1 终结协议**：`attempt_completion` / `plan_mode_respond` 置 Complete；
  `ask_followup_question` 不终结（等待应答继续循环）。
- **FR-L2 纠错环（保留）**：有工具可用但模型未调用 → 注入 `noToolsUsedMsg`；
  连续错误计数达上限（默认 3，可配置）→ 返回错误终止。
- **FR-L3 预派发（保留）**：SSE 流期间并行执行**只读**工具；
  副作用工具（write/replace/execute + 使用方自定义副作用清单）排除在外；
  竞态防护逻辑（preDispatchedIDs/results 三锁）原样保留。
- **FR-L4 ToolChoice 策略（保留）**：有待办工作时 `tool_choice=auto`
  （DeepSeek/Kimi 用 auto，避免 required 混淆停止）。
- **FR-L5 Callback 事件接口**（对齐 gline StreamCallback + 新增事件）：

```go
type Callback interface {
    OnStreamStart()
    OnContent(delta string)
    OnReasoning(delta string)
    OnToolCallStart(call types.ToolCall)
    OnToolCallUpdate(call types.ToolCall, partial string)   // 新增：execute_command 流式输出
    OnToolCallComplete(call types.ToolCall, result string)
    OnCompactionStart() / OnCompactionEnd(ok bool)          // 新增
    OnError(err error)
    OnComplete()
}
```

- **FR-L6 ToolGuard 挂载点**：主循环与预派发两条路径都必须先过 guard。
- **FR-L7 Store 接口（可选，保留 gline 形状并裁剪）**：

```go
type Store interface {
    CreateTask(title, prompt, mode, provider, model, workDir string) (string, error)
    SaveMessage(taskID string, msg types.Message) error
    StartToolCall(taskID, name, input string) (int64, error)
    CompleteToolCall(id int64, result string) error
    FailToolCall(id int64, err string) error
    CompleteTask(taskID string) error
    FailTask(taskID, reason string) error
}
```

  nil 时全部跳过；Maestro 用自己的 SQLite 实现。

- **FR-L8 自动重试（新增）**：`CreateMessageStream` 建连失败或流中途错误 →
  指数退避重试（初始 1s，倍增，最多 4 次），尊重 `Retry-After`；
  重试期间回调 `OnError` 之前的 `OnRetry(attempt, err)`（可选事件）。
  4xx 参数类错误不重试。
- **FR-L9 中止**：`Abort()` 置位 + context 取消双通道；panic 安全
  （defer 恢复 running/abort 状态，保留 gline 现有实现）。

### 4.5 compact 包

```go
type Strategy interface {
    ShouldCompact(conv *types.Conversation) bool
    Compact(ctx context.Context, conv *types.Conversation) error
}
```

- **FR-C1 Trim 策略（移植 gline）**：60% 低水位 / 80% 高水位，
  保留 system 消息 + 最近轮次，丢弃最旧消息。
- **FR-C2 Summarize 策略（新增，借鉴 pi）**：触发时把最旧的 N 轮
  （保留最近 K 轮不动）交给 `Summarizer`（函数类型，由使用方实现——内部
  就是调 provider 生成摘要），替换为一 блок摘要消息，标注
  `[COMPACTED] Early conversation summarized:`。
  - 默认 K=6 轮；N = 超出水位线的部分。
  - Maestro 将用任意 provider 实例化 Summarizer。
- 默认组合：先 Summarize，失败/未配置时回退 Trim。
- token 预算估算含 system prompt 与工具描述 JSON（保留 gline enforceTokenBudget 逻辑）。

---

## 5. 非功能需求

| 编号 | 需求 |
|---|---|
| NFR-1 | 运行时第三方依赖为 0（go.mod require 仅 stdlib）；测试同理 |
| NFR-2 | `go vet` / `go test -race ./...` 全绿；Windows 本机为主要开发平台 |
| NFR-3 | 单文件 ~2000 行以内为上限——loop 拆分后单文件不超过 ~400 行 |
| NFR-4 | 所有公共 API 有 godoc 注释；示例可独立 `go run` |
| NFR-5 | 语义化版本；v0.1.0 为首个 tag |
| NFR-6 | 无网络即可跑全部单测（mock provider + httptest 假 SSE 服务） |

---

## 6. 测试要求

1. **移植 gline 现有测试**：agent_test / xml_tools_test / file_test /
   search_test / openai_test（改为 stdlib testing + httptest）。
2. **新增**：
   - Anthropic SSE mock 服务测试（含 thinking 块、tool_use 增量、
     cache_control、stop_reason 三分支、429+Retry-After 重试）。
   - ToolGuard 拒绝路径测试（主循环 + 预派发两处都要覆盖）。
   - Summarize compaction 测试（fake Summarizer）。
   - `go test -race` 并发预派发回归测试。
3. **examples 冒烟**：mock provider 驱动的 README 示例脚本可跑通。

---

## 7. 里程碑

| 里程碑 | 内容 | 验收标准 |
|---|---|---|
| **M0 仓库初始化** | go.mod（module github.com/liup215/cobot, go 1.25）、LICENSE(MIT)、.gitignore、README 骨架 | `go build ./...` 通过 |
| **M1 骨架迁移** | types/ + tools 接口层 + provider 接口层 + openai provider + mock provider；剔除 go-rod/excelize/zerolog/viper 等一切外部依赖 | `go list -m all` 无第三方包；gline 相关单测移植通过 |
| **M2 loop 重构** | RunWithCallback 拆分主循环/流处理/工具执行；ToolGuard；自动重试 | 单测覆盖终结协议/纠错环/预派发/guard 拒绝；`-race` 绿 |
| **M3 Anthropic 适配器** | SSE 流式 + thinking + cache_control + stop_reason 映射 + 重试集成 | httptest 假 SSE 全分支测试通过 |
| **M4 compaction 与工具集** | Trim + Summarize 双策略；v1 内置工具六件套 + attempt_completion | 大会话压缩测试；工具单测 |
| **M5 文档与发布** | README（中英）、examples 四个、Maestro embedded 后端接入验证 | Maestro `embedded` runner 跑通一次真实 demand 执行；`gh repo create` + push + `gh release create v0.1.0` |

---

## 8. 发布流程（gh-cli）

```bash
# 首次（M5 完成后，经用户确认再执行）
gh repo create liup215/cobot --public --source . --push --description "Pure-Go agent SDK extracted from gline"
git tag v0.1.0 && git push origin v0.1.0
gh release create v0.1.0 --title "v0.1.0" --notes "..."
# 后续版本：常规 git push + tag + gh release create
```

- 破坏性变更前先发 `v0.x`；API 稳定后升 v1。
- Go module proxy 会自动拾取 tag，无需额外注册。

---

## 9. 风险与对策

| 风险 | 对策 |
|---|---|
| gline loop 为单人验证强度 | 移植全部现有测试 + Maestro 自举（用它跑自己的 demand 当狗粮） |
| 1173 行单体拆分引入回归 | 拆分前先固化行为基线测试（用 mock provider 锁定现有行为），再动手 |
| Anthropic 适配器无真实 API 覆盖 | httptest 假 SSE 逐事件断言；联调由 Maestro 集成阶段补真实调用验证 |
| 预派发竞态 | 三锁结构原样保留 + `-race` 回归；副作用工具白名单默认关闭预派发 |
| 模块路径未来迁移 | v0 期间允许；v1.0 前冻结为 github.com/liup215/cobot |

---

## 10. 明确的继承/剔除清单（提取手术单）

**继承（来自 gline）**：Provider/StreamChunk/ToolChoice 抽象、OpenAI 兼容适配器、
agent 主循环行为（终结协议/纠错环/Plan-Act/预派发/存储回调）、
Tool/Registry、XML 兜底解析、内置工具（六件套）、水位线 compaction、
token 估算器、`noToolsUsedMsg` 文案、连续错误熔断。

**新写（借鉴 pi 或新增）**：Anthropic 适配器、Summarize compaction、
ToolGuard、自动重试、OnToolCallUpdate/OnCompaction* 事件。

**剔除（不迁移）**：wails GUI 全部、go-rod browser 工具、excelize/markdown/
web_fetch/readability 工具、memory 统一引擎（2566 行）、SQLite storage 实现
（保留接口）、zerolog、viper、skills 系统、subagent 模块、MCP client（2272 行）、
slash 命令、GUI api 层。

> 提取源以 gline @ 2026-06-27（commit fix-replace-in-file-errors）为基线快照。
