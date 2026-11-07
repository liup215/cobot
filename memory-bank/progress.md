# Progress: cobot

## What Works

- [x] ask_followup_question 多选协议 allow_multiple（2026-11-07，c0277ac 已推送）：schema/input 加布尔字段、mapOptionAnswer 单选契约不变+多选逐项映射、provider.MultiAskCallback 可选接口（loop 类型断言 uniform 路由，未实现者降级）、adapter 多选 CLI 兜底、toolexec 从原始 input 解析塞闭包、prompt-guard nonToolIdentifiers 补 allow_multiple；2 组新测试（映射+真实 run 路由），9 包+vet+gofmt 绿
- [x] read 看图 P1（2026-11-07，b5cc3fe 已推送）：`classifyImage` 内容嗅探三分类——视觉栅格发 `[image: path]` 标记（宿主转视觉部件）、非视觉格式元数据（新措辞）、SVG/文本优先走文本路径；>10MB 超限前置拦截；描述更新；read_image_test 6 组，9 包测试+vet+gofmt 绿
- [x] 解析层抽出 + read 合并文档提取（2026-11-07 二轮）：新公共包 `docparse/`（自 ltedu knowledge 迁入，KB 导入改调它）；`read` 经 docparse 抽文本后行分页（text extracted 头标注 / ErrNoTextLayer 友好提示 / 64MB 提取上限）；依赖政策修订（纯 Go 第三方 gopdf/striprtf/word-extractor/xls，零依赖表述作废——README/requirements/techContext 已改）；9 包测试绿
- [x] 工具面与 gline 对齐（2026-11-07）：新 `shell/` 包（bash-first 解析 + 共享 Run 执行器 + 审批启发式）；
      重命名 read/write/edit/ls/grep/run + 新增 glob（fd 快路径）与 grep 的 rg 快路径；run 带
      `SetApprovalHandler` 审批钩子（fail-closed）；合并时保留 cobot 独有加固（原子写/500 条截断/
      真实 TotalMatches+截断通知/确定性 formatCodeDefinitions）；list_code_definition_names 保留实现
      不注册（gline 修剪）+ prompt 重写；8 包 `go test` 全绿（prompt-guard 双向）+ `-race` 绿
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

### M4 compaction 与工具集 — 完成
- [x] 工具集定稿之一：list_code_definition_names **保留并注册**进默认 Registry
      （plan/act 双模式，CategoryCode；类型枚举 ToolListCodeDefinitionNames 同步；
      新增工具单测 + 提示词↔Registry 守卫测试，见 loop/prompt_guard_test.go）
- [x] compact.Strategy 接口（ShouldCompact + Compact，就地重写、可重复调用、
      错误=“本策略未生效”）+ compact.Round/SplitRounds（轮 = 一条用户消息及其
      响应；assistant tool_call 与 tool 结果永不分家；前置 system 不属于任何轮）
- [x] compact.Trim（gline 水位修剪移植）：60/80 水位（默认）、保留 system + 首条
      用户消息（原始任务）+ 最近 KeepRounds（默认 6）轮；从最老轮开始丢弃、含
      notice 预算的精确投影决定丢几轮；仍超 80% 时回落 gline 硬修剪
      Conversation.TrimToMaxTokens（保留 system + 至少 2 条消息）
- [x] compact.Summarize（新写）：K=6 轮内保留原文，更早轮渲染成转录
      （user/assistant/tool result 标注、单条 2000 字符上限、总预算
      DefaultMaxTranscriptTokens=24000）交给 Summarizer（默认
      loop.ProviderSummarizer 单次非流式调用，MaxTokens 1024），替换为一条
      RoleUser 结构化摘要（五段：Task goal/Completed steps/Key decisions/
      Open questions/Files + `[COMPACTED]` 头）；提示词可被 Config.SummarizePrompt
      覆盖；无 summarizer（ErrNoSummarizer）/provider 错误/空摘要
      （ErrEmptySummary）/超时（默认 120s）/无待压缩轮（不花 LLM 调用）均为错误
