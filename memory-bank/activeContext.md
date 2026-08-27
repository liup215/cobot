# Active Context: agentcore

## 当前工作焦点

**M1 骨架迁移已完成（types + provider + tools 全部落地）**，接下来进入
**M2：loop 重构**——把 gline 1173 行 agent.go 拆为主循环/流处理/工具执行三段。

## 近期变更

- 2026-08-28：**tools 包迁移完成（commit da55106）**：
  - tool.go/registry.go/file.go/command.go/search.go/interaction.go 均为
    cp 字节级复制 + 仅 gofmt 对齐；file/search 测试 0 语义差异。
  - init.go 裁剪为 v1 九件套注册（六件套 + ask_followup/plan_mode_respond/
    attempt_completion）；剔除 memory/summarizer/browser/web_fetch/use_skill。
  - list_code_definition_names 实现保留但未注册（手术清单未列剔除，M4 定去留）。
  - xml_fallback.go(+test)：签名改为 `ParseXMLToolCalls(content, toolNames []string)
    []types.ToolCall`，避免 tools→provider 反向依赖；log 内联 slog。
  - Smoke 验证：InitDefaultRegistry 注册 9 工具无重复，plan/act 过滤正常。
- 2026-08-28：**provider 包迁移完成（commit 477b224）**。采用“cp 字节级复制
  + sed 仅改包名/路径/前缀”的流程，彻底避免 write 重写引入不可见字符：
  - provider/provider.go（接口层，源 gline internal/agent/provider.go）
  - provider/openai.go（SSE 流式，源 internal/api/openai.go；zerolog 23 处
    调用改为 log/slog shim，见 provider/log.go）
  - provider/mock.go、registry.go（registry 内重复 ProviderConfig 已删，
    复用接口层定义）、openai_test.go（6 测试全绿，-race）
  - gline 语义零改动；CRLF 已统一为 LF；perl 扫描确认无零宽/BOM 字符。
- 2026-08-28：types 包迁移完成（message/conversation/token-estimator）。
- 2026-08-28：仓库创建并推送 https://github.com/liup215/agentcore。

**重要经验**：从 gline 迁移文件一律用 `cp` + 字节级 `sed`（包名、import
路径、`agent.` 前缀、log 前缀），禁止用 write 工具重写源文件——之前用
write 创建 provider 文件反复出现特殊不可见字符导致失败。

## 下一步（按里程碑顺序）

1. **M2 loop 重构**：先用 mock provider 固化行为基线测试（终结协议/
   纠错环/预派发），再把 `gline/internal/agent/agent.go`（1173 行）拆为
   主循环/流处理/工具执行三段；ToolGuard（两挂点 + ErrAbortRun）、
   自动重试、Store 接口。同样遵循 cp+sed 复制流程。
2. **M3**：新写 Anthropic 适配器（参照 pi 的 anthropic-messages.ts 移植；
   可直接拷贝 provider/openai.go 作为骨架再改事件解析）。
3. **M4**：compact 双策略 + 工具集裁剪定稿（含 list_code_definition_names 去留）。
4. **M5**：examples + README 中英 + Maestro embedded 后端接入验证 →
   发 v0.1.0（仓库已存在，此步只剩打 tag 与 release）。

## 活跃决策

- **模块路径** `github.com/liup215/agentcore`，仓库目录名 agent-core（本地）。
- **零依赖硬约束**：go.mod require 仅 stdlib；日志用 log/slog（不迁移 zerolog）。
- **提取基线冻结**：gline @ 2026-06-27；不在提取过程中顺手改进 gline 语义，
  行为变更只做 requirements.md 列出的四项新增（Anthropic/ToolGuard/
  Summarize compaction/自动重试）。
- 仓库推送已按用户指令提前完成（原计划 M5 后）；tag/release 仍留到 M5。

## 经验与洞察（来自调研）

- gline 的预派发（SSE 流期间并行跑只读工具）与三锁竞态防护是亮点，
  务必原样保留并加 `-race` 回归测试。
- `claude` CLI `-p` 模式必须经 stdin 传 prompt（位置参数会失败）——迁移到
  进程内后此坑消失，但 Maestro runner 的 `--mcp-config`/workdir 逻辑要相应重审。
- Anthropic 适配器的难点在 SSE 事件序（content_block_start/delta/stop 嵌套
  tool_use 增量）与 stop_reason 映射，用 httptest 假 SSE 逐事件断言。
