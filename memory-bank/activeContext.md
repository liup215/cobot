# Active Context: cobot

## 当前工作焦点

**M4 审查修复复验通过（2026-08-30，第 2 次续跑会话）**：
- 修复已在 commit f2f8ec3f84fb 落地（工作树干净），本次未重做，只做复验：
  - `go test ./... -race -count=2` 7 包全绿；`go build ./...`、`go vet ./...`、
    `gofmt -l` 干净；`go list -m all` 仅本模块（零第三方依赖）。
  - 变异验证两条（临时改动后立即恢复，仓库无残留）：
    1) 还原 summarize.go 的 `firstUser < cut` 守卫 →
       TestSummarizeRestoredToolFirstHistoryDoesNotDuplicateTask 红，
       复现出审查所述"任务消息出现两次"；
    2) 注释 loop/agent.go 60% 挂点的 runCompaction 调用 →
       TestOptionsCompactionRunsInjectedStrategyAtBudgetHook 红
       （"Compact called 0 times"）。
- 会话残留清理：`.git-commit-msg.txt` 是 Maestro 会话的提交信息临时文件，
  被上次会话随修复提交一并 `git add` 进了仓库（dc88baf 中不存在），
  已 `git rm --cached` 并加入 .gitignore（与 .maestro-mcp.json 同类），
  磁盘上的文件保留，不影响提交流程。

**M4 审查修复完成（2026-08-30）**：
- compact/summarize.go Compact() 的任务消息回插守卫收紧为
  `firstUser >= 0 && firstUser < cut`：历史不以 user 消息开头（预发送硬修剪
  删掉首轮，或 Store 恢复的 assistant/tool 开头会话）时，首条用户消息落在
  保留窗口起点（firstUser == cut），旧代码会把它既留在 body[cut:] 又插到
  摘要前——任务消息出现两次。实测复现过，修后由 4 个回归测试钉住
  （assistant 开头边界 / tool_call 开头的 Store 恢复 / firstUser<cut 分支 /
  Trim 同场景 parity）。
- loop/compaction_test.go（新）：钉住 60% 预算挂点（enforceTokenBudget）
  会把注入的 Options.Compaction 跑在活会话上；预算按 system prompt +
  工具描述开销精确取值，隔离出 60% 挂点（避开 80% AutoCompact 与 100%
  预发送硬修剪）。变异验证：注释掉 runCompaction 调用 → 测试红。
- tools/utf8.go（新）truncateUTF8Prefix/Suffix：execute_command 32KB 头尾
  切割与 read_file 64KB 整文件 / 100KB 行区间三处改 rune 边界安全（切割点
  回退/前进至多 3 字节），截断通知改报实际展示字节数；中文/emoji 不再在
  截断边缘变 U+FFFD（tools/utf8_test.go 全覆盖）。
- 验收：`go test ./... -race -count=2` 全绿；`go list -m all` 仅本模块；
  gofmt/vet 干净。

**M4 完成（compaction 双策略 + 工具行为定稿）**：
- compact 包新建：`Strategy` 接口 + `Trim`（gline 水位修剪移植，60/80 水位、
  保 system/首条用户消息/最近 6 轮）+ `Summarize`（K=6、五段结构化摘要、
  120s 超时、空摘要/无 summarizer/超时均报错）+ `Combo`（Summarize 失败或
  未把会话压到回退水位以下即回退 Trim；双失败合并错误，loop 记日志继续跑）。
- loop 接线：`Options.Compaction/Summarizer/SummarizePrompt`，默认
  `Combo(Summarize(ProviderSummarizer), Trim)`；60% 预算触发点与 80% 挂点改走
  策略，预发送 100% 仍是 `TrimToMaxTokens` 硬修剪，`OnCompactionStart/End`
  配对不变；`Compact()` 手动触发改走策略并返回"是否真的压缩了"。
