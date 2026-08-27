# Project Brief: cobot

## 一句话定位

cobot 是一个**纯 Go、零第三方运行时依赖**的 Agent SDK——从 gline
（Cline 血统）的 `internal/agent` 内核提取重构而来，作为独立 GitHub 包发布，
供 Maestro 等项目以进程内方式驱动 LLM agent 完成任务。

## 为什么存在

Maestro（个人 Agent OS，Go daemon）目前靠子进程调用 Claude Code CLI 执行
任务，存在 Node.js/claude CLI 外部依赖，无法成为纯 Go 单二进制。调研
（2026-08-28，详见 memory-bank/requirements.md §1.1）评估了 pi RPC 子进程、
pi Go 移植、gline 提取三条路线后选定 **gline 提取**：代码自有（MIT）、
Go 原生、约 1.5-2.5 周，且 Cline 的"领任务→自主执行→attempt_completion
交付"哲学与 Maestro Worker 场景天然匹配。

## 核心需求（详细规格见 memory-bank/requirements.md）

1. 从 gline 提取 agent 内核为独立 module `github.com/liup215/cobot`。
2. 运行时依赖为 0（仅 stdlib），测试依赖也为 0。
3. 新增原生 Anthropic Messages 适配器（流式 SSE + thinking + cache_control）。
4. 新增 ToolGuard 审批门（PreToolUse 拦截）。
5. 新增 LLM 摘要式 compaction（与 gline 水位线裁剪并存）。
6. 新增流式请求自动重试（指数退避 + Retry-After）。
7. 发布 v0.1.0（gh-cli），Maestro 以 embedded 后端接入。

## 范围

**In**：types / provider（openai 兼容 + anthropic + mock）/ tools（六件套
内置工具 + ToolGuard + XML 兜底）/ loop（终结协议、纠错环、预派发、
回调事件）/ compact（Trim + Summarize）/ examples。

**Out（v0.1 明确不做）**：TUI、extensions、会话树、OAuth 订阅认证、
MCP client/server、记忆引擎、浏览器自动化、subagent 编排、Skills。

## 成功标准

1. `go list -m all` 无任何第三方依赖；三平台 `go test -race ./...` 绿。
2. Maestro `embedded` runner 跑通一次真实 demand 执行。
3. v0.1.0 发布到 github.com/liup215/cobot。

## 关键约束

- 提取基线：gline @ 2026-06-27（commit fix-replace-in-file-errors）快照。
- Go 1.25；无 CGO；Windows 本机为主要开发平台。
- 手术清单（继承/新写/剔除）见 requirements.md §10，开发时严格照办。