- [x] compact.Combo：主策略（Summarize）失败或未把会话压到回退水位以下时回退
      Trim；双失败返回合并错误（loop 记日志继续跑，硬修剪兜底）
- [x] loop 接线：Options.Compaction/Summarizer/SummarizePrompt；默认 =
      Combo(Summarize(ProviderSummarizer), Trim)；`Compact()` 手动触发改走策略；
      enforceTokenBudget 60% 触发点与 80% 挂点改走策略，
      OnCompactionStart/OnCompactionEnd 事件配对不变（预发送 100% 仍是
      TrimToMaxTokens 硬修剪，无 LLM）
- [x] 根包门面：Config.Compaction/Summarizer/SummarizePrompt + aliases.go 再导出
      （Strategy/Summarizer/Trim/Summarize/Combo/DefaultKeepRounds/
      DefaultSummarizePrompt/NewProviderSummarizer）
- [x] 工具行为定稿（全部有测试钉住，见 tools/behavior_test.go）：
      execute_command 默认超时 120s（DefaultCommandTimeoutSeconds，可按调用
      覆盖）且真的杀进程树（Windows taskkill /F /T + WaitDelay=3s 兜底，修复
      cmd.exe 不转发 kill 导致的 29s 挂起）；stdout/stderr 分离采集；
      各自 32KB 截断（保头 3/4 + 尾 1/4 + 截断标记）；shell 选择抽成
      shellCommand() 并钉死（windows=cmd /C，其余=sh -c）；前台阻塞语义写入
      描述（禁止用于长驻服务）
      read_file 大文件策略从“拒绝”改为“头截断 + 通知”：整文件 64KB
      （MaxReadFileBytes）、行区间 100KB（MaxReadRangeBytes），通知报告真实
      字节数/行数并指路 search_files/start_line；修掉原 4000 token 阈值使
      100KB 阈值成死代码的问题（len/2 估算法下有效阈值只有 ~8KB）
      write_to_file / replace_in_file：writeFileAtomic（同目录临时文件 +
      rename，保留原文件 mode，新文件 0644）——写失败不破坏原文件；
      replace_in_file 全部块先在内存中应用，任一块失配则整文件不动并点名失败块
      search_files：sort.SliceStable + (path,line,column) 三键排序（输出确定）；
      TotalMatches 记录真实命中数，结果截断到 500 并附
      `[Results truncated: …]` 通知
      list_files：os.ReadDir 天然按文件名排序（写入 godoc）+ 500 条上限
      （MaxListEntries）与截断通知
      list_code_definition_names：按文件路径排序 + 文件内按行号稳定排序（消除
      map 遍历顺序泄漏），500 条上限与截断通知
      交互三件套行为钉死：attempt_completion 必填 result（command 可选）、
      plan_mode_respond 原样回显、ask_followup_question 走 handler（可注入）
      并支持“按选项序号作答”
- [x] 验收：`go test ./... -race` 全绿（7 包）；`go list -m all` 仅本模块
      （零第三方依赖）；compact 22 个单测（Trim 移植回归 / Summarize 假
      summarizer / Combo 回退 / 失败矩阵 / 截断上限）、tools 行为测试 24 个、
      loop 既有 compaction 事件测试继续绿

### M4 审查修复（M4 review fix）— 完成
- [x] 阻塞缺陷修复：compact.Summarize 任务消息重复。历史不以 user 消息开头时
      （预发送硬修剪删掉首轮，或从 Store 恢复的 assistant/tool 开头会话），
      首条用户消息落在保留窗口起点（firstUser == cut），Compact() 曾把它
      既留在 body[cut:] 又再插到摘要前——实测复现：
      [system, assistant, user(task), assistant] + KeepRounds=1 →
      [system, user(task), [COMPACTED]…, user(task), assistant]。
      守卫收紧为 `firstUser >= 0 && firstUser < cut`（与构造 dropped 的
      循环同边界，compact/summarize.go）