- 根包门面：`Config.Compaction/Summarizer/SummarizePrompt` + aliases.go 再导出。
- 工具行为定稿（tests 钉死，见 tools/behavior_test.go）：execute_command
  120s 默认超时 + 进程树击杀（修 Windows 29s 挂起）+ stdout/stderr 分离 +
  32KB 截断 + shellCommand() 钉死；read_file 64KB/100KB 头截断 + 通知
  （修 4000 token 死阈值）；write/replace 原子写（writeFileAtomic，失败不破坏
  原文件）；search_files 稳定三键排序 + 真实 TotalMatches + 截断通知；
  list_files/definitions 上限与确定性输出；交互三件套行为钉死。
- 验收：`go test ./... -race` 全绿、`go list -m all` 仅本模块。
下一步 **M5：examples（basic/streaming/custom-tool/guard）+ README 中英 +
godoc 清理 + Maestro embedded 后端接入 → tag v0.1.0**。

**已知残留（有意保留，超出本次点名范围且删除会改行为）**：
loop/stream.go:173 预派发副作用白名单 case 里列有 "kb_ingest"/"browser_copy"
（剔除工具名）。该白名单是安全网——删除会使同名自定义工具变为可预派发
（竞态风险），故保留。


## 近期变更

- 2026-08-30：**M4 审查修复**：Summarize 任务消息重复（`firstUser < cut`
  守卫 + compact_test 4 个回归）、loop 60% 挂点接线测试
  （loop/compaction_test.go，含变异验证）、工具字节截断 rune 边界安全
  （tools/utf8.go + utf8_test.go，通知改报实际字节数）。
- 2026-08-29：**M4 compaction 双策略完成**：
  - compact/compact.go（新）：`Strategy` 接口（ShouldCompact + Compact；就地
    重写、可重复调用、错误=“本策略未生效”供 Combo 回退）、`Round`/
    `SplitRounds`（轮=用户消息+其响应；assistant tool_call 与 tool 结果永不分家；
    前置 system 不属于任何轮）、默认值常量（KeepRounds=6、60/80 水位、
    SummarizeTimeout=120s、MaxSummaryTokens=2000、MaxTranscriptTokens=24000）、
    哨兵错误 ErrNoSummarizer/ErrEmptySummary。
  - compact/trim.go（新）：水位修剪移植；丢弃循环用"拼装后消息列表的精确 token
    投影（含 notice）"决定丢几轮（早期"只减不增"算法被回插 notice 吃掉释放
    预算，会反复触发）；仍超 80% 回落 `Conversation.TrimToMaxTokens`。
  - compact/summarize.go（新）：转录渲染（user/assistant/tool result 标注、
    tool_call→tool 名映射、单条 2000 字符上限、newest-first 预算适配）、
    五段默认提示词（可覆盖）、120s 超时、摘要/转录双上限。
  - compact/combo.go（新）：主策略失败或未把会话压到回退水位以下 → 回退；
    双失败返回合并错误。
  - loop/summarizer.go（新）：`ProviderSummarizer`（单次非流式 CreateMessage，
    MaxTokens 1024），复用会话 provider，不走流重试。
  - loop/agent.go：Options/BaseAgent 增 compaction；`runCompaction`（nil 策略
    保留 legacy `AutoCompact()`）；`Compact()` 返回是否真的压缩。
  - types/conversation.go：新增 `ReplaceMessages`（就地换历史 + 重估 token +
    清 actual tokens）与导出 `MessageTokens`。
  - compact/compact_test.go（22 个）：SplitRounds 边界、Trim 移植回归
    （保 system/任务/最近 K 轮、水位以下不动、幂等、硬修剪回落、配置项）、
    Summarize（替换/转录内容/提示词覆盖/失败矩阵/上限）、Combo（回退/跳过/
    双失败合并/ShouldCompact OR 语义/默认装配）。
