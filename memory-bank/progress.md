# Progress: cobot

## What Works

- [x] 三路线调研完成（pi RPC 子进程 / pi Go 移植 / gline 提取），选定 gline 提取（2026-08-28）
- [x] 需求规格定稿：memory-bank/requirements.md（v0.1，含手术清单 §10 与里程碑 §7）
- [x] Memory Bank 初始化（projectbrief/productContext/activeContext/systemPatterns/techContext/progress）
- [x] 仓库推送：github.com/liup215/cobot（public，main，origin 已配置；gh 账号 liup215）
- [x] go mod init（go 1.25）+ MIT LICENSE + .gitignore/.gitattributes
- [x] 提示词↔工具集守卫测试（2026-08-28）：loop/prompt_guard_test.go 断言
      系统提示词中出现的每个工具名都在默认 Registry 中（双模式）、AVAILABLE
      TOOLS 段仅宣传当前模式允许的工具；prompts/system_test.go 钉死静态工具
      目录（GetToolDescriptions 及 plan/act 变体）与 Registry 三集合一致。
      起因：CAPABILITIES 段宣传 list_code_definition_names 但未注册。
- [x] 模块改名 agentcore → cobot（commit 726cdae）+ 改名清扫（README/
      包注释/错误前缀/系统提示词身份 `Config.AgentName`，2026-08-28）
- [x] 清扫收尾：误入库的 `.maestro-mcp.json`（会话级 MCP 配置）出库并
      加入 .gitignore；仓库不再跟踪任何会话本地文件

## What's Left to Build

### M0 仓库初始化 — 完成
- [x] git init（main）+ 首次提交 + GitHub 推送
- [x] go mod init github.com/liup215/cobot（go 1.25）+ LICENSE (MIT)

### M1 骨架迁移（零第三方依赖）— 完成
- [x] types/ 收编 gline pkg/types（剔除 render/slash，Estimator 注入）
- [x] provider/ 接口层 + openai 兼容（SSE + tool_calls 增量 + reasoning_content）
- [x] provider/ mock（场景化回放）+ registry
- [x] tools/ Tool/Registry + 六件套 + attempt_completion/ask_followup/plan_mode_respond
- [x] tools/ XML 兜底解析（签名去 provider 依赖）
- [x] 验收：`go list -m all` 无第三方；-race 全绿

### M2 loop 重构 — 完成
- [x] agent.go 1173 行拆分：loop/agent.go(582)/stream.go/toolexec.go +
      guard.go + store.go + log.go（Step A，commit 43b31b2）
- [x] 行为基线测试 12 个：终结协议/纠错环/预派发/guard 三态/plan 模式/
      XML 兜底/流错误/abort（-race -count=2 绿）
- [x] ToolGuard（主循环 + 预派发两挂点）+ ErrAbortRun + fail-closed
- [x] Store 接口（nil 安全）
- [x] 行为修复×2：a.abort→atomic.Bool（race）；consecutiveMistakes
      清零时机（FR-L2 纠错环死循环）
- [x] 自动重试：loop/retry.go createStreamWithRetry（指数退避 1/2/4/8s +
      Retry-After 优先；Options.RetryDelays nil=默认/空=禁用）；openai.go
      同步建连 + provider/errors.go HTTPError/parseRetryAfter
      （Step B，commit 654a4db）
- [x] Callback 补齐：OnToolCallUpdate / OnCompactionStart / OnCompactionEnd /
      OnRetry（Adapter no-op；trim 与 60% 双挂点；同步执行分支才发 update）
- [x] 验收：18 loop 测试 + httptest SSE 错误矩阵，`-race -count=2` 全绿

### M3 Anthropic 适配器 — 完成
- [x] provider/anthropic.go（传输+流式）+ provider/anthropic_wire.go（wire 类型与
      双向映射；拆两文件守住 ~400 行规模约束）