- [x] Trim.dropOldestRounds 同边界确认正确（candidate 循环 `i < cut` 天然
      满足），并补同场景回归测试钉住 parity
- [x] 回归测试（compact/compact_test.go）：assistant 开头历史（firstUser==cut
      边界，测试内自校验）、从 Store 恢复的 tool_call 开头历史（工具轮整体进
      转录，永不分家）、firstUser < cut 分支（任务消息应原样保留且仅一次）；
      全部断言任务消息只出现一次
- [x] loop/compaction_test.go（新）：注入自定义 Strategy，钉住 60% 预算挂点
      （enforceTokenBudget）会把 Options.Compaction 跑在活会话上（预算按
      system prompt + 工具描述开销精确取值，隔离出 60% 挂点、避开 80%/100%
      两个挂点）；变异验证过（注释掉 runCompaction 调用测试即红）
- [x] 建议项落地：字节截断改 rune 边界安全（tools/utf8.go 新增
      truncateUTF8Prefix/truncateUTF8Suffix，切割点回退/前进到 rune 边界，
      至多让出 3 字节）：execute_command 32KB 头尾切割、read_file 64KB 整文件
      与 100KB 行区间三处，截断通知改报“实际展示字节数”（不再写死阈值）；
      中文/emoji 输出不再在截断边缘产生 U+FFFD（utf8_test.go 全覆盖）
- [x] 验收：`go test ./... -race -count=2` 全绿（7 包）；`go list -m all`
      仅本模块；gofmt/vet 干净
- [x] 复验（2026-08-30 第 2 次续跑会话，commit f2f8ec3f84fb 已落地、未重做）：
      两条变异验证复跑均红——还原 `firstUser < cut` 守卫 →
      TestSummarizeRestoredToolFirstHistoryDoesNotDuplicateTask 复现双任务
      消息；注释 60% 挂点 runCompaction → loop 接线测试
      "Compact called 0 times"。改动均已恢复，仓库无残留。附带清理：
      `.git-commit-msg.txt`（Maestro 会话提交信息临时文件）被上次会话误
      提交进仓库，已 `git rm --cached` + .gitignore（与 .maestro-mcp.json
      同类会话本地文件，磁盘文件保留）

### M5 文档与发布
- [x] examples：basic / streaming / custom-tool / guard（均可离线 go run，scripted
      provider 回放 LLM 轮次，无需 API key；examples/basic 打印 RunResult 交付物、
      examples/streaming 演示 RunWithCallback + StreamCallbackAdapter 的
      OnContent/OnToolCall 事件流、examples/custom-tool 演示
      InitDefaultRegistry().Register 注入宿主工具（即 Maestro 内置工具注入方式）、
      examples/guard 演示 ToolGuard 三态 deny/ErrAbortRun/放行）
- [x] README 中英双语（README.md 中文完整版 + README_EN.md 英文版：徽章与 v0.1.0
      对齐、快速开始、Config 全字段表 19 项、嵌入指南——宿主注册工具/接管系统
      提示词/读取 RunResult/其他挂钩、M4 工具行为定稿表对齐并补 rune 边界截断与
      headless ask_followup 语义）+ godoc 清理（补 MockScenario/HTTPError.Error/
      ScriptedProvider 访问器/StreamCallbackAdapter 组注释；全库无 TODO 残留）
- [ ] Maestro embedded 后端接入（runner 新后端与 claude/pi 并列）——不在 M5 发布
      收尾任务范围内，留待独立任务
- [x] v0.1.0 release（2026-09-08 发布）：tag 4230e32 解引用指向 4eb4858（M0–M3
      完成点，未移动）。release notes 定稿入库 docs/releases/v0.1.0.md（M0–M3
      特性清单 + 已知限制，指路 v0.1.1/v0.2.0 承载 main 内容），README「版本与
      发布」段已同步声明。`gh release create v0.1.0 --title "cobot v0.1.0 —
      M0–M3 foundation release" --notes-file docs/releases/v0.1.0.md` 成功，
      https://github.com/liup215/cobot/releases/tag/v0.1.0 存在且指向 4eb4858