- 2026-08-29：**M4 工具行为定稿完成**（tools/behavior_test.go 24 个测试钉死）：
  - execute_command：默认超时 60s→120s；`configureProcessKill`（Windows
    taskkill /F /T 进程树 + cmd.WaitDelay=3s）修掉"超时后 Run 仍阻塞到
    ping 自然结束（实测 29s）"；stdout/stderr 分离采集（去掉
    `"\nexit status "` hack）；`truncateCommandOutput` 每流 32KB 保头 3/4 +
    尾 1/4；shell 选择抽成 `shellCommand(goos, cmd)` 并钉死。
  - read_file：大文件从"拒绝"改"头截断 + 通知"（64KB 整文件 / 100KB 区间）；
    删除 estimateTokensFromBytes(len/2) 阈值（其 4000 token 阈值 ≈ 8KB，
    使 100KB 阈值成死代码）。
  - write_to_file / replace_in_file：`writeFileAtomic`（同目录临时文件 +
    rename + 保留原 mode + fsync）；replace 保持 all-or-nothing。
  - search_files：`sort.SliceStable` + (path,line,column) 三键；
    `Truncated` 字段 + 真实 TotalMatches + `[Results truncated:` 通知。
  - list_files：`listFilesUpTo` 500 条上限 + 通知；os.ReadDir 排序语义写入
    godoc。list_code_definition_names：文件路径排序 + 行内稳定排序 +
    500 条上限通知（修 map 遍历顺序泄漏）。
  - 交互三件套：attempt_completion 必填 result / plan_mode_respond 回显 /
    ask_followup handler + 选项序号映射，全部有测试。
- 2026-08-28：**M3 Anthropic 适配器完成**：

- 2026-08-28：**M3 Anthropic 适配器完成**：
  - provider/anthropic.go（~680 行）：NewAnthropicProvider（key 空则读
    ANTHROPIC_API_KEY，默认模型 claude-sonnet-4-5，默认 baseURL
    api.anthropic.com）、buildAnthropicURL（host//v1//v1/messages 三形态）、
    post（x-api-key + anthropic-version: 2023-06-01）、checkAnthropicStatus
    （错误信封 + Retry-After → HTTPError）、CreateMessage /
    CreateMessageStream（同步建连非 200 → HTTPError，之后 goroutine 跑
    pumpStream）、pumpStream（SSE 解析：空闲 120s 定时器 + 每读 30s 截止，
    blocks map[index]*state 累积，message_stop/EOF 时统一 finish 单个
    Done chunk）、finalizeUnclosedToolBlocks、convertAnthropicResponse、
    mapAnthropicStopReason / mapAnthropicToolChoice。
  - provider/anthropic_wire.go（~265 行）：请求/响应/SSE wire 类型 +
    convertMessagesToAnthropic（system 归顶层、tool_result 归组 user 消息、
    空消息剔除与占位）+ normalizeAnthropicToolInput。
  - provider/registry.go：InitDefaultRegistry 注册 "anthropic"。
  - 测试：anthropic_stream_test.go（18 个：纯文本/双工具/增量拼接/thinking/
    零参/错误事件/stop_reason 矩阵/429+529+400/EOF/截断修复/未知事件/
    畸形 SSE 容错/缺 id 跳过/不可修复 JSON 跳过/块起始自带 input/
    error 无 payload/连接中断 hijack）+ anthropic_test.go（11 个：默认值/
    缺 key/URL/请求形状逐字段/system cache_control/默认 schema/
    非流式映射/非流式 429/网络错 Status 0/registry/映射边界）。
