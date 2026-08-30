# cobot

> 纯 Go、零第三方依赖的 Agent SDK —— 从 gline（Cline 血统）提取重构，供 Maestro 等项目进程内驱动 LLM agent。

**状态**：M0–M4 已完成（`go test -race ./...` 全绿）：根包门面、Anthropic 适配器、compaction 双策略（Trim 移植 + Summarize 新写）与工具行为定稿。M5（examples + 发布）进行中。规格唯一来源：[memory-bank/requirements.md](memory-bank/requirements.md)。

## 快速上手

```go
import "github.com/liup215/cobot"

agent, err := cobot.NewAgent(cobot.Config{
	APIKey:      "sk-...",
	Model:       "gpt-4o",
	AutoApprove: true,
})
res, err := agent.Run(ctx, "fix the login bug")
// res.FinalText      attempt_completion 的 result 参数（无终结工具时为最后一条 assistant 文本）
// res.InputTokens / res.OutputTokens  本次 run 各回合用量之和
// res.Turns / res.Duration / res.Err 完成回合数、耗时与终结错误（nil = 正常结束）
```

单一 `Config` 入口（根包门面）：可选 `AgentName` 自定义系统提示词中的 agent 身份（默认 `"cobot"`）；`SystemPromptOverride` 非空时**完全替换**默认系统提示词（宿主自带完整角色提示词，如 Maestro 的中文角色提示词），`AVAILABLE TOOLS` 工具说明段落仍由 loop 依据 Registry 渲染，`CustomRules`/`AgentName` 在覆盖模式下被忽略。

`Run` / `RunWithCallback` 返回 `(*RunResult, error)`：`err == nil` 即本次 run 正常结束（含 `Abort()`）；`ctx` 取消会中断流式读取与工具执行并返回 `errors.Is(err, context.Canceled)` 为真的包装错误；宿主的 `ask_followup_question` 回调返回错误时，错误文本作为工具结果回传给模型、run 继续由模型自行决策。

## 设计要点（v0.1）

- `github.com/liup215/cobot`，Go 1.25，运行时依赖 = 0（仅 stdlib）
- 包结构：根包门面（`cobot.NewAgent(Config)`）/ `types` / `provider`（openai 兼容 + **anthropic** + mock）/ `tools`（六件套 + ToolGuard + XML 兜底）/ `loop` / `compact`（Trim + Summarize + Combo）/ `examples`
- 继承 gline（Cline）：`attempt_completion` 终结协议、Plan/Act、纠错环、预派发、reasoning 通道
- 新增（借鉴 pi）：Anthropic 流式适配器、LLM 摘要式 compaction、PreToolUse 审批门、自动重试
- 明确不做：TUI、extensions、会话树、OAuth、MCP、编排

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
| `read_file` | 整文件读上限 64KB（`MaxReadFileBytes`）、`start_line/end_line` 区间读上限 100KB；超出返回头部 + 截断通知（含真实字节数/行数与指路），不再直接拒绝 |
| `write_to_file` | 原子写：同目录临时文件 + `rename`，保留原文件 mode；写失败不破坏原文件。自动创建目录 |
| `replace_in_file` | 所有块先在内存中应用，任一 `search` 失配 → 整文件不动并点名失败块（all-or-nothing）；结果同样原子写回 |
| `execute_command` | 前台阻塞执行，默认超时 120s（`timeout` 可按调用覆盖）；超时杀整个进程树（Windows `taskkill /F /T` + `WaitDelay` 兜底）；stdout/stderr 分离采集，各截断 32KB（保头 3/4 + 尾 1/4 + 标记）；shell 钉死：Windows `cmd /C`，其余 `sh -c`。非 0 退出码是结果不是错误。不用于长驻服务 |
| `search_files` | 结果确定性排序（path → line → column，稳定排序）；`TotalMatches` 报告真实命中数；结果超过 500 条时截断并附 `[Results truncated: …]` 通知 |
| `list_files` | 目录条目按文件名排序（`os.ReadDir` 语义，已钉死）；超过 500 条截断并附通知 |
| `list_code_definition_names` | 按文件路径排序、文件内按行号稳定排序（输出可复现）；超过 500 条截断并附通知 |
| `attempt_completion` | 终结工具：必填 `result`，可选 `command`（建议用户复核结果的命令） |
| `ask_followup_question` | 澄清问题，不终结运行；`SetHandler` 注入交互端（TUI/宿主），未注入时回退 stdin；支持按选项序号作答 |
| `plan_mode_respond` | Plan 模式终结工具：响应原样回显给用户 |

文件/命令类工具**不做工作目录沙箱**（gline 基线语义）：路径可以指向工作目录之外，
执行控制请通过 `Config.Guard`（`ToolGuard`）实现。

## 开发流程

项目文档与进度全部在 [`memory-bank/`](memory-bank/) 维护（六文件体系）。里程碑 M0-M5 见 `memory-bank/progress.md`。

发布：M5 完成并经确认后 `gh repo create liup215/cobot --public --source . --push` 并打 `v0.1.0` tag。

## License

MIT
