# Progress: agentcore

## What Works

- [x] 三路线调研完成（pi RPC 子进程 / pi Go 移植 / gline 提取），选定 gline 提取（2026-08-28）
- [x] 需求规格定稿：memory-bank/requirements.md（v0.1，含手术清单 §10 与里程碑 §7）
- [x] Memory Bank 初始化（projectbrief/productContext/activeContext/systemPatterns/techContext/progress）
- [x] 仓库推送：github.com/liup215/agentcore（public，main，origin 已配置；gh 账号 liup215）

## What's Left to Build

### M0 仓库初始化
- [x] git init（main）+ .gitignore + .gitattributes(LF) + 首次提交
- [x] GitHub 仓库创建并推送（liup215/agentcore，public）
- [ ] go mod init github.com/liup215/agentcore（go 1.25）
- [ ] LICENSE (MIT)

### M1 骨架迁移（零第三方依赖）
- [x] types/ 收编 gline pkg/types（剔除 render/slash，加 Estimator 注入）
- [x] provider/ 接口层（Provider/StreamChunk/Request/Response，源自 internal/agent/provider.go）
- [x] provider/ openai 兼容（移植，SSE + tool_calls 增量聚合 + reasoning_content 透传）
- [x] provider/ mock（场景化回放）+ registry（工厂注册表，ProviderConfig 复用接口层定义）
- [x] provider/ 验收：`go list -m all` 无第三方；6 单测 -race 全绿；无不可见字符
- [ ] tools/ Tool/Registry + 六件套 + attempt_completion(+ask_followup)
- [ ] tools/ XML 兜底解析（移植 + 测试）
- [ ] 验收：gline 相关单测移植通过

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
- [ ] v0.1.0 tag + `gh release create`（仓库已存在，仅剩打标发布）

## Current Status

**M1 进行中（types + provider 完成，剩 tools/）**。provider 迁移验证了
cp+sed 复制流程可行且干净，tools 迁移照此执行。

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
| 2026-08-28 | 仓库提前推送（原计划 M5 后） | 用户指令「直接先推送到 github」；v0.1.0 tag/release 仍留到 M5 |
| 2026-08-28 | 迁移流程定为 cp+sed 字节级复制，禁止 write 重写 | write 重写 provider 文件反复引入不可见字符；cp+sed 零风险且语义零改动 |
| 2026-08-28 | provider 双 ToolCall 并存（provider.ToolCall string 版 / types.ToolCall RawMessage 版） | 与 gline 语义一致，M2 loop 拆分时再统一归口 |
