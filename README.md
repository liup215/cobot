# cobot

> 纯 Go、零第三方依赖的 Agent SDK —— 从 gline（Cline 血统）提取重构，供 Maestro 等项目进程内驱动 LLM agent。

**状态**：M0–M2 + 根包门面 + 嵌入式宿主适配面（结构化 `RunResult`、ctx 取消语义、headless ask_followup 行为、`SystemPromptOverride`）已完成（`go test -race ./...` 全绿）；M3（Anthropic 适配器）进行中。规格唯一来源：[memory-bank/requirements.md](memory-bank/requirements.md)。

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
- 包结构：根包门面（`cobot.NewAgent(Config)`）/ `types` / `provider`（openai 兼容 + **anthropic** + mock）/ `tools`（六件套 + ToolGuard + XML 兜底）/ `loop` / `compact`（Trim + Summarize）/ `examples`
- 继承 gline（Cline）：`attempt_completion` 终结协议、Plan/Act、纠错环、预派发、reasoning 通道
- 新增（借鉴 pi）：Anthropic 流式适配器、LLM 摘要式 compaction、PreToolUse 审批门、自动重试
- 明确不做：TUI、extensions、会话树、OAuth、MCP、编排

## 开发流程

项目文档与进度全部在 [`memory-bank/`](memory-bank/) 维护（六文件体系）。里程碑 M0-M5 见 `memory-bank/progress.md`。

发布：M5 完成并经确认后 `gh repo create liup215/cobot --public --source . --push` 并打 `v0.1.0` tag。

## License

MIT
