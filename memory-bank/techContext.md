# Tech Context: cobot

## 技术栈

- **语言**：Go 1.25（兼容 maestro 的 1.26）
- **依赖**：运行时仅 stdlib；测试仅 stdlib（testing/httptest）；
  日志 `log/slog`；无 CGO
- **模块**：`github.com/liup215/cobot`
- **License**：MIT

## 提取源（只读基线）

- gline 仓库：`C:/Users/22569/Documents/20-Projects/gline`
- 基线快照：2026-06-27，commit `fix-replace-in-file-errors`
- 源文件映射（gline → cobot）：

| gline 源 | cobot 去向 | 处置 |
|---|---|---|
| pkg/types/message.go 等 | types/ | 收编，剔除 render_strategy/slash |
| internal/api/openai.go | provider/openai.go | 移植（stdlib 化） |
| internal/api/mock.go | provider/mock.go | 移植 + 脚本化回放增强 |
| internal/tools/tool.go, registry.go | tools/ | 移植 |
| internal/tools/init.go 及六件套 | tools/ | 裁剪到六件套 + attempt_completion(+ask_followup) |
| internal/agent/xml_tools.go | tools/xml_fallback.go | 移植 + 测试 |
| internal/agent/agent.go (1173行) | loop/（拆 3 文件） | **重构拆分**，行为语义不变 |
| internal/agent/provider.go | provider/（接口层） | 移植 |
| internal/storage/store.go（仅接口） | loop/store.go | 裁剪移植 |
| pi 的 anthropic-messages.ts | provider/anthropic.go | 参照新写（不逐行翻译） |

## 本地开发

```bash
cd C:/Users/22569/Documents/20-Projects/cobot
go build ./...          # M0 起必须通过
go vet ./...
go test -race ./...     # 全部单测不得依赖真实网络
```

- 开发平台：Windows 11（主要）；CI 可后置（M5）。
- 测试双支柱：mock provider（loop 行为）+ httptest 假 SSE（anthropic 适配器）。

## 发布（M5，经用户确认后执行）

```bash
gh repo create liup215/cobot --public --source . --push \
  --description "Pure-Go agent SDK extracted from gline"
git tag v0.1.0 && git push origin v0.1.0
gh release create v0.1.0 --title "v0.1.0" --notes "..."
```

- gh 已登录账号 liup215（https 协议）。
- Go module proxy 自动拾取 tag，无需注册。

## 技术约束

- go.mod require 不得出现第三方包（CI/验收用 `go list -m all` 检查）。
- 单文件 ≤ ~400 行（loop 拆分后的上限）。
- 三平台（win/linux/darwin）可编译可测试。
- 破坏性 API 变更仅允许在 v0.x；v1.0 前冻结模块路径。
