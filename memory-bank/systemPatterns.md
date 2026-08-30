# System Patterns: cobot

## 架构总览

```
使用方（Maestro / gline / 示例）
      │ 组装
      ▼
┌───────────────────── cobot ─────────────────────┐
│ loop/  主循环 ──► 流处理 ──► 工具执行（三段拆分）      │
│   │            Callback 事件接口     │               │
│   │  ToolGuard（审批门，两路径均挂）  │               │
│   ▼                                  ▼               │
│ provider/ ── Provider 接口 ──► openai │ anthropic │ mock│
│ tools/   ── Tool/Registry + 六件套 + XML 兜底        │
│ compact/ ── Strategy: Trim(移植) + Summarize(新写)   │
│ types/   ── Message/ToolCall/Usage/Conversation      │
└──────────────────────────────────────────────────────┘
```

## 依赖方向（硬规则，禁止反向）

```
loop ──► provider ──► types
  │        ▲
  └──► tools ──► types
  └──► compact ──► types
```

- 使用方 implements：`loop.Store`（持久化）、`loop.Callback`（事件消费）、
  `compact.Summarizer`（摘要实现）、自定义 `tools.Tool`。
- cobot 不 import 使用方；全部解耦通过接口。

## 关键设计模式（继承自 gline/Cline 血统）

1. **终结工具协议**：`attempt_completion` / `plan_mode_respond` 调用即结束
   运行；`ask_followup_question` 不终结，等待使用方注入应答后继续。
2. **纠错环**：模型有工具可用却不调用 → 注入 `noToolsUsedMsg` 错误消息；
   连续错误计数达上限（默认 3）→ 终止并返回错误。
3. **Plan/Act 双模式**：工具集按模式过滤（`GetForMode`），plan 模式禁写。
4. **预派发**：SSE 流期间并行执行只读工具（副作用工具排除，白名单可配）；
   preDispatchedIDs/results 三锁防竞态——**原样保留，不重构**。
5. **XML 兜底**：无原生 tool_calls 时从正文解析 `<tool>...参数...</tool>`，
   兼容 function calling 弱的 OpenAI 兼容模型（DeepSeek/Kimi）。
6. **ToolChoice 策略**：有待办工作时 `tool_choice=auto`（不用 required）。

## 新增设计（借鉴 pi / 新写）

1. **ToolGuard**：`func(ctx, call) (allow bool, reason string)`；
   拒绝 → 工具结果消息 `[DENIED] <reason>`，循环继续；哨兵 `ErrAbortRun`
   → 整体中止。主循环与预派发两条路径都必须过 guard。
2. **Summarize compaction**：水位触发 → 最旧 N 轮交 Summarizer（使用方实现，
   内部调 provider）→ 替换为 `[COMPACTED]` 摘要块，保留最近 K=6 轮；
   失败回退 Trim。
3. **自动重试**：流建连/中途错误 → 指数退避（1s 起步，×2，至多 4 次），
   尊重 Retry-After，4xx 参数错不重试；可选 OnRetry 事件。
4. **Estimator 注入**：token 估算默认 chars/4 启发式，可按模型注入精确计数。

## compaction 架构（M4 定稿）

- **压缩单元 = 轮**（`compact.SplitRounds`）：一条用户消息 + 它引发的全部
  assistant/tool 消息；assistant 的 tool_call 与 tool 结果永不分家（两种
  wire 格式都要求相邻）；前置 system 消息不属于任何轮。
- **Strategy 契约**：`ShouldCompact(conv)` + `Compact(ctx, conv)`；就地重写
  （`types.Conversation.ReplaceMessages`）、可重复调用（第二次是 no-op）、
  错误语义 = “本策略未生效”（Combo 据此回退，loop 记日志继续跑）。
- **三层保护**（Trim 与 Summarize 一致）：前置 system 消息、首条用户消息
  （原始任务）、最近 `KeepRounds`(=6) 轮。
- **水位分工**：预发送 100% = `TrimToMaxTokens` 硬修剪（无 LLM）；
  `AutoCompact` 80% 与 `enforceTokenBudget` 60% = 配置的 `compact.Strategy`
  （默认 `Combo(Summarize, Trim)`）。紧急路径永不调 LLM。
