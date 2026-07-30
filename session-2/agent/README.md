# CLI AI Agent (Go)

A command-line AI agent you talk to in a loop. It answers from its own
knowledge, searches the web, reports the weather, writes and runs scripts, edits
files, and **asks a human before doing anything dangerous**. It also compacts its
own conversation history so a long session doesn't keep growing the token bill.

Model access goes through the [OpenRouter Go SDK](https://github.com/OpenRouterTeam/go-sdk)
(`github.com/OpenRouterTeam/go-sdk`), which needs **Go 1.25+**.

## Run

```sh
# from this directory; needs OPENROUTER_API_KEY in .env
go run .
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
        llm/                   OpenRouter Go SDK client factory
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
| 2 | Search the web | `NativeWebSearch` — OpenRouter's own `web` plugin — `tools/nativesearch.go` |
| 3 | Write scripts and run them | `WriteFile` + `RunCommand` + `ReadFile` — `tools/files.go`, `tools/shell.go` |
| 4 | Edit existing files | `ReadFile` + `EditFile` — `tools/files.go` |
| 5 | Human-in-the-loop before danger | `Approver` gate on write/edit/delete/run — `tools/*.go`, `ui/console.go` |
| 6 | Eval suite | `tools/tools_test.go` + `agent/eval_test.go` + `agent/eval_single_test.go` |

## Beyond the six

- **`get_weather`** — current temperature and wind for a place, via Open-Meteo's
  free, keyless APIs (geocode the name, then fetch conditions). Read-only, no
  approval — `tools/weather.go`.
- **Thinking spinner** — a turn can take several model round trips, so the
  console animates a one-line indicator while it waits instead of leaving the
  terminal dead. Everything that prints mid-turn (tool lines, compaction
  notices, the approval prompt) stops the spinner first and restarts it after,
  so a frame never lands on top of real output. It draws only to a terminal —
  on a pipe or a test buffer it's a no-op, keeping captured output free of
  carriage-return noise — `ui/spinner.go`, `ui/console.go`.
- **History compaction** — every `CompactEvery` questions (default 5), the agent
  folds the older part of the conversation into one summary message and keeps the
  system prompt and the most recent turn verbatim. This caps per-request token
  growth without blurring the turn a follow-up is most likely to reference. It
  runs only at a safe boundary (after a final answer, so no tool call is left
  awaiting its result). Best-effort: if summarizing fails, the full history is
  kept — `agent/agent.go`.

## How the loop works (`agent/agent.go`)

`Agent.Ask` runs `think → runTools` until the model replies with no tool calls:

1. **think** — send the conversation + tool specs to the model.
2. If the reply has no tool calls, it's the final answer — return it.
3. **runTools** — run each requested tool, append results as `role: tool`
   messages, and loop (capped at `maxSteps`).

When a turn ends with a final answer, the agent counts the turn and compacts the
history if it has hit the interval (see **Beyond the six**).

The model never runs code itself — it only *asks*. A dangerous tool first calls
`Approver.Confirm` for a y/n on the terminal.

## Tests

```sh
go test ./...            # everything (also live web search + a real model call)
go test ./... -short     # fast, offline, deterministic (no key, no network)
```

- **`ui` package** — spinner unit tests: it animates and clears, stays silent
  off a terminal, and the console pauses it before printing mid-turn.
- **`tools` package** — unit evals: script roundtrip, edit, denial blocks the
  action, read-only tools never prompt, unknown tool handled, live web search.
- **`agent` package** — live evals (skipped with `-short` or without a key):
  - *behavioral* (`eval_test.go`) — whole tasks through the real model, graded
    on which tools it chose, its answer, and the actual side effects on disk.
  - *tool selection* (`eval_single_test.go`) — one-shot: does the model pick the
    right tool, with the right arguments, on the first step? Never executes.
  - *compaction* (`compact_smoke_test.go`) — asks five questions and checks the
    history folds down to the system prompt, a summary, and the last turn.

  The behavioral and tool-selection evals print a scorecard.

## Web search

`openrouter_web_search` is the agent's only way to look something up, and it
goes through OpenRouter rather than a third-party search API.

OpenRouter doesn't expose search as its own endpoint — it models search as a
*plugin on a chat request*. So the tool sends the query as a one-off completion
with the `web` plugin attached; OpenRouter runs the search, injects the hits
into that request's prompt, and the model writes the findings back. What the
agent gets is therefore an already-summarized answer with source URLs, not raw
ranked results, at the cost of one extra model call.

`OPENROUTER_API_KEY` is the only key you need — searches bill to the same
account as the model calls. Knobs live on the `NativeWebSearch` struct:
`MaxResults` (default 5) and `Engine` (`native`, `exa`, `parallel`,
`firecrawl`, `perplexity`; empty lets OpenRouter choose).

The tool needs an authenticated client, so `tools.Default` only registers it
when given `WithOpenRouterSearch(client, model)` — without that option the
agent has no search at all and must answer from its own knowledge.
