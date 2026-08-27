# Active Context: agentcore

## 当前工作焦点

**M0：仓库初始化**——写需求（已完成）、建 go.mod、LICENSE、.gitignore、
README 骨架，然后进入 M1 骨架迁移。

## 近期变更

- 2026-08-28：项目创建于 `C:/Users/22569/Documents/20-Projects/agent-core`。
  完成三路线调研（pi RPC 子进程 / pi Go 移植 / gline 提取），选定 gline 提取。
- 2026-08-28：requirements.md 定稿（v0.1），纳入 memory bank 体系管理。
- 2026-08-28：**仓库已创建并推送**：https://github.com/liup215/agentcore（public，
  main 分支，origin 已配置）。注意：这只是仓库托管；v0.1.0 tag/release 仍按
  里程碑 M5 完成后再发。

## 下一步（按里程碑顺序）

1. **M0**：`go mod init github.com/liup215/agentcore`（go 1.25）、MIT LICENSE、
   .gitignore、README（指向 memory-bank/requirements.md）、首次 git commit。
2. **M1**：从 gline 复制手术清单（requirements.md §10）标注的文件到
   types/ tools/ provider/，剔除第三方依赖，移植现有单测。
   关键源文件：
   - `gline/pkg/types/`（message.go 等，剔除 render/slash）
   - `gline/internal/api/openai.go`、`registry.go`、`mock.go`
   - `gline/internal/tools/`（tool.go/registry.go/init.go 及六件套实现）
   - `gline/internal/agent/xml_tools.go`
3. **M2**：`gline/internal/agent/agent.go`（1173 行）拆分重构为
   主循环/流处理/工具执行三段，先固化行为基线测试再动手。
4. **M3**：新写 Anthropic 适配器（参照 pi 的 anthropic-messages.ts 移植）。
5. **M4**：compact 双策略 + 工具集裁剪定稿。
6. **M5**：examples + README 中英 + Maestro embedded 后端接入验证 →
   发 v0.1.0（`git tag v0.1.0 && git push origin v0.1.0` + `gh release create`；
   仓库已于 2026-08-28 创建，此步只剩打 tag 与 release）。

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
