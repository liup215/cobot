# System Patterns: agentcore

## 架构总览

```
使用方（Maestro / gline / 示例）
      │ 组装
      ▼
┌───────────────────── agentcore ─────────────────────┐
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
- agentcore 不 import 使用方；全部解耦通过接口。

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

## 组件关系

| 组件 | 依赖 | 产出 |
|---|---|---|
| types | stdlib | 消息/工具调用/token 估算 |
| provider | types | openai(迁移) / anthropic(新写) / mock |
| tools | types | Tool/Registry、六件套、XML 兜底、ToolGuard 类型 |
| loop | provider+tools+compact+types | 一次任务运行的完整生命周期与事件 |
| compact | types(+provider 经接口) | Trim / Summarize 策略 |

## 关键实现路径

1. **单次运行**：buildSystemPrompt(模式+工具) → token 预算(60%压缩/80%裁剪)
   → CreateMessageStream → 流式分发(Callback+预派发) → 逐个工具
   (guard→execute→store) → 终结判断 → CompleteTask。
2. **审批路径**：工具调用 → ToolGuard → allow: 执行 / deny: [DENIED] 结果
   回喂模型 → 模型自适应；ErrAbortRun → Abort()。
3. **压缩路径**：预检 token → Summarize(旧轮→摘要块) → 失败 → Trim →
   OnCompactionStart/End 事件。