- 2026-08-28：**list_code_definition_names 漂移修复 + 提示词↔Registry 守卫**：
  - 问题：prompts/system.go CAPABILITIES 段宣传 `list_code_definition_names`，
    但 `InitDefaultRegistry` 未注册——模型一旦调用必报 "tool not found"。
    该工具（tools/search.go:548，含 30s 列表缓存）实现完整却无人注册、无测试。
  - 定稿（M4 待决项）：**方案①注册**——tools/init.go 注册（plan/act 双模式、
    CategoryCode、无需确认）；types.ToolName 补 `ToolListCodeDefinitionNames`；
    `GetDefaultTools`/`GetToolsForMode` 同步；只读属性天然兼容流预派发。
  - 顺带定稿 prompts 静态目录：GetToolDescriptions 删掉 3 个从未实现、且在
    requirements §4.3/§10 剔除清单里的幻影工具（use_subagents/web_fetch/
    browser_copy），补上漏掉的 plan_mode_respond；模式过滤改为包级
    actOnlyTools/planOnlyTools 映射（与 Registry AllowedModes 镜像）。
  - 守卫测试（新增）：
    - loop/prompt_guard_test.go：①提示词中每个 snake_case 工具名形状 token
      必须是 Registry 工具或 `nonToolIdentifiers` 白名单（schema 属性名/
      提示词标记，如 file_pattern、environment_details）；②Registry 每个模式
      工具都必须出现在提示词中；③AVAILABLE TOOLS 段只宣传当前模式允许的
      工具。已验证撤掉注册时测试立刻红（能复现原始 bug）。
    - prompts/system_test.go：GetToolDescriptions = Registry 全集，
      GetActMode/GetPlanModeToolDescriptions = Registry 对应模式子集。
  - 新增工具单测 tools/search_test.go TestListCodeDefinitionNames（Go/Python
    定义识别、file_pattern 过滤、缓存命中、缺路径、path 必填）。
  - 验收：`go build/vet/gofmt` 干净，`go list -m all` 仅本模块，
    `go test -race -count=2 ./...` 全绿。

- 2026-08-28：**§10 遗留 API 清退**：prompts 僵尸工具目录四函数 + Skills
  段与参数、types/skill_meta.go、tools ToolBehavior 系 + init.go 旁路 API
  全部删除；loop/agent.go 调用点同步（去 nil 实参）。净 -272 行，
  build/vet/test -race 全绿，零依赖不变。
- 2026-08-28：**清扫收尾：会话本地配置出库**——commit dc67232 误将
  `.maestro-mcp.json`（Maestro 会话级 MCP 配置，含本机 URL 与
  role/task_id，每次会话都会漂移）提交进仓库。已 `git rm --cached`
  出库（磁盘保留供本会话连接用）并加入 .gitignore；`git ls-files`
  已无任何会话本地文件。

- 2026-08-28：**agentcore → cobot 改名清扫**（commit 726cdae 改名 + 本次
  清扫提交）：
  - README：标题、模块路径 `github.com/liup215/cobot`、状态行
    （M0–M2+根包门面完成，M3 进行中）、快速上手示例。
  - 根包 cobot.go：包注释/示例改 cobot；错误前缀 `"agentcore:"` →
    `"cobot:"`；新增 `Config.AgentName`（空 = "cobot"）。
  - prompts/system.go：`GetSystemPrompt` 新增 agentName 参数（空 =
    `prompts.DefaultAgentName`），角色行不再写死；loop `Options.AgentName`
    透传；根包再导出 `cobot.DefaultAgentName`。
  - 新增测试：prompts 身份（默认/自定义 + 遗留名泄漏回归）、loop 与门面
    的 AgentName 透传；`go test -race -count=2 ./...` 全绿。
  - types/tools 文档注释清扫；memory-bank 全部旧模块引用更新。

