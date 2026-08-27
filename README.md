# agentcore

> 纯 Go、零第三方依赖的 Agent SDK —— 从 gline（Cline 血统）提取重构，供 Maestro 等项目进程内驱动 LLM agent。

**状态**：需求已冻结，M0 开发中。规格唯一来源：[memory-bank/requirements.md](memory-bank/requirements.md)。

## 设计要点（v0.1）

- `github.com/liup215/agentcore`，Go 1.25，运行时依赖 = 0（仅 stdlib）
- 包结构：`types` / `provider`（openai 兼容 + **anthropic** + mock）/ `tools`（六件套 + ToolGuard + XML 兜底）/ `loop` / `compact`（Trim + Summarize）/ `examples`
- 继承 gline（Cline）：`attempt_completion` 终结协议、Plan/Act、纠错环、预派发、reasoning 通道
- 新增（借鉴 pi）：Anthropic 流式适配器、LLM 摘要式 compaction、PreToolUse 审批门、自动重试
- 明确不做：TUI、extensions、会话树、OAuth、MCP、编排

## 开发流程

项目文档与进度全部在 [`memory-bank/`](memory-bank/) 维护（六文件体系）。里程碑 M0-M5 见 `memory-bank/progress.md`。

发布：M5 完成并经确认后 `gh repo create liup215/agentcore --public --source . --push` 并打 `v0.1.0` tag。

## License

MIT
