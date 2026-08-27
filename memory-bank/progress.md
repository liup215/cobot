# Progress: agentcore

## What Works

- [x] 三路线调研完成（pi RPC 子进程 / pi Go 移植 / gline 提取），选定 gline 提取（2026-08-28）
- [x] 需求规格定稿：memory-bank/requirements.md（v0.1，含手术清单 §10 与里程碑 §7）
- [x] Memory Bank 初始化（projectbrief/productContext/activeContext/systemPatterns/techContext/progress）
- [x] gh 已确认登录（liup215）

## What's Left to Build

### M0 仓库初始化
- [ ] go mod init github.com/liup215/agentcore（go 1.25）
- [ ] LICENSE (MIT)、.gitignore、README 骨架
- [ ] 首次 git commit

### M1 骨架迁移（零第三方依赖）
- [ ] types/ 收编 gline pkg/types（剔除 render/slash，加 Estimator 注入）
- [ ] provider/ 接口层 + openai 兼容（移植）+ mock（增强脚本化回放）
- [ ] tools/ Tool/Registry + 六件套 + attempt_completion(+ask_followup)
- [ ] tools/ XML 兜底解析（移植 + 测试）
- [ ] 验收：`go list -m all` 无第三方；gline 相关单测移植通过

### M2 loop 重构
- [ ] agent.go 1173 行拆分：主循环 / 流处理 / 工具执行（先固化行为基线测试）
- [ ] ToolGuard（主循环 + 预派发两挂点）+ ErrAbortRun
- [ ] 自动重试（指数退避 + Retry-After + 4xx 不重试）
- [ ] Callback 补 OnToolCallUpdate / OnCompaction* / OnRetry
- [ ] Store 接口（nil 安全）
- [ ] 验收：终结协议/纠错环/预派发/guard 单测 + `-race` 绿

### M3 Anthropic 适配器
- [ ] SSE 流式（message_*/content_block_* 全事件）
- [ ] thinking 块 → ReasoningContent 通道
- [ ] cache_control（system ephemeral）+ stop_reason 映射
- [ ] Retry-After / overloaded_error 处理
- [ ] 验收：httptest 假 SSE 全分支测试

### M4 compaction 与工具集
- [ ] compact.Strategy 接口 + Trim（移植）+ Summarize（新写，K=6 轮）
- [ ] 默认组合：Summarize 失败回退 Trim
- [ ] 工具行为定稿（截断/超时/大文件策略）

### M5 文档与发布
- [ ] examples：basic / streaming / custom-tool / guard（均可离线 go run）
- [ ] README 中英 + godoc 清理
- [ ] Maestro embedded 后端接入（runner 新后端与 claude/pi 并列）
- [ ] 经用户确认：gh repo create + push + v0.1.0 release

## Current Status

**预开发阶段（M0 未开始）**。需求与设计已冻结，可随时开工 M0。

## Known Issues / 风险

- gline loop 为单人验证强度 → 靠移植全量测试 + Maestro 自举狗粮弥补
- 拆分 1173 行单体有回归风险 → 先用 mock provider 固化行为基线再动手
- Anthropic 适配器无真实 API 单测 → httptest 逐事件断言 + M5 集成联调补真实验证
- 预派发竞态 → 三锁结构原样保留 + `-race` 回归；副作用工具默认不预派发

## Evolution of Decisions

| 日期 | 决策 | 依据 |
|---|---|---|
| 2026-08-28 | 选定"gline 提取"而非 pi 移植/RPC 子进程 | 代码自有、Go 原生、1.5-2.5 周 vs 1-2 月；Cline 哲学契合 headless Worker |
| 2026-08-28 | 零第三方依赖硬约束（运行时+测试） | Maestro 纯 Go 单二进制目标；go.mod 卫生 |
| 2026-08-28 | 只新增四项能力：Anthropic 适配器 / ToolGuard / Summarize compaction / 自动重试 | pi 调研结论：headless 场景真正缺的只有这些 |
| 2026-08-28 | 明确不做：TUI/extensions/会话树/OAuth/MCP/记忆/浏览器/编排/Skills | v0.2+ 候选，避免范围蔓延 |
| 2026-08-28 | 需求文档纳入 memory bank 体系 | 用户指定的工作流 |