- 2026-08-28：**根包门面完成（commit 5f6e6ca）**：
  - cobot.go（根包）：`Config`（平铺全可选：APIKey/BaseURL/Model 派生
    openai provider；Tools nil=默认九件套；Mode/AutoApprove/MaxTokens/
    RetryDelays/CustomRules；Guard/Store/Title 挂钩）+ `NewAgent(cfg)`。
  - aliases.go：类型别名全量再导出（Agent/Callback/CallbackAdapter/
    ToolGuard/GuardedCall/Store/Provider/ToolRegistry/Tool、ModeAct/
    ModePlan、ErrAbortRun、DefaultRetryDelays）——使用方只 import 根包
    即可实现全部钩子接口。
  - cobot_test.go：门面端到端（scripted provider → attempt_completion
    → 事件回调）、默认注册表、凭据校验、自定义注册表注入。
  - 动机：用户反馈 loop.New 组装语义不清晰、要单一 config 参数；
    评估三方案（根包门面 / loop 改名 agent / 单包合并）后选定门面。

- 2026-08-28：**M2 Step B 完成（commit 654a4db）**：
  - provider/errors.go（新）：`HTTPError{Status, Message, RetryAfter, Err}` +
    `Retryable()`（0 网络错/408/429/5xx 可重试，其余 4xx 不可）+
    `parseRetryAfter`（秒制，上限 5min）+ AsHTTPError/IsRetryable。
  - openai.go 手术：**连接建立移出 goroutine**（message 转换 → marshal →
    httpReq → client.Do → 状态码检查全部同步），网络失败与非 2xx 直接
    `return nil, &HTTPError{...}`（错误文案不变）；goroutine 仅剩 SSE 读取。
  - loop/retry.go（新）：`createStreamWithRetry` 指数退避重试（默认
    1s/2s/4s/8s，Retry-After 更长时优先），Options.RetryDelays 可配
    （nil=默认、空切片=禁用）；每轮 `OnRetry(attempt, err, nextDelay)`。
  - Callback 新事件：`OnToolCallUpdate`（同步执行前发 "running"）、
    `OnCompactionStart/End`（主循环 trim 挂点 IsTokenAboveThreshold(100)
    + enforceTokenBudget 60% 挂点，后者新增 callback 参数）、`OnRetry`。
  - 测试：flakyProvider（前 N 次建流失败后委托 ScriptedProvider）+
    httptest 假 SSE（成功流 + 429/503/400 同步错误矩阵）+ compaction 事件。
- 2026-08-28：**M2 Step A 完成（commit 43b31b2）**：loop 包三段拆分、
  ToolGuard、Store、12 个行为基线测试；修复 gline 两个固有 bug
  （见下「行为修复记录」）。
- 2026-08-28：M1 完成（types/provider/tools，commits 477b224、da55106）。

**重要经验**：
- 会话级配置（`.maestro-mcp.json` 等 worktree 注入文件）绝不入库：
  提交前必须扫一眼 `git status`/`git add -A` 暂存清单，确认没有混入
  非任务文件；这类文件一律 .gitignore。
- 从 gline 迁移文件一律用 `cp` + 字节级 `sed`（包名、import、前缀），
  禁止 write 重写迁移源文件——不可见字符问题反复出现。
- **bash heredoc 追加大段 Go 代码不可靠**（长内容会被静默截断），新测试
  追加用 `cat >>` 后必须立即检查尾部完整性；或者干脆用 write（测试文件
  不受禁令限制）。
- edit 工具偶尔把 edits 数组误序列化为字符串（报 "must be object"），
  重试即可；跨文件多处修改必须拆成每文件一个 edit 调用。

## 行为修复记录（相对 gline 基线，均有测试钉住）

1. **`a.abort` 数据竞争**：`abort bool` → `atomic.Bool`（gline 中 defer
   置 false 与预派发 goroutine 读取构成 race）。纯内存安全修复，零语义变化。
2. **纠错环死循环（FR-L2）**：gline 在流创建后把 `consecutiveMistakes`
   清零，导致"有工具不用"的纠正环永不终止。新规则：mistakes 仅在工具
   成功执行后清零；不调工具的回复计入 3 次上限，达限返回
   `max consecutive mistakes reached: model did not use a tool in %d consecutive turns`。
