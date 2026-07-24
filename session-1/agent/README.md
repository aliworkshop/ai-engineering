# CLI AI Agent (Go)

A command-line AI agent you talk to in a loop. It answers from its own
knowledge, searches the web, writes and runs scripts, edits files, and **asks a
human before doing anything dangerous**.

## Run

```sh
# from the repo root; needs OPENROUTER_API_KEY in .env
go run ./agent
```

Type questions; type `exit` to quit.

## Architecture

Dependencies point inward. The UI knows the agent; the agent knows an abstract
tool box; the tools know an abstract approver. Nothing inner imports anything
outer, so each layer is testable on its own.

```
main.go                       wire everything together, then run
  └── internal/
        ui/          Console   REPL + terminal human-approver (owns stdin)
        agent/       Agent     the think → run-tools → repeat loop
        tools/       Registry  Tool interface + all tools + Approver port
        llm/                   OpenRouter client factory
```

Key seams (interfaces):

- **`tools.Approver`** — the human-in-the-loop gate. `ui.Console` implements it
  for real; tests pass a yes/no stub. Tools never import the UI.
- **`agent.ToolBox`** — what the agent needs from its tools (`Specs`,
  `Dispatch`). `tools.Registry` satisfies it; the agent never imports concrete
  tools.
- **`tools.Tool`** — one capability. Add a feature by writing one struct with
  `Spec()` + `Run()` and listing it in `tools.Default`.

## The 6 requirements → where they live

| # | Requirement | Where |
|---|---|---|
| 1 | Answer from own knowledge (no tool) | `agent.SystemPrompt` + loop returns when there are no tool calls — `agent/agent.go` |
| 2 | Search the web | `WebSearch` (Tavily if `TAVILY_API_KEY` set, else DuckDuckGo) — `tools/search.go` |
| 3 | Write scripts and run them | `WriteFile` + `RunCommand` + `ReadFile` — `tools/files.go`, `tools/shell.go` |
| 4 | Edit existing files | `ReadFile` + `EditFile` — `tools/files.go` |
| 5 | Human-in-the-loop before danger | `Approver` gate on write/edit/delete/run — `tools/*.go`, `ui/console.go` |
| 6 | Eval suite | `tools/tools_test.go` + `agent/eval_test.go` |

## How the loop works (`agent/agent.go`)

`Agent.Ask` runs `think → runTools` until the model replies with no tool calls:

1. **think** — send the conversation + tool specs to the model.
2. If the reply has no tool calls, it's the final answer — return it.
3. **runTools** — run each requested tool, append results as `role: tool`
   messages, and loop (capped at `maxSteps`).

The model never runs code itself — it only *asks*. A dangerous tool first calls
`Approver.Confirm` for a y/n on the terminal.

## Tests

```sh
go test ./agent/...            # everything (also live web search + a real model call)
go test ./agent/... -short     # fast, offline, deterministic (no key, no network)
```

- **`tools` package** — unit evals: script roundtrip, edit, denial blocks the
  action, read-only tools never prompt, unknown tool handled, live web search.
- **`agent` package** — behavioral eval: whole tasks through the real model,
  graded on which tools it chose, its answer, and the actual side effects on
  disk. Prints a scorecard. Skipped with `-short` or without a key.

## Optional: real web search

DuckDuckGo's free API is limited. For full results, add to `.env`:

```
TAVILY_API_KEY=tvly-...
```