## Current Status

**M5 发布收尾完成（2026-09-08）：examples 四件套（basic/streaming/custom-tool/
guard，scripted provider 离线 go run 全通过）+ README 中英双语（Config 全字段表 +
嵌入指南 + M4 工具行为表对齐）+ godoc 清理 + v0.1.0 release 已全部完成并提交；
release 已发布（https://github.com/liup215/cobot/releases/tag/v0.1.0，tag 4230e32
指向 4eb4858 未移动，notes = M0–M3 特性 + 已知限制）。`go build/vet/gofmt` 干净，
`go test ./...` 全绿。Maestro embedded 后端接入不在本任务范围，未勾选。**
**M4 审查修复完成（2026-08-30）：compact.Summarize 任务消息重复（历史不以
user 开头时首条用户消息出现两次）已修（`firstUser < cut` 守卫 + 回归测试）；
loop 60% 挂点对注入 Strategy 的接线补了测试（变异验证有效）；execute_command
与 read_file 三处字节截断改 rune 边界安全（中文输出不再产生 U+FFFD）。
`go test ./... -race -count=2` 全绿，`go list -m all` 零第三方依赖。**
**M4 完成（compaction 双策略 + 工具行为定稿）：`compact.Strategy` 接口 +
Trim（gline 水位修剪移植，60/80 水位、保 system/首条用户消息/最近 6 轮）+
Summarize（K=6 轮原文保留、五段结构化摘要、120s 超时、空摘要/无 summarizer/
provider 错误均报错）+ Combo（Summarize 失败或未达标回退 Trim）；loop 默认
装配 ProviderSummarizer，60%/80% 挂点走策略、事件配对不变；Config 全透传。
工具侧：execute_command 120s 默认超时 + 进程树击杀 + stdout/stderr 分离 +
32KB 截断；read_file 64KB/100KB 头截断 + 通知（修死阈值）；write/replace
原子写（失败不破坏原文件）；search/list/definitions 确定性排序 + 真实计数 +
截断通知。`go test ./... -race` 全绿，`go list -m all` 零第三方依赖。**
§10 手术单清退完成：prompts 僵尸工具目录/Skills 段、types/skill_meta.go、
tools ToolBehavior 系与 init.go 旁路 API 全部删除（净 -272 行）。
**M3 完成（原生 Anthropic Messages 适配器：SSE 全事件 + thinking 独立通道 +
cache_control + stop_reason 映射 + HTTPError/Retry-After；29 个测试
`-race` 绿，provider 覆盖率 95-100%，零第三方依赖）。**
**嵌入式宿主适配面完成（Maestro 集成前置，2026-08-29）**：
结构化 `RunResult`（`Run`/`RunWithCallback` 返回 `(*RunResult, error)`，
FinalText = attempt_completion `result` / plan_mode_respond `response`，
无终结工具时 best-effort 取本次 run 最后一条 assistant 文本；token/turns/
duration 按 run 归口累计）、ctx 取消语义钉死（中断流读取与工具执行——
含预派发 goroutine，`errors.Is(err, context.Canceled)` 成立，
`preDispatchWG.Wait()` 保证无 agent 自有 goroutine 跨 run 存活，
`Abort()` 语义不变）、headless ask_followup 行为（回调错误文本作为工具
结果回传、run 继续由模型自治）、`SystemPromptOverride` 宿主接管（完全
替换默认提示词，AVAILABLE TOOLS 仍由 loop 渲染；守卫测试 override 模式
跳过静态目录断言）。新增 22 个行为测试，`go test -race -count=2 ./...`
6 包全绿，依赖仍为 0。
下一步 M4：compaction（Trim 移植 + Summarize 新写）与工具行为定稿。（已于 fe0e1cd4 完成并合并）
**审查修复完成（2026-08-29 第 2 次执行）**：①flaky Duration 断言删除
（Windows 时钟粒度，改确定性回拨单测）；②mid-run compaction 空
FinalText/跨 run 泄漏改为 **turn 记录时捕获**（runStats.finalText/
lastAssistantText，删除 extractFinalText 与 startMsgCount 索引法）；
③同回合多 tool_call 取消/守卫中止时为未执行的调用落占位工具结果
（recordSkippedToolResults），转录本跨 run 保持良构。红→绿均以仓库外
探针验证；`-count=10 ./loop/` 绿、`-race -count=2 ./...` 6 包绿、依赖 0。
下一步 M5：examples（basic/streaming/custom-tool/guard）+ README 中英 +
godoc 清理 + Maestro embedded 后端接入。

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
| 2026-08-29 | **RunResult API 形态二选一：定稿为 `Run`/`RunWithCallback` 返回 `(*RunResult, error)`**，而非新增 `RunResult()` 查询方法 | 结果与产生它的 run 绑定（agent/conversation 可跨 run 复用），查询方法有 stale 状态与"哪一次 run"的歧义，宿主还得记 run 边界；v0.x 阶段允许破坏性签名变更，godoc 已写明语义 |
| 2026-08-29 | RunResult 用量按 run 归口累计（runStats 从 run 起始消息数/时间起算），会话级 `GetActualTokens` 保持"最后回合"语义不变 | Conversation.AddActualTokens 是替换式（只存最近回合）；聚合视图属 run，不属会话；不改动既有会话语义 |
| 2026-08-29 | ctx 取消 = 硬中断：processStream select ctx.Done（弃流先 drain，防 provider pump 卡死）、工具执行（含预派发 goroutine）以 ctx 收尾，`preDispatchWG.Wait()` 在 run defer 中等待 | 宿主需要可靠的打断原语；`errors.Is(err, context.Canceled)` 必须成立；取消不得消耗纠错环 mistake 配额（4 个检查点先查取消）；代价是忽略 ctx 的工具会推迟返回（godoc 已注明） |
| 2026-08-29 | `Abort()` 保持协作式检查点语义不变（不强制打断进行中的流读取）；打断靠 ctx，两者并存时取消优先 | 任务要求 Abort 行为不变；TestAbortDoesNotUnblockStreamReadButContextDoes 钉住该契约并写进 Abort godoc |
| 2026-08-29 | headless ask_followup：回调返回错误 → 错误文本作为该 tool_call 的工具结果回传，run 继续由模型自行决策（转 attempt_completion 或按假设继续） | Maestro 无人类可答；整 run 作废/panic/死锁都不可接受，模型自治最稳；每次错误都逐次回传（TestAskFollowupErrorRoundTripsEachTime） |
| 2026-08-29 | `SystemPromptOverride` 非空时**完全替换**默认系统提示词（CustomRules/AgentName 被忽略），但 AVAILABLE TOOLS 段仍由 loop 依 Registry 渲染 | Maestro 自带完整中文角色提示词，需要替换而非累加；不渲染工具段模型就看不到任何工具 schema，只能由宿主手工重述每个 schema |
| 2026-08-29 | 提示词↔Registry 守卫在 override 模式跳过静态目录断言，只校验"工具段存在 + ⊆ 当前模式 Registry + 无默认段落泄漏"；守卫构造 prompt 改走 `BaseAgent.systemPromptFor` 单一代码路径 | 宿主文本可合法提及宿主侧原生工具（submit_plan/update_step/report_progress 等），静态断言会误伤；单一代码路径防守卫与实现漂移 |
| 2026-08-29 | **FinalText 改为 turn 记录时捕获**（runStats.finalText/lastAssistantText，stream.go append assistant 消息时 recordAssistantTurn），删除「按 run 起始消息数索引转录本」的 extractFinalText | mid-run compaction 重写转录本后 run 起始消息数失效（本 run 消息落到索引之前），索引法报空 FinalText 甚至取到上一 run 的交付物；捕获式对 compaction 天然免疫，且"重试后的终结调用覆盖前者"语义更清晰 |
| 2026-08-29 | 测试禁止对 `RunResult.Duration > 0` 做行为断言（Windows 时钟粒度 ~0.5-15ms，子毫秒 run 报 0）；需要验证时长时回拨 `run.startTime` | `-count=10 ./loop/` 实测 3/10 概率挂；非确定性断言违反"新增行为测试 -race 绿"验收 |
| 2026-08-29 | 取消/守卫中止时对同回合未执行的 tool_call 逐个落占位工具结果（[NOT EXECUTED] 文案）再退出，保证转录本跨 run 良构 | OpenAI 兼容端点要求 assistant tool_calls 后每条 tool_call_id 都有 tool 消息，否则下一次请求 400；占位结果同时让回调收到完整 OnToolCallComplete 序列 |
| 2026-08-29 | compact 以“轮”为压缩单元（一条用户消息 + 其全部响应，含 tool_call↔tool 结果成对），而非逐条消息 | OpenAI 与 Anthropic 两种 wire 格式都要求 tool_use 与 tool_result 相邻；逐条丢弃会产生悬空 tool 结果 |
| 2026-08-29 | Trim/Summarize 都保护：前置 system 消息 + 首条用户消息（原始任务）+ 最近 KeepRounds(=6) 轮 | 任务描述与 gline 基线的交集；Maestro 长任务依赖最近工作状态与原始目标 |
| 2026-08-29 | Trim 的丢弃循环用“拼装后消息列表的精确 token 投影（含 notice）”判定停点 | 早期实现只减不增，插回的 notice (~180 token) 会吃掉释放的预算，导致每轮 60% 触发点都再丢一轮（churn）；精确投影一次到位 |
| 2026-08-29 | compaction 通知/摘要消息用 RoleUser（`[Context compacted:` / `[COMPACTED]` 前缀），不用 RoleSystem | Anthropic Messages API 消息列表禁止 system 角色（会被提升进顶层 system）；连续 user 轮由 API 自动合并，安全 |
| 2026-08-29 | Summarize.ShouldCompact 在“无待压缩轮”时返回 false，Compact 同样直接返回 | 小预算场景（如 loop 测试 MaxTokens=200）不浪费 ScriptedProvider 轮次，也保证“无 LLM 调用则无副作用” |
| 2026-08-29 | 默认组合 = Combo(Summarize(ProviderSummarizer), Trim)，失败/未达回退水位即回退；双失败返回合并错误 | 任务要求“任何失败回退 Trim”；loop 把压缩错误记日志继续跑（压缩是优化，不是失败理由），预发送硬修剪兜底 |
| 2026-08-29 | loop 三挂点分工：预发送 100% = TrimToMaxTokens 硬修剪（无 LLM）；80% AutoCompact 与 60% enforceTokenBudget = 配置的 Strategy | 紧急路径不能承受 LLM 调用的延迟与失败；主循环语义不变，事件配对保持 |
| 2026-08-29 | loop.ProviderSummarizer 用单次非流式 CreateMessage（MaxTokens 1024），不走路重试机制 | 摘要失败应由 Combo 回退到 Trim 处理；流式/重试机制属于主循环 |
| 2026-08-29 | execute_command 默认超时 60s → 120s（DefaultCommandTimeoutSeconds），并修复超时杀不掉子进程的问题（Windows taskkill /F /T 进程树 + cmd.WaitDelay=3s） | Maestro Worker 场景跑构建/测试 60s 偏短；cmd.exe 不转发 kill 导致 Run 阻塞到 ping 自然结束（实测 29s） |
| 2026-08-29 | execute_command 改为分离采集 stdout/stderr（原来是 CombinedOutput 塞进 Stdout） | 输出结构体本就声明两个字段；分离后不再需要 `"\nexit status "` 字符串 hack，模型可区分正常输出与报错 |
| 2026-08-29 | 命令输出截断 = 每流 32KB，保头 3/4 + 尾 1/4 + `[... output truncated ...]` | 构建错误在尾部、启动横幅在头部；定长上限防止单条工具结果炸上下文 |
| 2026-08-29 | shell 选择抽成 shellCommand(goos, cmd) 并用测试钉死（windows=cmd /C，其余=sh -c） | 任务要求“确认现状并钉死”；函数化后平台变更必须显式改测试 |
| 2026-08-29 | read_file 大文件从“拒绝”改为“头截断 + 通知”（64KB 整文件 / 100KB 行区间） | 拒绝迫使模型多一轮往返；头部（imports/类型/首个定义）已够定位，通知给出真实字节数与指路 |
| 2026-08-29 | 删除 estimateTokensFromBytes(len/2) 阈值 | 该估算法下 4000 token 阈值 ≈ 8KB，使 100KB 字节阈值成为死代码，读任何 >8KB 的源文件都被拒 |
| 2026-08-29 | write_to_file / replace_in_file 统一走 writeFileAtomic（同目录临时文件 + rename，保留原 mode） | 写失败（磁盘满/权限）不再破坏原文件；rename 同目录保证原子性；读方永远看不到半文件 |
| 2026-08-29 | replace_in_file 保持“全部块先在内存应用、任一失配整文件不动” | 任务要求 all-or-nothing 回滚语义；与原子写配合天然满足 |
| 2026-08-29 | 路径不做 workdir 沙箱（维持现状），把 ToolGuard 写成执行点 | gline/Cline 基线语义；沙箱是 v0.2 候选，host 现在就能用 Guard 拦截 |
| 2026-08-29 | search_files 排序改 sort.SliceStable + (path,line,column) 三键；TotalMatches 记录真实总数（截断只裁 Results） | 并发搜索顺序不定，两键排序同位置结果不稳定；真实计数 + `[Results truncated:` 通知防止模型误以为已看全 |
| 2026-08-29 | list_code_definition_names 输出按文件路径排序、文件内按行号稳定排序 | 原实现按 map 遍历顺序输出，同样输入每次结果不同，无法用于 diff 类推理 |
| 2026-08-29 | list_files 依赖 os.ReadDir 的文件名排序并写入 godoc，加 500 条上限 + 通知 | 上限防 node_modules 式目录炸上下文；截断必须可见 |
| 2026-08-29 | 交互三件套行为用测试钉死（attempt_completion 必填 result；plan_mode_respond 原样回显；ask_followup 走可注入 handler 且支持按选项序号作答） | 任务要求“行为定稿并写入 godoc/README”；未定稿的隐式行为无法被复用方依赖 |
| 2026-08-30 | Summarize 的任务消息回插守卫收紧为 `firstUser >= 0 && firstUser < cut`（与构造 dropped 集合的循环同边界） | 历史不以 user 开头时首条用户消息落在保留窗口起点，旧守卫会把它既留在 body[cut:] 又插到摘要前（任务消息出现两次）；Trim 的对应循环本就按该边界处理，补测试钉住 parity |
| 2026-08-30 | 字节截断改 rune 边界安全：tools/utf8.go 的 truncateUTF8Prefix/Suffix 供 execute_command（32KB 头尾）与 read_file（64KB 整文件 / 100KB 行区间）三处使用；截断通知改报实际展示字节数 | 普通字节切割会在 64KB/100KB/32KB 边界把中文/emoji 劈成 U+FFFD 污染模型上下文；回退切割点至多让出 3 字节，字节上限语义保持 |
| 2026-08-30 | `.git-commit-msg.txt`（Maestro 会话的提交信息临时文件）加入 .gitignore 并解除跟踪 | 上次会话把它随修复提交一并 add 进了仓库（dc88baf 中不存在）；不处理则每次提交都会带来脏 diff |
| 2026-09-07 | v0.1.0 tag 固定在 4eb4858 不移动；release notes 按 tag 实际内容（M0–M3）书写并显式声明 M4/M5 交付物在 main、随 v0.1.1/v0.2.0 发布 | 引用方对 tag 能力预期必须与 go get 拿到的代码一致（评审意见②）；当时点 Run 尚返回 error、无 compact 包、无 examples |