3. **流建连失败消耗 mistake 配额（基线语义澄清）**：无重试时每轮主循环
   建流失败计 1 次 mistake，3 轮达限终止；重试层只减少轮数，不改变该语义。

## 下一步（按里程碑顺序）

1. **M5 文档与发布**：
   - examples：basic / streaming / custom-tool / guard（均可离线 go run）。
   - README 中英 + godoc 清理（README 已含 compaction 与工具行为表，可作底稿）。
   - Maestro embedded 后端接入（runner 新后端与 claude/pi 并列）→ tag v0.1.0。

## 活跃决策

- **模块路径** `github.com/liup215/cobot`（2026-08-28 由 agentcore 改名，
  commit 726cdae），仓库目录名 cobot（本地与 GitHub 同名）。
- **零依赖硬约束**：go.mod require 仅 stdlib；日志用 log/slog shim。
- **提取基线冻结**：gline @ 2026-06-27；行为变更仅限上述三项修复 +
  requirements.md 的四项新增（Anthropic/ToolGuard/Summarize/重试）。
- **重试语义**：只重试同步建连失败（HTTPError.Retryable），永不重试
  流中错误；Options.RetryDelays 空 = 关闭。
- **OnToolCallUpdate** 只在同步执行分支发（预派发复用路径不发，
  避免"已跑完还显示 running"的误导）。
- **工具集定稿（M4 部分）**：list_code_definition_names 保留并注册（默认
  Registry 共 10 个工具）；提示词提及的每个工具必须有对应注册项，由
  loop/prompt_guard_test.go 强制。
- **compaction 默认 = Combo(Summarize(ProviderSummarizer), Trim)**（M4 定稿）：
  压缩单元是"轮"（保 tool_call↔tool 结果成对）；Trim/Summarize 都保护
  system + 首条用户消息 + 最近 6 轮；通知/摘要用 RoleUser（Anthropic 消息
  列表禁止 system 角色，连续 user 轮由 API 合并）；无待压缩轮不花 LLM 调用；
  紧急路径（预发送 100%、80% 挂点的硬修剪）永不调 LLM。
- **工具行为（M4 定稿）**：execute_command 120s + 进程树击杀 + 32KB/流截断 +
  cmd /C 与 sh -c 钉死；read_file 64KB/100KB 头截断 + 通知；write/replace
  原子写（writeFileAtomic）；search/list/definitions 确定性排序 + 500 条上限 +
  截断通知；路径不做 workdir 沙箱（ToolGuard 是执行点，沙箱 v0.2 候选）。
- **Anthropic thinking 不回传**：Messages API 要求 thinking 块与其加密
  签名成对出现且仅在请求开启 thinking 时合法，内部 types.Message 不携带
  签名 → convertMessagesToAnthropic 丢弃 ReasoningContent（v0.2 候选：
  持久化签名）；响应侧 thinking → ReasoningContent 独立通道。
- **Anthropic system 归顶层**：历史 RoleSystem 消息并入顶层 system 数组
  （messages 内禁止 system 角色），ephemeral cache_control 打在最后一个
  system 块（缓存以 marker 结尾的最长前缀）。
- **Anthropic SSE 按 payload type 派发**：data 无 type 才回退 event: 行；
  畸形/未知事件一律忽略不毒化流。
- 仓库推送已提前完成；tag/release 留到 M5。

## 经验与洞察（来自调研）

- gline 的预派发（SSE 流期间并行跑只读工具）与三锁竞态防护是亮点，
  已原样保留并有 `-race` 回归测试（TestPreDispatchExecutesOnce）。
- Anthropic 适配器难点在 SSE 事件序与 stop_reason 映射，
  用 httptest 假 SSE 逐事件断言（openai_stream_test.go 已示范结构）。
- Guard 测试必须放行 attempt_completion，否则 deny 环造成 O(n²)
  token 估算假死（AutoCompact 本身无 bug）。