- [x] SSE 全事件解析：message_start / content_block_start /
      content_block_delta（text_delta/thinking_delta/signature_delta/
      input_json_delta）/ content_block_stop / message_delta / message_stop /
      ping / error；按 payload type 派发（event: 行兜底，代理剥离 event 行仍可解析）
- [x] tool_use 增量 JSON 按 index 拼接，content_block_stop 时解析为 ToolCall
      （复用 isValidJSON/isCompleteJSONObject 括号修复；零参工具取
      content_block_start 内置 input；EOF 截断 best-effort 收尾）
- [x] thinking 块 → ReasoningContent 独立通道（请求侧不回传，见决策表）
- [x] system 顶层块 + ephemeral cache_control（打在最后一个 system 块）；
      tools → input_schema；tool_choice auto/any；tool_result 归组进 user 消息
- [x] stop_reason 映射 end_turn/stop_sequence→stop、tool_use→tool_calls、
      max_tokens→length、未知透传；tool_use 时工具调用保证完整可用（有测试）
- [x] 错误处理：同步建连非 200 → provider.HTTPError（含 Retry-After、
      overloaded_error/529 可重试）；流中 error 事件 → 流错误通道走纠错环
- [x] registry 注册 "anthropic"（ANTHROPIC_API_KEY 环境变量兜底）
- [x] 验收：29 个 Anthropic 测试（httptest 假 SSE 逐事件断言 18 +
      请求形状/映射 11），anthropic.go/wire.go 函数覆盖率 95-100%，
      `go test ./... -race` 全绿

### M4 compaction 与工具集
- [x] 工具集定稿之一：list_code_definition_names **保留并注册**进默认 Registry
      （plan/act 双模式，CategoryCode；类型枚举 ToolListCodeDefinitionNames 同步；
      新增工具单测 + 提示词↔Registry 守卫测试，见 loop/prompt_guard_test.go）
- [ ] compact.Strategy 接口 + Trim（移植）+ Summarize（新写，K=6 轮）
- [ ] 默认组合：Summarize 失败回退 Trim
- [ ] 工具行为定稿（截断/超时/大文件策略）

### M5 文档与发布
- [ ] examples：basic / streaming / custom-tool / guard（均可离线 go run）
- [ ] README 中英 + godoc 清理
- [ ] Maestro embedded 后端接入（runner 新后端与 claude/pi 并列）
- [ ] v0.1.0 tag + `gh release create`

## Current Status

**M2 完成（loop 三段拆分 + ToolGuard + 自动重试 + 全部 Callback 事件，
**M2 完成（loop 三段拆分 + ToolGuard + 自动重试 + 全部 Callback 事件，
18 个行为测试 `-race -count=2` 绿；提交 43b31b2 + 654a4db）+ 根包门面
（5f6e6ca）+ agentcore→cobot 改名清扫（726cdae 及后续清扫提交）完成。**
§10 手术单清退完成：prompts 僵尸工具目录/Skills 段、types/skill_meta.go、
tools ToolBehavior 系与 init.go 旁路 API 全部删除（净 -272 行）。
**M3 完成（原生 Anthropic Messages 适配器：SSE 全事件 + thinking 独立通道 +
cache_control + stop_reason 映射 + HTTPError/Retry-After；29 个测试
`-race` 绿，provider 覆盖率 95-100%，零第三方依赖）。**
下一步 M4：compaction（Trim 移植 + Summarize 新写）与工具行为定稿。

## Known Issues / 风险

- gline loop 为单人验证强度 → 18 个行为基线测试 + Maestro 自举狗粮弥补
- Anthropic 适配器无真实 API 单测 → httptest 逐事件断言 + M5 集成联调
- 预派发双执行（no-Delay 场景）为 gline 固有 best-effort 语义 → 测试用
  Delay 30ms 钉时序，未改循环
- 流建连失败每轮消耗 mistake 配额（基线语义）→ 有重试时通常轮内恢复，
  无重试 3 轮终止；已在测试中固化

## Evolution of Decisions