- **消息角色**：压缩通知/摘要都是 RoleUser（`[Context compacted:` /
  `[COMPACTED]` 前缀）——Anthropic 消息列表禁止 system 角色（会被提升进
  顶层 system），连续 user 轮由 API 自动合并。
- **精确投影**：Trim 决定丢几轮时用“拼装后消息列表的精确 token 数（含回插
  的 notice）”比较水位；只减不增的近似算法会被 notice 吃掉释放的预算，造成
  每次 60% 触发点重复丢轮。

## 工具行为契约（M4 定稿，tools/behavior_test.go 钉死）

| 主题 | 契约 |
|---|---|
| 超时 | execute_command 默认 120s；超时 = 结果（exit -1）而非错误；Windows 杀进程树（taskkill /F /T）+ `WaitDelay=3s` 兜底 |
| 输出 | stdout/stderr 分离，每流 32KB（保头 3/4 + 尾 1/4 + 标记）；shell = cmd /C（windows）/ sh -c（其余），`shellCommand()` 钉死 |
| 大文件 | read_file 64KB 整文件 / 100KB 行区间，超出 = 头截断 + 通知（拒绝已废除） |
| 写入 | write_to_file / replace_in_file 走 `writeFileAtomic`（同目录临时文件 + rename + 保留 mode）；replace all-or-nothing |
| 排序 | search_files (path,line,column) 稳定三键；list_files 靠 os.ReadDir 文件名排序；definitions 文件路径 + 行内行号 |
| 上限 | search_files/list_files/definitions 均 500 条，截断必须带通知；search 的 TotalMatches 报真实总数 |
| 沙箱 | 不做 workdir 沙箱（gline 基线）；ToolGuard 是执行点 |

## 组件关系

| 组件 | 依赖 | 产出 |
|---|---|---|
| types | stdlib | 消息/工具调用/token 估算 + ReplaceMessages/MessageTokens |
| provider | types | openai(迁移) / anthropic(完成) / mock；errors.go HTTPError+RetryAfter |
| tools | types | Tool/Registry、十件套（六件套+definitions+交互三件套）、XML 兜底、ToolGuard 类型 |
| loop | provider+tools+compact+types | 一次任务运行的完整生命周期与事件；ProviderSummarizer 适配器 |
| compact | types(+provider 经接口) | Trim / Summarize / Combo 策略 |

### provider 适配器共同结构（openai 与 anthropic 对齐）

- **同步建连**：转换 → marshal → httpReq → client.Do → 状态码检查全部在
  调用 goroutine 内；网络失败与非 2xx 返回 `*HTTPError`（loop 重试层据此
  退避）。goroutine 仅负责 SSE 读取。
- **流式输出契约**：文本/思考增量即时发 chunk；tool_call 先发 IsPartial
  拷贝再发完整调用；整个流恰好一个 `Done` chunk 携带 FinishReason+Usage；
  流中错误发 `{Error, Done:true}` chunk（走纠错环，不重试）。
- **工具参数健壮性**：增量 JSON 拼接 → isValidJSON 校验 →
  isCompleteJSONObject 括号修复（补 `}` 至多 10 字符）→ 仍失败则跳过并记日志。
- **SSE 读取防护**：空闲 120s 定时器 + 每读 30s SetReadDeadline；EOF 后
  flush 尾事件 + 收尾未闭合 tool_use 块 + 兜底 finish。

## 关键实现路径

1. **单次运行**：buildSystemPrompt(模式+工具) → token 预算(60%压缩/80%裁剪)
   → CreateMessageStream → 流式分发(Callback+预派发) → 逐个工具
   (guard→execute→store) → 终结判断 → CompleteTask。
2. **审批路径**：工具调用 → ToolGuard → allow: 执行 / deny: [DENIED] 结果
   回喂模型 → 模型自适应；ErrAbortRun → Abort()。
3. **压缩路径**：预检 token → Summarize(旧轮→摘要块) → 失败 → Trim →
   OnCompactionStart/End 事件。
