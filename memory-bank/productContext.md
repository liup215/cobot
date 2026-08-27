# Product Context: agentcore

## 解决的问题

1. **Maestro 的外部依赖链**：Worker/评审/需求发起等角色依赖 claude CLI
   子进程（Node.js 运行时），部署与升级受制于外部工具链。agentcore 让
   agent 执行回到进程内纯 Go。
2. **gline 与 Maestro 的重复建设**：两者都需要"循环 + 工具 + provider +
   压缩"内核。提取为共享包后，gline（GUI 场景）与 Maestro（headless 场景）
   共用同一内核，bug 修复双向受益。
3. **人工审批门没有标准挂点**：ToolGuard 提供统一的 PreToolUse 拦截位，
   Maestro 在此实现人工审批/策略拦截，工具调用被拒时把原因回喂给模型。

## 用户与使用方式

- **主要用户**：Maestro（headless daemon，Worker 在隔离 worktree 中跑任务）。
- **次要用户**：gline（未来可切回该内核）、任何需要纯 Go agent 循环的 CLI/服务。
- 使用形态：Go library。使用方组装 Provider + Registry + Callback +
  ToolGuard + Store，调用 loop 完成一次任务运行；无 UI、无守护进程。

## 产品体验目标（库的 DX）

1. **五分钟上手**：mock provider + 内置工具即可离线跑通一个 agent loop。
2. **行为可观测**：Callback 事件覆盖内容流 / reasoning / 工具起止 / 压缩 /
   完成 / 错误，使用方据此渲染进度（Maestro 的事件流与 task progress）。
3. **安全可控**：副作用工具（write/replace/execute）默认建议套 ToolGuard；
   工具结果可控截断；token 预算自动水位管理。
4. **依赖零负担**：引 agentcore 不引入任何传递依赖，不影响宿主的 go.mod 卫生。

## 与竞品/替代方案的差异

- vs Claude Agent SDK（子进程）：无外部 CLI、无 Node 运行时、错误与流
  完全进程内可控。
- vs pi（TS harness）：不追求交互式体验（TUI/steer/extensions），只取
  headless 需要的最小闭环：loop + 工具 + 流式 + 压缩。
- vs 通用编排框架（ADK-Go/Eino/LangChainGo）：不做图编排与 RAG 生态，
  只做单一 agent 的"领任务→执行→交付"内核，小而稳。