| 日期 | 决策 | 依据 |
|---|---|---|
| 2026-08-28 | 选定"gline 提取"而非 pi 移植/RPC 子进程 | 代码自有、Go 原生、1.5-2.5 周 vs 1-2 月；Cline 哲学契合 headless Worker |
| 2026-08-28 | 零第三方依赖硬约束（运行时+测试） | Maestro 纯 Go 单二进制目标；go.mod 卫生 |
| 2026-08-28 | 只新增四项能力：Anthropic 适配器 / ToolGuard / Summarize compaction / 自动重试 | pi 调研结论：headless 场景真正缺的只有这些 |
| 2026-08-28 | 明确不做：TUI/extensions/会话树/OAuth/MCP/记忆/浏览器/编排/Skills | v0.2+ 候选，避免范围蔓延 |
| 2026-08-28 | 迁移流程定为 cp+sed 字节级复制，禁止 write 重写迁移源 | write 重写反复引入不可见字符；cp+sed 零风险 |
| 2026-08-28 | provider 双 ToolCall 并存（string / RawMessage），loop 归口转换 | 与 gline 语义一致，M2 拆分时统一 |
| 2026-08-28 | xml_fallback 签名去 provider 依赖 | 避免 tools→provider 反向依赖 |
| 2026-08-28 | a.abort 改 atomic.Bool（修 gline race）；mistakes 清零时机改"工具成功后"（修 FR-L2 死循环） | 内存安全 + 需求 FR-L2；均有测试钉住，零其他语义变化 |
| 2026-08-28 | 重试只覆盖同步建连失败；流中错误走纠错环 | Step B 设计定稿；流中重试会造成重复副作用 |
| 2026-08-28 | OnToolCallUpdate 仅同步执行分支发 | 预派发复用时工具已完成，发 running 会误导 |
| 2026-08-28 | list_code_definition_names 定稿：注册进默认 Registry（plan/act、CategoryCode、无需确认）而非删除提示词 | 工具已完整实现（含 30s 列表缓存）、只读、可安全预派发；提示词 CAPABILITIES 段本就推荐该工作流（environment_details → 定义概览 → read_file → 编辑） |
| 2026-08-28 | 提示词与工具集的一致性用测试钉死（prose 提及 ⊆ Registry；AVAILABLE TOOLS ⊆ 当前模式；静态目录 = Registry 三集合） | 提示词是自由文本，无结构约束；只有测试能防再次漂移 |
| 2026-08-28 | Anthropic 适配器拆 anthropic.go（传输/流式）+ anthropic_wire.go（wire 类型/映射） | 单文件 ~700 行超出 NFR-3 规模约束；任务点名 anthropic.go 仍保留 |
| 2026-08-28 | thinking（ReasoningContent）请求侧不回传 Anthropic | Messages API 要求 thinking 块与其加密签名成对回传且仅在开启 thinking 时合法，内部 Message 类型不携带签名；丢弃是唯一安全行为（v0.2 候选：持久化签名） |
| 2026-08-28 | ephemeral cache_control 打在最后一个 system 块 | Anthropic 缓存"以 marker 结尾的最长前缀"；SystemPrompt 与历史 system 消息合并进顶层 system 数组（messages 内禁止 system 角色） |
| 2026-08-28 | SSE 按 payload 的 type 字段派发，event: 行兜底 | 代理/网关可能剥离 event 行；data 无 type 时才回退 |
| 2026-08-28 | SSE 解析复用 openai.go 的 isValidJSON/isCompleteJSONObject 括号修复与 partial-chunk 语义 | 两个 provider 行为一致，loop 无需区分 |
| 2026-08-28 | 模块/仓库改名 agentcore → cobot（go.mod、import、README、包注释、错误前缀全量清扫），系统提示词 agent 身份提为 `Config.AgentName`（默认 "cobot"） | 项目更名与 GitHub 仓库一致；硬编码模型身份不利于复用方品牌化 |
