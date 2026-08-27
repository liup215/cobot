# Active Context: cobot

## 当前工作焦点

**改名清扫完成（agentcore → cobot）**：commit 726cdae 改 go.mod 与 import
路径，本次清扫补齐残留（README/包注释/错误前缀/系统提示词身份），系统
提示词 agent 身份提为可配置 `Config.AgentName`（默认 "cobot"）。
根包门面已落地（commit 5f6e6ca）：现公开入口 = `cobot.NewAgent(Config)`，
子包架构不变（门面纯语法糖，`Agent = loop.BaseAgent` 别名零包装）。
M2 全部完成；下一步 **M3：Anthropic 适配器**。

## 近期变更

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

1. **M3 Anthropic 适配器**（provider/anthropic.go，新写）：
   - 参照 pi 的 anthropic-messages.ts 移植 SSE 事件序
     （message_start/content_block_start/delta/stop/message_delta/message_stop，
     嵌套 tool_use 增量 JSON 拼接）；thinking 块 → ReasoningContent；
     cache_control（system ephemeral）；stop_reason 映射；overloaded_error。
   - 同步建连（照 openai.go 手术后的结构直接写对，非 2xx → HTTPError）。
   - httptest 假 SSE 逐事件断言。
2. **M4**：compact 双策略（Summarize 新写 + Trim 回退）+ 工具集定稿
   （含 list_code_definition_names 去留）。
3. **M5**：examples + README 中英 + Maestro embedded 验证 → tag v0.1.0。

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
- 仓库推送已提前完成；tag/release 留到 M5。

## 经验与洞察（来自调研）

- gline 的预派发（SSE 流期间并行跑只读工具）与三锁竞态防护是亮点，
  已原样保留并有 `-race` 回归测试（TestPreDispatchExecutesOnce）。
- Anthropic 适配器难点在 SSE 事件序与 stop_reason 映射，
  用 httptest 假 SSE 逐事件断言（openai_stream_test.go 已示范结构）。
- Guard 测试必须放行 attempt_completion，否则 deny 环造成 O(n²)
  token 估算假死（AutoCompact 本身无 bug）。
