# cobot

> A pure-Go (CGO-free) agent foundation framework — extracted and refactored from gline (of Cline lineage) so projects like Maestro and LTEDU can drive an LLM agent in-process. Ships a default tool set (read/grep/glob/run/…) plus reusable capability layers (the shell executor, the docparse document parser). Dependency policy: third-party libraries are pure Go only (gopdf/striprtf/word-extractor/xls, all for document parsing) — the old "zero dependencies" claim is retired.

[![Release](https://img.shields.io/github/v/tag/liup215/cobot?label=release&sort=semver)](https://github.com/liup215/cobot/releases/tag/v0.1.0)
[![Go](https://img.shields.io/badge/go-1.25-00ADD8?logo=go&logoColor=white)](https://go.dev)
[![Dependencies](https://img.shields.io/badge/dependencies-0-brightgreen)](go.mod)
[![License](https://img.shields.io/badge/license-MIT-blue)](LICENSE)
[![Tests](https://img.shields.io/badge/tests-go%20test%20%2Drace%20%E2%9C%93-success)](memory-bank/progress.md)

English | [简体中文](README.md)

**Status**: M0–M5 complete (`go test -race ./...` green): root-package facade, Anthropic adapter, dual-strategy compaction (Trim port + Summarize), finalized tool behaviors, four runnable examples and bilingual docs. Current release: **v0.1.0** (see [Versioning](#versioning--release)). The single source of truth for the spec is [memory-bank/requirements.md](memory-bank/requirements.md) (Chinese).

## Quick Start

```go
import "github.com/liup215/cobot"

agent, err := cobot.NewAgent(cobot.Config{
	APIKey:      "sk-...",   // OpenAI-compatible endpoint; or inject a Provider
	Model:       "gpt-4o",
	AutoApprove: true,
})
res, err := agent.Run(ctx, "fix the login bug")
// res.FinalText      the attempt_completion `result` parameter (falls back to
//                    the last assistant text when no terminal tool was called)
// res.InputTokens / res.OutputTokens  sum of per-turn usage for this run
// res.Turns / res.Duration / res.Err completed turns, wall-clock time and
//                    terminal error (nil = ended normally)
```

A single `Config` entry point (root-package facade): every field is optional and
falls back to the default documented per field in the table below.
`Run` / `RunWithCallback` return `(*RunResult, error)`: `err == nil` means the run
ended normally (including via `Abort()`); a canceled `ctx` interrupts stream reads
and tool execution and returns an error for which `errors.Is(err, context.Canceled)`
holds; when the host's `ask_followup_question` callback returns an error, the error
text is fed back to the model as a tool result and the run continues at the model's
discretion.

Fully runnable offline examples live in [`examples/`](examples/):
[`basic`](examples/basic) (minimal run), [`streaming`](examples/streaming)
(streaming events), [`custom-tool`](examples/custom-tool) (registering a custom
tool) and [`guard`](examples/guard) (the three ToolGuard outcomes). All four replay
LLM turns with a scripted provider — `go run ./examples/<name>` needs no API key.

## Config Reference

All fields are optional except that exactly one of `APIKey`/`Provider` is required.

| Field | Type | Default | Description |
|---|---|---|---|
| `APIKey` | `string` | required unless `Provider` is injected | API key for the OpenAI-compatible endpoint (OpenAI, Volcengine Ark, DeepSeek, Kimi, ...) |
| `BaseURL` | `string` | official OpenAI API | Endpoint override, e.g. `https://ark.cn-beijing.volces.com/api/v3` |
| `Model` | `string` | provider default (`gpt-4`) | Model name sent to the provider |
| `Provider` | `cobot.Provider` | nil | Inject a fully custom backend (non-OpenAI protocol, test double); bypasses `APIKey`/`BaseURL` |
| `Tools` | `*cobot.ToolRegistry` | `tools.InitDefaultRegistry()` (the ten built-ins) | Tool registry exposed to the model; to extend the defaults start from `InitDefaultRegistry()` and `Register` extras |
| `Mode` | `cobot.Mode` | `ModeAct` | Initial mode: `ModeAct` executes changes; `ModePlan` explores and plans (write tools hidden) |
| `AutoApprove` | `bool` | false | Yolo mode, skips per-tool approval; prefer a `Guard` for execution control (see `Guard` row) |
| `MaxConsecutiveMistakes` | `int` | 3 | How many consecutive turns the model may fail to call a tool before the run aborts |
| `CustomRules` | `string` | "" | Extra rules appended to the system prompt; ignored while `SystemPromptOverride` is set |
| `SystemPromptOverride` | `string` | "" | When non-empty, **completely replaces** the built-in system prompt (for hosts shipping their own persona); the `AVAILABLE TOOLS` section is still rendered by the loop from the Registry; `CustomRules`/`AgentName` are ignored |
| `AgentName` | `string` | `"cobot"` | Agent identity in the system prompt's role section; ignored while `SystemPromptOverride` is set |
| `MaxTokens` | `int` | 128000 | Conversation context token budget (compaction watermarks derive from it) |
| `RetryDelays` | `[]time.Duration` | `DefaultRetryDelays` (1s/2s/4s/8s) | Backoff schedule for automatic stream-connection retries (a longer server `Retry-After` wins); nil = default, empty non-nil = disabled. Only synchronous connection failures are retried — never mid-stream errors |
| `Compaction` | `cobot.Strategy` | `Combo(Summarize, Trim)` | Compaction strategy applied at the 60% pre-request check and the 80% hook; the 100% pre-send hard trim never calls the LLM |
| `Summarizer` | `cobot.Summarizer` | one non-streaming call over the session provider | Summarizer used by the default compaction strategy; ignored when `Compaction` is set explicitly |
| `SummarizePrompt` | `string` | `DefaultSummarizePrompt` | Overrides the summarization instructions (the transcript is always appended); ignored when `Compaction` is set explicitly |
| `Guard` | `cobot.ToolGuard` | nil | Approval gate consulted before every tool execution (both the main-loop and pre-dispatch paths); see [Embedding](#embedding-guide) |
| `Store` | `cobot.Store` | nil (persistence disabled) | Hook for persisting tasks, messages and tool calls |
| `Title` | `string` | "" | Title used when a new task is created |

## Embedding Guide

For a host (e.g. Maestro) embedding cobot in its own process, three things matter:
**give it tools, give it a persona, take the result back**.

### 1. Register custom tools (inject host-native tools)

Implement the `tools.Tool` interface (`Name`/`Description`/`InputSchema`/`Execute`),
wrap it in a `tools.ToolInfo`, register it in a registry and hand the registry to
the agent via `Config.Tools`:

```go
reg := tools.InitDefaultRegistry()           // keep the built-ins
reg.Register(&tools.ToolInfo{
	Tool:         &myHostTool{},             // your Go implementation
	Category:     tools.CategoryCommand,     // informational
	AllowedModes: []string{"plan", "act"},   // modes where it is available ("*" = all)
})
agent, _ := cobot.NewAgent(cobot.Config{Tools: reg})
```

The tool's `Description` and `InputSchema` appear in the system prompt's
`AVAILABLE TOOLS` section — they are the model's documentation, so state clearly
when to use the tool and what each parameter means. Full runnable version:
[`examples/custom-tool`](examples/custom-tool).

### 2. Take over the system prompt

When `SystemPromptOverride` is non-empty it **completely replaces** the default
system prompt (the default role, conventions and custom rules are all dropped) —
the host ships its own full persona prompt. The single exception: the
`AVAILABLE TOOLS` section (tool names, descriptions and JSON schemas, filtered by
the current mode) is still rendered by the loop from the Registry — without it the
model would see no tool schemas at all and the host would have to restate each one.

```go
agent, _ := cobot.NewAgent(cobot.Config{
	SystemPromptOverride: "You are ... (the host's full prompt)",
})
```

### 3. Read the RunResult

`Run`/`RunWithCallback` return a `*cobot.RunResult` bound to the run that produced it:

| Field | Semantics |
|---|---|
| `FinalText` | The deliverable: the `attempt_completion` `result` / `plan_mode_respond` `response`; any other ending falls back, best effort, to the last assistant text produced during this run. The deliverable is captured when the assistant turn is recorded, so mid-run compaction can neither blank it nor surface an earlier run's text |
| `InputTokens` / `OutputTokens` | Sum of the per-turn provider-reported usage for this run (0 when the provider never reports usage) |
| `Turns` | Number of completed LLM turns (streams read to the end); retried connections and turns cut short by errors/cancellation are not counted |
| `Duration` | Wall-clock time of the run |
| `Err` | Terminal error; nil = ended normally. Always mirrors the error returned by `Run`/`RunWithCallback` |

Cancellation: a canceled `ctx` is a hard interrupt (it interrupts stream reads and
tool execution, including pre-dispatch goroutines) and `errors.Is(err,
context.Canceled)` holds; `Abort()` keeps its cooperative-checkpoint semantics
(it does not interrupt an in-flight stream read); when both are in play,
cancellation wins.

### 4. Other hooks

- **Streaming UI**: pass a `cobot.Callback` to `RunWithCallback` (embed
  `cobot.CallbackAdapter` and override only what you need): `OnContent`/
  `OnReasoning` text and thinking deltas, `OnToolCallStart/Update/Complete` tool
  lifecycle, `OnCompactionStart/End` compaction events, `OnRetry` connection
  retries, `AskFollowupQuestion` clarification interaction. See
  [`examples/streaming`](examples/streaming).
- **Approval gate**: `Config.Guard` (a `cobot.ToolGuard`) is consulted before every
  tool execution (both the main-loop and pre-dispatch paths, possibly twice per
  call → it MUST be side-effect free and idempotent). Three outcomes: allow
  `(true, "", nil)`; deny `(false, reason, nil)` (the tool is not executed, the
  model receives `[DENIED] <reason>` and the run continues); an error wrapping
  `cobot.ErrAbortRun` ends the whole run immediately; any other non-nil error
  fails closed (treated as a deny). See [`examples/guard`](examples/guard).
- **Persistence**: implement `cobot.Store` to plug in task/message/tool-call
  storage; it is nil-safe — leaving it nil disables persistence.
- **Compaction tuning**: `Config.Compaction` accepts any `compact.Strategy`
  implementation (built-ins: `compact.NewTrim()`, `compact.NewSummarize(...)`,
  `compact.NewCombo(...)`).

## Long-Task Context Compaction

Past 60% of the context budget the oldest conversation rounds are compacted; the
default policy is "summarize first, fall back to trimming":

- **Summarize**: keeps the last `KeepRounds` (default 6) rounds verbatim and renders
  older rounds into a transcript handed to the `Summarizer` (by default
  `loop.ProviderSummarizer`, a single non-streaming call over the session's own
  provider), which is replaced by one structured five-section summary (task goal /
  completed steps / key decisions / open questions / files). The prompt can be
  overridden via `Config.SummarizePrompt`; a timeout (default 120s), provider
  errors, an empty summary and a missing summarizer all count as failure.
- **Trim** (ported from gline's watermark trimming): no LLM call; drops the oldest
  rounds until back below the 60% watermark; always preserves the system message,
  the first user message (the original task) and the last 6 rounds; falls back to
  gline's hard trim past 80%.
- **Combo**: Summarize first, falling back to Trim on any failure; if both fail the
  loop logs and keeps running, with the pre-send hard trim as the safety net.

Compaction operates on whole "rounds" (one user message plus all of its responses;
an assistant `tool_call` is never separated from its tool result) — both wire
formats require `tool_use` and `tool_result` to be adjacent, and dropping single
messages would leave dangling tool results.

```go
// Custom: trim only, no summarization calls
agent, _ := cobot.NewAgent(cobot.Config{
	Compaction: compact.NewTrim(),
})
```

## Built-in Tool Behavior (finalized)

The default registry (`tools.InitDefaultRegistry()`) registers ten tools; every
behavior below is pinned by tests (`tools/behavior_test.go`):

| Tool | Finalized behavior |
|---|---|
| `read` | Line-paged reads: 1000 lines by default, `limit` up to 2000 lines, 50KB byte cap per chunk (truncated with a `[Chunk truncated … Continue with line_number=N]` pointer, rune-boundary safe); `~` expansion and relative paths made absolute; **document extraction (docparse registry)**: PDF/Word (incl. .doc)/Excel (incl. .xls)/PPT/ODF/RTF/HTML/EPUB are text-extracted before paging (header notes `text extracted from …`, `line_number` indexes the extracted text), zips return an entry listing, binaries with no text layer get a friendly notice instead of garbage; image files return metadata instead of binary |
| `write` | Atomic write: same-directory temp file + `rename`, preserving the original file mode; a failed write never destroys the original. Creates directories as needed |
| `edit` | All blocks are applied in memory first; any `search` mismatch → the file is left untouched and the failing block is named (all-or-nothing); the result is written back atomically |
| `run` | **Shell resolution prefers bash** (finds Git Bash on Windows, WSL stubs skipped; `cmd /C` only as fallback; `bash`→`sh` on Unix), with the syntax hint baked into the tool description; foreground, blocking, default timeout 120s (per-call `timeout`, capped at 600s); on timeout the whole process tree is killed (`taskkill /F /T` + `WaitDelay` backstop on Windows); stdout/stderr captured separately, each truncated at 32KB (head 3/4 + tail 1/4 + marker, rune-boundary safe); a non-zero exit code is a result, not an error; `requires_approval` or destructive commands go through the approval hook (`SetApprovalHandler`, fail-closed). Not for long-running services |
| `grep` | **ripgrep fast path** when `rg` is installed (parallel traversal, .gitignore-aware, scan killed at the cap), falling back to a concurrent pure-Go walk; deterministic ordering (path → line → column, stable sort); `TotalMatches` reports the real hit count; results beyond 500 are truncated with a `[Results truncated: …]` notice |
| `ls` | Entries sorted by filename (`os.ReadDir` semantics, pinned); truncated past 500 entries with a notice |
| `glob` | Recursive filename glob; **fd fast path** when `fd` is installed (parallel + .gitignore-aware), falling back to a pure-Go walk; returns up to 500 paths |
| `attempt_completion` | Terminal tool: `result` required, optional `command` (a command for the user to verify the result) |
| `ask_followup_question` | Clarification question, does not end the run; `SetHandler` injects the interaction side (TUI/host), falling back to stdin; answers may be given by option index; in headless use a handler error is fed back to the model as text and the run continues |
| `plan_mode_respond` | Plan-mode terminal tool: the response is echoed back verbatim |

File/command tools deliberately do **no working-directory sandboxing** (gline
baseline): paths may point outside the working directory; control execution with
`Config.Guard` (`ToolGuard`).

## Design Notes (v0.1)

- `github.com/liup215/cobot`, Go 1.25, runtime dependencies = 0 (stdlib only)
- Package layout: root facade (`cobot.NewAgent(Config)`) / `types` / `provider`
  (OpenAI-compatible + **anthropic** + mock/scripted) / `tools` (registry +
  ToolGuard + XML fallback) / `loop` / `compact` (Trim + Summarize + Combo) /
  `examples`
- Inherited from gline (Cline): the `attempt_completion` terminal protocol,
  Plan/Act, the correction loop, stream pre-dispatch, the reasoning channel
- Added (drawing on pi): a streaming Anthropic adapter, LLM-summary compaction, a
  PreToolUse approval gate, automatic retries
- Explicitly out of scope: TUI, extensions, session trees, OAuth, MCP, orchestration

## Versioning & Release

- **v0.1.0** ([release](https://github.com/liup215/cobot/releases/tag/v0.1.0)): the
  tag points at the M0–M3 completion point (root facade, OpenAI-compatible
  provider, Anthropic adapter, tools/loop skeleton, ToolGuard, Store, automatic
  retries, mock/scripted providers). It does **not** include the M4 compaction
  strategies or the M5 examples/docs — those live on `main` and will ship with a
  later tag (v0.1.1 / v0.2.0). Keep this in mind when pinning with `go get`.
  Known limitations are listed in the release notes.
- Milestone details (M0–M5) are tracked in
  [`memory-bank/progress.md`](memory-bank/progress.md) (Chinese).

## License

MIT
