# Active Context: agentcore

## 当前工作焦点

**M3 原生 Anthropic Messages 适配器完成**：provider/anthropic.go（传输+SSE
流式）+ provider/anthropic_wire.go（wire 类型与双向映射），registry 注册
"anthropic"，29 个 httptest 逐事件断言测试 `-race` 绿，覆盖率 95-100%。
下一步 **M4：compaction（Trim 移植 + Summarize 新写）与工具行为定稿**。

**已知残留（有意保留，超出本次点名范围且删除会改行为）**：
loop/stream.go:173 预派发副作用白名单 case 里列有 "kb_ingest"/"browser_copy"
（剔除工具名）。该白名单是安全网——删除会使同名自定义工具变为可预派发
（竞态风险），故保留。


## 近期变更

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
- 2026-08-28：**根包门面完成（commit 5f6e6ca）**：
  - agentcore.go（根包）：`Config`（平铺全可选：APIKey/BaseURL/Model 派生
    openai provider；Tools nil=默认九件套；Mode/AutoApprove/MaxTokens/
    RetryDelays/CustomRules；Guard/Store/Title 挂钩）+ `NewAgent(cfg)`。
  - aliases.go：类型别名全量再导出（Agent/Callback/CallbackAdapter/
    ToolGuard/GuardedCall/Store/Provider/ToolRegistry/Tool、ModeAct/
    ModePlan、ErrAbortRun、DefaultRetryDelays）——使用方只 import 根包
    即可实现全部钩子接口。
  - agentcore_test.go：门面端到端（scripted provider → attempt_completion
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

1. **M4 compaction 与工具集**：
   - compact.Strategy 接口 + Trim（移植 gline）+ Summarize（新写，K=6 轮）。
   - 默认组合：Summarize 失败回退 Trim。
   - 工具行为定稿（截断/超时/大文件策略；list_code_definition_names 去留）。
2. **M5**：examples + README 中英 + Maestro embedded 验证 → tag v0.1.0。

## 活跃决策

- **模块路径** `github.com/liup215/agentcore`，仓库目录名 agent-core（本地）。
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
