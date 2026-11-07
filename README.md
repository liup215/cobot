# cobot

> 纯 Go（无 CGO）的 Agent 基础框架 —— 从 gline（Cline 血统）提取重构，供 Maestro、LTEDU 等项目进程内驱动 LLM agent。自带默认工具集（read/grep/glob/run/…）与可复用能力层（shell 执行器、docparse 文档解析）；依赖策略：只引入纯 Go 第三方库（gopdf/striprtf/word-extractor/xls，均为文档解析所用），不再宣称零依赖。

[![Release](https://img.shields.io/github/v/tag/liup215/cobot?label=release&sort=semver)](https://github.com/liup215/cobot/releases/tag/v0.1.0)
[![Go](https://img.shields.io/badge/go-1.25-00ADD8?logo=go&logoColor=white)](https://go.dev)
[![Dependencies](https://img.shields.io/badge/dependencies-0-brightgreen)](go.mod)
[![License](https://img.shields.io/badge/license-MIT-blue)](LICENSE)
[![Tests](https://img.shields.io/badge/tests-go%20test%20%2Drace%20%E2%9C%93-success)](memory-bank/progress.md)

[English](README_EN.md) | 简体中文

**状态**：M0–M5 已完成（`go test -race ./...` 全绿）：根包门面、Anthropic 适配器、compaction 双策略（Trim 移植 + Summarize 新写）、工具行为定稿、examples 四件套与中英文档。当前发布：**v0.1.0**（见[版本说明](#版本与发布)）。规格唯一来源：[memory-bank/requirements.md](memory-bank/requirements.md)。

## 快速上手

```go
import "github.com/liup215/cobot"

agent, err := cobot.NewAgent(cobot.Config{
	APIKey:      "sk-...",   // OpenAI 兼容端点；也可注入 Provider
	Model:       "gpt-4o",
	AutoApprove: true,
})
res, err := agent.Run(ctx, "fix the login bug")
// res.FinalText      attempt_completion 的 result 参数（无终结工具时为最后一条 assistant 文本）
// res.InputTokens / res.OutputTokens  本次 run 各回合用量之和
// res.Turns / res.Duration / res.Err 完成回合数、耗时与终结错误（nil = 正常结束）
```

单一 `Config` 入口（根包门面）：所有字段可选，未设置的字段回落到各字段文档写明的默认值。
`Run` / `RunWithCallback` 返回 `(*RunResult, error)`：`err == nil` 即本次 run 正常结束（含 `Abort()`）；
`ctx` 取消会中断流式读取与工具执行并返回 `errors.Is(err, context.Canceled)` 为真的包装错误；
宿主的 `ask_followup_question` 回调返回错误时，错误文本作为工具结果回传给模型、run 继续由模型自行决策。

可离线运行的完整示例见 [`examples/`](examples/)：[`basic`](examples/basic)（最小 run）、
[`streaming`](examples/streaming)（流式事件）、[`custom-tool`](examples/custom-tool)（注册自定义工具）、
[`guard`](examples/guard)（审批门三态）。四个示例都用 scripted provider 回放 LLM 轮次，
`go run ./examples/<name>` 即可，无需 API key。

## Config 全字段表

所有字段可选，除 `APIKey`/`Provider` 二选一。

| 字段 | 类型 | 默认 | 说明 |
|---|---|---|---|
| `APIKey` | `string` | 必填（除非注入 `Provider`） | OpenAI 兼容端点（OpenAI、火山方舟、DeepSeek、Kimi 等）的鉴权 key |
| `BaseURL` | `string` | 官方 OpenAI API | 端点覆盖，如 `https://ark.cn-beijing.volces.com/api/v3` |
| `Model` | `string` | provider 默认（`gpt-4`） | 发送给 provider 的模型名 |
| `Provider` | `cobot.Provider` | nil | 注入完全自定义后端（非 OpenAI 协议、测试替身），设置后绕过 `APIKey`/`BaseURL` |
| `Tools` | `*cobot.ToolRegistry` | `tools.InitDefaultRegistry()`（内置十件套） | 暴露给模型的工具注册表；扩展默认工具集可从 `InitDefaultRegistry()` 起步再 `Register` |
| `Mode` | `cobot.Mode` | `ModeAct` | 初始模式：`ModeAct` 执行修改；`ModePlan` 只探索规划（写工具隐藏） |
| `AutoApprove` | `bool` | false | yolo 模式跳过逐工具审批；推荐用 `Guard` 控制执行（见 `Guard` 行） |
| `MaxConsecutiveMistakes` | `int` | 3 | 模型连续多少回合未调用工具即中止 run |
| `CustomRules` | `string` | "" | 追加到系统提示词末尾的额外规则；`SystemPromptOverride` 生效时被忽略 |
| `SystemPromptOverride` | `string` | "" | 非空时**完全替换**默认系统提示词（宿主自带角色提示词用）；`AVAILABLE TOOLS` 段仍由 loop 按 Registry 渲染；`CustomRules`/`AgentName` 被忽略 |
| `AgentName` | `string` | `"cobot"` | 系统提示词角色段的 agent 身份；`SystemPromptOverride` 生效时被忽略 |
| `MaxTokens` | `int` | 128000 | 会话上下文 token 预算（compaction 触发水位按此计算） |
| `RetryDelays` | `[]time.Duration` | `DefaultRetryDelays`（1s/2s/4s/8s） | 流建连失败自动重试的退避序列（服务端 `Retry-After` 更长时优先）；nil = 默认，空切片 = 关闭。只重试同步建连失败，永不重试流中错误 |
| `Compaction` | `cobot.Strategy` | `Combo(Summarize, Trim)` | 60% 预请求检查与 80% 挂点的压缩策略；预发送 100% 始终是硬修剪（不花 LLM 调用） |
| `Summarizer` | `cobot.Summarizer` | 会话 provider 单次非流式调用 | 默认压缩策略的摘要器；`Compaction` 显式设置时被忽略 |
| `SummarizePrompt` | `string` | `DefaultSummarizePrompt` | 摘要指令覆盖（转录始终附在提示词后）；`Compaction` 显式设置时被忽略 |
| `Guard` | `cobot.ToolGuard` | nil | 审批门：每次工具执行前（主循环 + 预派发两条路径）咨询；见[嵌入指南](#嵌入指南) |
| `Store` | `cobot.Store` | nil（禁用持久化） | 任务/消息/工具调用持久化钩子 |
| `Title` | `string` | "" | 新建任务的标题 |

## 嵌入指南

宿主（如 Maestro）把 cobot 嵌进自己进程的三件核心事：**给它工具、给它人格、从它手里拿结果**。

### 1. 注册自定义工具（注入宿主内置工具）

实现 `tools.Tool` 接口（`Name`/`Description`/`InputSchema`/`Execute`），包一层
`tools.ToolInfo` 注册进 Registry，再通过 `Config.Tools` 交给 agent：

```go
reg := tools.InitDefaultRegistry()           // 保留内置工具
reg.Register(&tools.ToolInfo{
	Tool:         &myHostTool{},             // 你的 Go 实现
	Category:     tools.CategoryCommand,     // 信息性分类
	AllowedModes: []string{"plan", "act"},   // 哪些模式可用（"*" = 全部）
})
agent, _ := cobot.NewAgent(cobot.Config{Tools: reg})
```

工具的 `Description` 与 `InputSchema` 会出现在系统提示词 `AVAILABLE TOOLS` 段，就是模型的文档——
写清楚何时该用、参数含义。完整可运行版本：[`examples/custom-tool`](examples/custom-tool)。

### 2. 接管系统提示词

`SystemPromptOverride` 非空时**完全替换**默认系统提示词（默认的角色、规范、自定义规则全部不生效），
宿主自带完整角色提示词即可。唯一例外：`AVAILABLE TOOLS` 段（工具名、描述、JSON schema，按当前
模式过滤）仍由 loop 依据 Registry 渲染追加——没有它模型看不到任何工具 schema，宿主就得手工重述。

```go
agent, _ := cobot.NewAgent(cobot.Config{
	SystemPromptOverride: "你是 XX 助手……（宿主完整提示词）",
})
```

### 3. 读取 RunResult

`Run`/`RunWithCallback` 返回 `*cobot.RunResult`，与产生它的这一次 run 绑定：

| 字段 | 语义 |
|---|---|
| `FinalText` | 交付物：`attempt_completion` 的 `result` / `plan_mode_respond` 的 `response`；其他结束方式 best-effort 取本次 run 最后一条 assistant 文本。交付物在 assistant 回合记录时捕获，mid-run compaction 重写转录本既不会清空它、也不会串到上一 run |
| `InputTokens` / `OutputTokens` | 本次 run 各回合 provider 上报用量之和（provider 不报则恒为 0） |
| `Turns` | 完成（读到流末尾）的 LLM 回合数；重试建连与被错误/取消截断的回合不计 |
| `Duration` | run 墙钟耗时 |
| `Err` | 终结错误，nil = 正常结束；与 `Run`/`RunWithCallback` 返回的 error 一致 |

取消语义：`ctx` 取消是硬中断（中断流读取与工具执行，含预派发 goroutine），
`errors.Is(err, context.Canceled)` 成立；`Abort()` 是协作式检查点语义（不打断进行中的流读取），
两者并存时取消优先。

### 4. 其他挂钩

- **流式 UI**：`RunWithCallback(ctx, prompt, cb)` 传 `cobot.Callback` 实现（嵌入
  `cobot.CallbackAdapter` 只覆盖需要的事件）：`OnContent`/`OnReasoning` 文本与思考增量、
  `OnToolCallStart/Update/Complete` 工具生命周期、`OnCompactionStart/End` 压缩事件、
  `OnRetry` 建连重试、`AskFollowupQuestion` 澄清交互。见 [`examples/streaming`](examples/streaming)。
- **审批门**：`Config.Guard`（`cobot.ToolGuard`）在每次工具执行前咨询（主循环与预派发两条路径，
  可能对同一调用咨询两次 → 必须无副作用、幂等）。三态：放行 `(true, "", nil)`；拒绝
  `(false, reason, nil)`（工具不执行，`[DENIED] <reason>` 回传模型，run 继续）；
  错误包裹 `cobot.ErrAbortRun` → 整个 run 立即结束；其他非 nil 错误 fail-closed 按拒绝处理。
  见 [`examples/guard`](examples/guard)。
- **持久化**：实现 `cobot.Store` 挂接任务/消息/工具调用存储；nil 安全，禁用即不接入。
- **压缩调优**：`Config.Compaction` 接受任意 `compact.Strategy` 实现（内置
  `compact.NewTrim()` / `compact.NewSummarize(...)` / `compact.NewCombo(...)`）。

## 长任务上下文压缩（compaction）

超过上下文预算 60% 时，最旧的对话轮被压缩；默认策略是“先摘要、失败回退修剪”：

- **Summarize**：保留最近 `KeepRounds`（默认 6）轮原文，更早的轮渲染成转录交给
  `Summarizer`（默认 `loop.ProviderSummarizer`，用会话自己的 provider 单次非流式
  调用），替换为一条五段结构化摘要（任务目标 / 已完成步骤 / 关键决定 / 未决问题 /
  文件清单）。提示词可通过 `Config.SummarizePrompt` 覆盖；超时（默认 120s）、
  provider 错误、空摘要、未配置 summarizer 都视为失败。
- **Trim**（gline 水位修剪移植）：不调用 LLM，从最旧轮开始丢弃直到回到 60% 水位
  以下；始终保留 system 消息、首条用户消息（原始任务）与最近 6 轮；仍超 80% 时
  回落 gline 硬修剪。
- **Combo**：Summarize 优先，任何失败回退 Trim；两者都失败时 loop 记日志继续跑，
  预发送硬修剪兜底。

压缩以“轮”为单位（一条用户消息及其全部响应，`tool_call` 与工具结果永不分家），
两种 wire 格式都要求 `tool_use` 与 `tool_result` 相邻，逐条丢弃会产生悬空工具结果。

```go
// 自定义：只修剪，不花摘要调用
agent, _ := cobot.NewAgent(cobot.Config{
	Compaction: compact.NewTrim(),
})
```

## 内置工具行为定稿

默认 Registry（`tools.InitDefaultRegistry()`）注册十件工具，行为均已用测试钉死
（`tools/behavior_test.go`）：

| 工具 | 行为定稿 |
|---|---|
| `read` | 按行分页读取：默认 1000 行、`limit` 上限 2000 行、块字节上限 50KB（超出截断并附 `[Chunk truncated … Continue with line_number=N]` 指路，rune 边界安全）；`~` 展开、相对路径绝对化；**文档提取（docparse 注册表）**：PDF/Word(含 .doc)/Excel(含 .xls)/PPT/ODF/RTF/HTML/EPUB 自动抽文本后按行分页（头标注 `text extracted from …`，`line_number` 作用于提取文本），zip 返回条目清单，无文本层二进制返回友好提示（不吐垃圾）；图片文件返回元数据而非二进制 |
| `write` | 原子写：同目录临时文件 + `rename`，保留原文件 mode；写失败不破坏原文件。自动创建目录 |
| `edit` | 所有块先在内存中应用，任一 `search` 失配 → 整文件不动并点名失败块（all-or-nothing）；结果同样原子写回 |
| `run` | **shell 解析优先 bash**（Windows 找 Git Bash、跳过 WSL stub，找不到才 `cmd /C`，Unix `bash`→`sh`），语法提示烘进工具描述；前台阻塞，默认超时 120s（`timeout` 按调用覆盖，上限 600s）；超时杀整个进程树（Windows `taskkill /F /T` + `WaitDelay` 兜底）；stdout/stderr 分离采集，各截断 32KB（保头 3/4 + 尾 1/4 + 标记，rune 边界安全）；非 0 退出码是结果不是错误；`requires_approval` 或破坏性命令走审批钩子（`SetApprovalHandler`，fail-closed）。不用于长驻服务 |
| `grep` | **ripgrep 快路径**（安装 `rg` 时：并行遍历、尊重 .gitignore、达到上限即杀扫描），缺失时回落纯 Go 并发遍历；结果确定性排序（path → line → column，稳定排序）；`TotalMatches` 报告真实命中数；超过 500 条截断并附 `[Results truncated: …]` 通知 |
| `ls` | 目录条目按文件名排序（`os.ReadDir` 语义，已钉死）；超过 500 条截断并附通知 |
| `glob` | 按文件名 glob 递归查找；**fd 快路径**（安装 `fd` 时：并行 + .gitignore 感知），缺失回落纯 Go walk；最多返回 500 条 |
| `attempt_completion` | 终结工具：必填 `result`，可选 `command`（建议用户复核结果的命令） |
| `ask_followup_question` | 澄清问题，不终结运行；`SetHandler` 注入交互端（TUI/宿主），未注入时回退 stdin；支持按选项序号作答；headless 下 handler 返回错误时错误文本回传模型、run 继续 |
| `plan_mode_respond` | Plan 模式终结工具：响应原样回显给用户 |

文件/命令类工具**不做工作目录沙箱**（gline 基线语义）：路径可以指向工作目录之外，
执行控制请通过 `Config.Guard`（`ToolGuard`）实现。

## 设计要点（v0.1）

- `github.com/liup215/cobot`，Go 1.25，运行时依赖 = 0（仅 stdlib）
- 包结构：根包门面（`cobot.NewAgent(Config)`）/ `types` / `provider`（openai 兼容 + **anthropic** + mock/scripted）/ `tools`（工具注册表 + ToolGuard + XML 兜底）/ `loop` / `compact`（Trim + Summarize + Combo）/ `examples`
- 继承 gline（Cline）：`attempt_completion` 终结协议、Plan/Act、纠错环、预派发、reasoning 通道
- 新增（借鉴 pi）：Anthropic 流式适配器、LLM 摘要式 compaction、PreToolUse 审批门、自动重试
- 明确不做：TUI、extensions、会话树、OAuth、MCP、编排

## 版本与发布

- **v0.1.0**（[release](https://github.com/liup215/cobot/releases/tag/v0.1.0)）：tag 指向
  M0–M3 完成点（根包门面、openai 兼容 provider、anthropic 适配器、tools/loop 骨架、
  ToolGuard、Store、自动重试、mock/scripted provider）。**不含** M4 compaction 双策略
  与 M5 examples/README —— 这些在 main 分支上，将随后续 tag（v0.1.1 / v0.2.0）发布。
  以 `go get` 固定引用时请注意这一差异。已知限制见 release notes。
- 里程碑明细（M0–M5）见 [`memory-bank/progress.md`](memory-bank/progress.md)。

## License

MIT
