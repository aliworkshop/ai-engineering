# AI Agent (Go) — terminal or browser, on a real harness

> **Branch `session-4`.** Each session of the course is a branch, with the agent
> at the repo root — `git switch session-4` then `go run .`. This is the newest:
> `session-3` is the same agent without the harness, `session-2` adds the
> built-in diagram renderers, `session-1` has no tools at all.

An AI agent you talk to in a loop, **in the terminal or in a browser**. It
answers from its own knowledge, searches the web, reports the weather, writes and
runs scripts, edits files, draws diagrams, and **asks a human before doing
anything dangerous**.

Underneath it is **the harness** — the runtime layer between the agent loop and
the real world, and the part most agents don't have until it bites them. One
sentence holds the whole design together:

> **The LLM decides the next semantic step. The harness owns execution.**

Kill the process mid-task and nothing is repeated when it resumes. Model-written
code runs in a sandbox with no credentials and a timer. Context is assembled per
turn against a token budget instead of growing forever. The agent you talk to
literally cannot delete a file — it hands that to a specialist that can. And an
approval nobody answers parks the run on disk, where it costs nothing to wait
three days.

Model access goes through the [OpenRouter Go SDK](https://github.com/OpenRouterTeam/go-sdk)
(`github.com/OpenRouterTeam/go-sdk`), which needs **Go 1.25+**.

## Run

```sh
# needs OPENROUTER_API_KEY in .env

go run .                 # terminal: type questions, 'exit' to quit
go run . -http :8080     # browser: open http://localhost:8080
go run . -v              # ...with the full harness event stream

# the harness's own commands
go run . -list           # workflows, and which are waiting on you
go run . -approve <id>   # approve a parked action, then finish the run
go run . -deny <id>      # refuse it, then finish the run
go run . -resume <id>    # pick up a run that crashed
```

Everything the runtime owns lives in `.harness/` as plain files: a workflow is a
JSON file, the event log is JSONL, a pending human decision is a one-line JSON
file. That is deliberate — the concepts are the point, and swapping in Postgres
later changes the storage, not a single idea above it. It is gitignored, and
`rm -rf .harness` is a clean slate.

Both front-ends drive the *same* agent with the *same* tools and the *same*
approval gate — the only difference is who answers the y/n and where the
progress is printed.

## Architecture

Dependencies point inward. A UI knows the agent; the agent knows an abstract
tool box; the tools know an abstract approver. Nothing inner imports anything
outer, so each layer is testable on its own.

```
main.go                       wire everything together, then run
  └── internal/
        ui/          Console   REPL + terminal human-approver (owns stdin)
        web/         Server    chat page + browser human-approver (one session each)
        agent/       Agent     the loop, memory, the roster, the supervisor
        tools/       Registry  Tool interface + all tools + Approver port
        approval/    Gate      a human decision as a checkpointed step
        durable/     Workflow  checkpoint · crash · resume, with no double-sends
        sandbox/     Sandbox   the one door model-written code goes through
        events/      Event     typed stream + glyphed console + JSONL log
        llm/                   OpenRouter Go SDK client factory
```

The harness packages sit *under* the agent, and the arrows still only point
inward: `durable` and `sandbox` know nothing about agents, `approval` knows
nothing about tools, and `events` knows nothing about anything. Each is usable —
and testable — on its own.

Key seams (interfaces):

- **`tools.Approver`** — the human-in-the-loop gate. `ui.Console` implements it
  for the terminal and `web.Session` for the browser; tests pass a yes/no stub.
  Tools never import a UI.
- **`agent.ToolBox`** — what the agent needs from its tools (`Specs`,
  `Dispatch`). `tools.Registry` satisfies it; the agent never imports concrete
  tools.
- **`tools.Tool`** — one capability. Add a feature by writing one struct with
  `Spec()` + `Run()` and listing it in `tools.Default`.

## The harness

Seven parts, one runtime. Each names a production failure mode, and the part of
the codebase that answers it.

| # | Failure it prevents | Where |
|---|---|---|
| 1 | *Invisible infrastructure is undebuggable* — progress that exists only as `print` | `internal/events` — every move is a typed `Event`; the terminal renders it, the JSONL log stores it |
| 2 | *The process dies after a side effect* — state lost, model re-billed, the write done twice | `internal/durable` — a workflow is a JSON file, a step is checkpointed the moment it finishes |
| 3 | *The model asks to run code* — and something just runs it | `internal/sandbox` — one mediated door: killed process group, stripped env, empty scratch dir, timer |
| 4 | *Context grows forever* — slower, dumber, more expensive every turn | `internal/agent/memory.go` — history ≠ state ≠ context, compacted against a token budget |
| 5 | *One agent holds every tool* — no least privilege | `internal/agent/roster.go` — an agent is a name, a prompt, and a tool subset |
| 6 | *Sub-tasks run serially; one failure kills all* | `internal/agent/supervisor.go` — plan → fan out → fan in → synthesize, degrading gracefully |
| 7 | *Approval is a blocked function call* — holds the server open, dies on restart | `internal/approval` — a human decision is a checkpointed step; no answer parks the run |

### 1 · Everything is an event

The harness never prints its feelings. It emits typed events — `workflow.started`,
`tool.requested`, `approval.requested` — and the consumers differ: the terminal
renders one glyphed line each, the browser pushes them down its SSE stream, and
`.harness/events.jsonl` appends every one.

```
▶ workflow.started    wf=acca4cb5 text=write "hello harness" to /tmp/demo.txt
▪ step.completed      wf=acca4cb5 name=model-00 ms=837
↪ agent.handoff       wf=acca4cb5 from=assistant to=operator
✋ approval.requested  wf=acca4cb5 text=WRITE file "/tmp/demo.txt" (13 bytes)
⏸ workflow.suspended  wf=acca4cb5
```

The quiet payoff: because every event already carries a timestamp, cost and
latency reporting is a script over a file — no new instrumentation, no change to
the harness.

`step.completed` and `tool.completed` are deliberately different events. The
first is about the *checkpoint* (model turns, human decisions, and whole
investigations are checkpointed too); the second is about a tool actually
running.

### 2 · Durable execution

The problem is not "remember the conversation". It is: the tool deleted the
file, and the process died before anything was written down. Re-run the task and
it deletes again.

A workflow is a JSON file. A step is a named unit of work whose result is
checkpointed the instant it finishes. To recover you simply **re-run the
workflow body** — completed steps return their cached result without executing
(no model call, no side effect, no cost) and execution races forward to exactly
where it died. There is no separate recovery path; resuming *is* running.

```go
result, err := durable.Step(wf, "tool-"+call.ID, func() (string, error) {
    return a.tools.Dispatch(ctx, call.Name, call.Args), nil
})
```

One rule worth its own line: **a failed step is never checkpointed.** Pinning a
failure would make the crash permanent, and retrying the workflow has to mean
retrying the thing that broke.

### 3 · Sandboxing and code mode

The better reason to care about code execution is that sometimes the best thing
an agent can do is write code. Chaining tools to count something means every
intermediate result round-trips through the model. Code mode flips it: hand the
model the tools as an API and let it write one program.

That is *why* the sandbox exists. A `run_code` program gets a throwaway
directory, a killed process group, a wall-clock timeout, an output cap, and an
environment stripped to `PATH`/`HOME`/`LANG` — so a program that cannot see a
credential cannot leak one. It reaches the agent's read-only tools over a unix
socket in its own scratch dir:

```python
import agent_tools
text = agent_tools.call("read_file", path="go.mod")
print(sum(1 for line in text.splitlines() if "require" in line))
```

Stated plainly: this stops accidents, runaway loops, and casual exfiltration. It
is **not** a boundary against a determined attacker — that needs a container or a
disposable micro-VM. What the harness owes you is the same either way, and is the
actual lesson: every dangerous capability goes through one door, so hardening
later means changing one file.

### 4 · Memory and context hydration

Three different things, kept apart on purpose:

| | |
|---|---|
| **History** | Everything that happened. Durable, in the event log. *Never sent to the model wholesale.* |
| **State** | A compact running summary of older turns. Concrete facts survive: paths, commands, values. |
| **Context** | What the model sees *this turn* — assembled fresh from system prompt + pinned goal + summary + recent turns. |

Compaction is triggered by the assembled context passing a **token budget**, not
by a turn count — one turn where the model batched six tool calls is worth more
than five chatty ones, and a turn count cannot tell them apart. It cuts on whole
turns, so a tool result is never separated from the call that produced it, and
it never touches the most recent turn, because that is what a follow-up refers
to.

> Context is a runtime decision, not a chat log.

### 5 · Routing and handoffs

The honest answer most multi-agent material skips: **usually you don't need
this.** One capable model with good tools is a generalist. A handoff earns its
keep when the other agent is genuinely different — and the case that always
qualifies is least privilege.

```
assistant  read_file · get_weather · openrouter_web_search · diagram×4 ·
           run_code · investigate · handoff
operator   read_file · write_file · edit_file · delete_file · run_command · run_code
```

The assistant cannot be *talked into* deleting a file, because there is no
`delete_file` in its list to call. It hands over instead, and the switch is
recorded in the workflow so a crash resumes as the operator. Run with `-v` to
see the roster printed at startup.

### 6 · Supervision

Same honesty: you usually don't need this either. It earns its keep on three
specific wins — **context isolation** (each investigator works in its own window
and returns a short finding, so the parent never drowns in three subjects' worth
of tool output), **parallelism**, and **synthesis that survives partial
failure**.

Four phases: plan → dispatch → fan in → synthesize. The plan is a first-class
artifact — emitted and read by synthesis, not left as ephemeral reasoning — and
investigators are strictly read-only, which is what lets a whole investigation
be one durable step that is safe to replay. A failed investigator becomes a
finding with an error on it, and synthesis is told to write around the gap:

```
🗺 plan.created        name=3 tasks
├ subagent.started    name=weather-tehran
├ subagent.started    name=weather-oslo
├ subagent.started    name=go.mod
✓ subagent.completed  name=weather-tehran result=38.1°C, wind 14.3 km/h
```

Whether it actually makes your agent better is a question for evals, not for an
architecture diagram.

### 7 · Human-in-the-loop

Where the whole thing pays off. The naive version is one line with three bugs:

```go
approved := approver.Confirm(action)  // 1. holds the process open for the whole wait
                                      // 2. dies on restart — the question is gone
                                      // 3. a human might answer in 30 seconds or 3 days
```

The fix needs no new machinery, because Part 2 already built it: **a human
decision is just another checkpointed step**, one whose value comes from a
person rather than a function.

The interactive y/n is kept — a human who *is* sitting there shouldn't have to
run a second command — but it is now the fast path into that same step. And the
front-ends can now tell "the human said no" from "no human answered", which used
to be indistinguishable: a closed browser tab silently denied the action.
Only a click or a keystroke is an answer. Everything else parks the run.

```
$ go run .
you> write "hello harness" to /tmp/demo.txt
  ↪ agent.handoff       from=assistant to=operator
⚠️  Approve this action?
    WRITE file "/tmp/demo.txt" (13 bytes)
    [y/N]:
(no answer — parking this for later)
⏸  awaiting approval: WRITE file "/tmp/demo.txt" (13 bytes)
   go run . -approve acca4cb5      (or -deny acca4cb5)

$ # the process EXITED. nothing is running. nothing was written.
$ # hours or days pass. the server can reboot; it changes nothing.

$ go run . -approve acca4cb5
  ⟲ workflow.resumed    wf=acca4cb5 agent=operator
  ⏩ step.cached         wf=acca4cb5 name=model-00        # replayed, not re-billed
  ⏩ step.cached         wf=acca4cb5 name=tool-call_2D4S  # the handoff, replayed
  ↪ agent.handoff       wf=acca4cb5 from=assistant to=operator
  ⏩ step.cached         wf=acca4cb5 name=model-01
  🖊 approval.resolved   wf=acca4cb5 approved=true
  ✓ tool.completed      wf=acca4cb5 name=write_file result=Wrote /tmp/demo.txt
  ✔ workflow.completed  wf=acca4cb5
```

Say `-deny` instead and the model is told a human refused — so it explains that
the change needs manual review rather than retrying blindly, and nothing is
written.

Two details that are easy to get wrong and worth stating:

- **A park must not be checkpointed.** When no one answers, the gate returns
  false and the tool politely reports "denied" — a perfectly good string, and
  caching it would be a disaster: the resumed run would replay that refusal
  forever. The park travels as an *error* instead, so `durable.Step` declines to
  cache anything and the replay reaches the same call again.
- **Model turns are keyed by position alone**, not by which agent is speaking.
  Folding in the agent name reads like extra safety and is the opposite — a
  handoff happens mid-run, so the names stop matching on replay, the run
  diverges, and the symptom is a resumed workflow asking for the same approval
  twice. `TestHandoffThenParkResumesOntoTheSameCall` is the regression test.

## The requirements → where they live

| # | Requirement | Where |
|---|---|---|
| 1 | Answer from own knowledge (no tool) | `agent.SystemPrompt` + loop returns when there are no tool calls — `agent/agent.go` |
| 2 | Search the web | `NativeWebSearch` — OpenRouter's own `web` plugin — `tools/nativesearch.go` |
| 3 | Write scripts and run them | `WriteFile` + `RunCommand` + `ReadFile` — `tools/files.go`, `tools/shell.go` |
| 4 | Edit existing files | `ReadFile` + `EditFile` — `tools/files.go` |
| 5 | Human-in-the-loop before danger | `Approver` gate on write/edit/delete/run — `tools/*.go`, `ui/console.go`, `web/session.go`; made durable by `approval/` |
| 6 | Eval suite | `tools/tools_test.go` + `agent/eval_test.go` + `agent/eval_single_test.go` |
| 7 | Draw a diagram from a prompt | `GenerateDiagram` → `canvas.svg` + `canvas.excalidraw` — `tools/diagram/` |
| 8 | Edit it in place | `AddElements` / `UpdateElements` / `RemoveElements` — `tools/diagram/crud.go` |

## Beyond the six

- **`generate_diagram`** — draw anything visual in one call from a prompt like
  *"Draw a flowchart of user signup"*: twelve shapes, tables, and bar/line/pie
  charts, mixed freely on one canvas. Writes two files from one layout:
  `canvas.svg` (open in a browser, refresh after each redraw) and
  `canvas.excalidraw` (edit it by hand in the browser UI, or at
  [excalidraw.com](https://excalidraw.com)). The model
  passes only structure — boxes with ids and labels, arrows referencing those
  ids — in a single `elements` array; **it never passes coordinates**, because
  asking a model for pixel geometry reliably produces overlapping boxes and
  crossed arrows. Placement is the tool's job — see **Diagram layout** below —
  `tools/diagram.go`.
- **`add_elements` / `update_elements` / `remove_elements`** — focused CRUD over
  the diagram already on the canvas, so an edit never means redrawing the whole
  picture. Boxes are addressed by their id, arrows by `"from->to"`
  (`"validate->create"`), since arrows have no ids of their own. See **Editing a
  diagram** below — `tools/diagram/crud.go`.
- **`get_weather`** — current temperature and wind for a place, via Open-Meteo's
  free, keyless APIs (geocode the name, then fetch conditions). Read-only, no
  approval — `tools/weather.go`.
- **Browser UI** — `go run . -http :8080` serves the same agent as a chat page:
  tool calls appear as they run, dangerous ones stop for an Approve/Deny click,
  and a diagram the agent draws shows up next to the chat as a live Excalidraw
  canvas you can drag boxes around in. See **Browser UI** below — `web/`.
- **Thinking spinner** — a turn can take several model round trips, so the
  console animates a one-line indicator while it waits instead of leaving the
  terminal dead. Everything that prints mid-turn (tool lines, compaction
  notices, the approval prompt) stops the spinner first and restarts it after,
  so a frame never lands on top of real output. By default it draws only to a
  terminal, so a pipe or a test buffer gets nothing and captured output stays
  free of carriage-return noise. **An IDE run window (GoLand's Run tool window,
  VS Code's debug console) is a pipe, not a terminal, so the spinner disables
  itself there** — set `AGENT_SPINNER=1` to force it on (or `=0` to force it
  off anywhere) — `ui/spinner.go`, `ui/console.go`.
- **Bounded context** — the agent keeps its full history but never sends it. Each
  turn it *assembles* a context (system prompt + pinned goal + summary + recent
  turns) and, when that assembly passes a token budget, folds the oldest whole
  turns into the summary. Tokens sent per turn stop growing. It runs only at a
  safe boundary (after a final answer, so no tool call is left awaiting its
  result) and is best-effort: if summarizing fails, the turns go back untouched,
  because a lost summary costs tokens and a lost turn costs the conversation —
  `agent/memory.go`. See **The harness · 4**.
- **`run_code`** — code mode: the model writes one program instead of chaining
  six tool calls, and it runs in the sandbox with the read-only tools available
  over a socket. See **The harness · 3** — `tools/code.go`, `sandbox/`.
- **`investigate`** — fans a multi-part question out to read-only sub-agents that
  each research in their own context, then writes up what came back, naming any
  investigation that failed. See **The harness · 6** — `agent/supervisor.go`.

## How the loop works (`agent/agent.go`)

`Agent.Ask` runs `think → runTools` until the model replies with no tool calls.
Each line below names the part that owns it:

```
workflow := store.Open(id, task)        // 2 · durable state
for {
    context := memory.Context(working)  // 4 · assembled fresh, bounded
    reply   := durable.Step(model)      // 1, 5 · the LLM decides
    gate.Scope(call.ID)                 // 7 · approval as a durable state
    result  := durable.Step(tool)       // 3 · sandbox · resume from exactly here
    if handoff { switch specialist }    // 5 · lateral control transfer
}
```

1. **think** — assemble the context, send it with the current agent's tool specs,
   and checkpoint the reply. On a replay this returns from disk, which is what
   makes the tool call ids stable across a crash.
2. If the reply has no tool calls, it's the final answer — commit the turn to
   memory, compact if over budget, return.
3. **runTools** — run each requested tool as its own checkpointed step, append
   results as `role: tool` messages, and loop (capped at `maxSteps`). A handoff
   result swaps the prompt and the toolset; a park unwinds the whole run.

Every harness service is **optional**. With no store the loop is an ordinary
in-memory one — brittle, and honest about it — which is exactly what keeps the
eval suite and the unit tests from having to stand up a runtime to test a
prompt. `agent.New(...)` alone is the pre-harness agent; the `With*` methods add
each part.

The model never runs code itself — it only *asks*. A dangerous tool first calls
`Approver.Confirm`, which is now the durable gate.

## Browser UI (`web/`)

```sh
go run . -http :8080     # then open http://localhost:8080
```

One page, embedded in the binary with `go:embed` — no build step and no CDN. The
single exception is the diagram canvas below, which is Excalidraw itself, served
from the same `npm install` the renderer uses and fetched only when a diagram is
shown. `web.Session` is to the browser what `ui.Console` is to the terminal: it
is the approver the tools ask, and the hooks the agent reports progress to.

- **A turn is a stream.** `POST /api/chat` answers with server-sent events and
  stays open until the turn ends, so tool calls, compaction notices and approval
  requests appear *as they happen* rather than all at once at the end — the same
  reason the terminal has a spinner.
- **Approval is a real block.** When a gated tool calls `Confirm`, the page shows
  an Approve/Deny card and the tool sits there, parked, until the click arrives
  on `POST /api/approve` — a separate request, because the turn's own response is
  busy being a stream. Anything that isn't a real yes denies: a closed tab, a
  browser that hung up, or five minutes of silence.
- **One conversation per browser.** A cookie names the session; each has its own
  agent, and therefore its own history. A second question arriving mid-turn is
  refused with `409` rather than queued — the agent rewrites its history as it
  works, so turns must not overlap. Idle conversations are dropped after two
  hours.
- **The diagram is a canvas, not a picture.** The panel is Excalidraw itself,
  open on `canvas.excalidraw`, so a box the model put in the wrong place is
  dragged rather than re-prompted — the same drag, select and retype you'd get on
  excalidraw.com. **Save** writes both files back: the scene, so the next edit
  starts where this one stopped, and the SVG, exported by Excalidraw in the
  browser, so everything pointed at `canvas.svg` shows the edited drawing.
  **Picture** is the way back to that flat SVG, shown in an `<img>` where it
  cannot run script. See [The canvas](#the-canvas).
- **It follows the agent, until you touch it.** Tools that redraw flag their
  event; an untouched canvas reloads itself, so "move the database box down"
  updates what you're looking at. A canvas with unsaved edits is left alone, and
  offers **Reload** for when you're ready to take the agent's version.
- **New chat** rebuilds the session's agent with an empty history — the
  browser's version of quitting the CLI and starting it again.

## Tests

```sh
go test ./...            # everything (also live web search + a real model call)
go test ./... -short     # fast, offline, deterministic (no key, no network)
```

- **`ui` package** — spinner unit tests: it animates and clears, stays silent
  off a terminal, and the console pauses it before printing mid-turn.
- **`web` package** — the browser front-end end to end over a real HTTP server,
  with a scripted stand-in for the agent, so no key and no network: a turn
  streams its tool calls and ends with the answer, a failed turn streams the
  error, an approval blocks the tool until the click and hands back exactly what
  was clicked, a stale or unwatched approval denies, an overlapping question is
  refused, browsers don't share a history but follow-ups do, and the canvas is
  served only once something has been drawn. The editor half is covered too: a
  save writes both files, junk that isn't a scene or isn't an SVG bounces with
  nothing written, a save mid-turn is refused, the routes vanish without a built
  bundle, the asset route can't be walked out of its directory, and a save
  replaces a file whole rather than in place.
- **`tools` package** — unit evals: script roundtrip, edit, denial blocks the
  action, read-only tools never prompt, unknown tool handled, live web search,
  the diagram tool (valid SVG on disk, no overlap, cycles, bad input), the
  Excalidraw scene (envelope, element counts, bindings resolve, determinism),
  and the CRUD tools (batch add/update/remove, atomic rollback on a breaking
  batch, refusal to cascade a delete, edits composing across calls, spec
  round-trips to the same drawing).
- **`durable` package** — the claim the package exists to make, measured: a
  crashed run resumes without re-executing a single completed step, a failed step
  is *not* checkpointed so it can be retried, state survives through a brand new
  Store the way a second process would see it, and a corrupt workflow file fails
  loudly rather than silently starting over and repeating every side effect.
- **`approval` package** — a human is asked exactly once and every replay reads
  the answer off disk; nobody answering parks the run and records *nothing*; a
  refusal is a decision, so it is checkpointed and does not park; a decision left
  by the CLI is consumed once and cannot approve a second action; and a different
  action under the same call id asks again rather than reusing the yes.
- **`sandbox` package** — output is captured and capped, a runaway loop is killed
  on time, a backgrounded child dies with the process group, a crash comes back
  as a *result* the model can reason about rather than an error, `OPENROUTER_API_KEY`
  is provably absent inside, the tool bridge serves the allowlist and refuses
  everything else, and a **relative** scratch dir works — the last being a
  regression test for a bug every other test in the file was blind to, because
  they all used absolute `t.TempDir()` paths and production does not.
- **`events` package** — the JSONL log is append-only and every line parses with a
  timestamp, an unanswered request is distinguishable from a refusal, a panicking
  sink cannot take the others down with it, and a multi-line tool result still
  renders as exactly one line.
- **`agent` package** — the harness end to end against a scripted model over a
  local HTTP server, so the runtime is deterministic even though the model isn't:
  - *durability* (`harness_test.go`) — a crash after the irreversible action does
    not repeat it on resume, and only the *uncached* turn costs a model call. The
    same loop with no store repeats everything, which is the point.
  - *approval* — a parked run resumes onto the same call after the human decides,
    performing the gated action exactly once.
  - *routing* — a handoff swaps the toolset and survives a crash; an invented
    agent name is ignored, because the roster is the authority, not the model.
  - *supervision* (`supervisor_test.go`) — a failed investigator degrades into an
    honest gap in the write-up instead of an error, raw sub-agent tool output
    never reaches the parent's context, and an unusable plan falls back to one task.
  - *memory* (`memory_test.go`) — compaction cuts only on turn boundaries (a tool
    result is never orphaned from its call), never touches the most recent turn,
    and tool-call ids survive translation to the SDK's message union.
- **`agent` package, live evals** (skipped with `-short` or without a key):
  - *behavioral* (`eval_test.go`) — whole tasks through the real model, graded
    on which tools it chose, its answer, and the actual side effects on disk.
  - *tool selection* (`eval_single_test.go`) — one-shot: does the model pick the
    right tool, with the right arguments, on the first step? Never executes.
  - *context budget* (`compact_smoke_test.go`) — asks five questions under an
    absurdly small budget and checks that the context stops growing, that the
    older turns survive as a summary rather than being dropped, and that the last
    question is still there verbatim.

  The behavioral and tool-selection evals print a scorecard.

## Diagram layout (`tools/diagram.go`)

The model says *what* connects to what; the tool decides *where* everything
goes, then writes one self-contained SVG.

1. **Layer** each box by its longest path from a starting box, so a flowchart
   reads top to bottom. Back edges — the "invalid input, go back to the form"
   arrow — are found by a depth-first walk and excluded from this step. They're
   still drawn; they just don't get a vote on depth. Letting them vote stretched
   an 8-box signup chart across 15 rows of mostly empty canvas.
2. **Place** each layer as a centred row, boxes sized around their wrapped
   labels, so nothing overlaps by construction.
3. **Route** forward arrows bottom-to-top as bezier curves; back edges and
   same-row hops bulge out to the right so they stay visible instead of cutting
   through the boxes between.

Shapes follow flowchart convention: `ellipse` for start/end, `diamond` for a
decision, `box` for a step. The SVG has no external references and carries a
`prefers-color-scheme` block, so it renders standalone in light or dark mode.

### Editing a diagram

Three properties shape all three CRUD tools:

- **Additive** — each changes only what it names. Nothing is regenerated, so an
  edit can't quietly restyle or relabel the rest of the picture.
- **Batch** — each takes a list. Real edits arrive in groups ("add the retry
  path" is a box and two arrows), and three round trips to add three elements is
  three chances for the model to drift.
- **Explicit** — nothing cascades. Removing a box that still has arrows on it is
  an error *naming those arrows*, not a silent deletion of edges the caller never
  mentioned.

All three are atomic: the edit is applied to a clone, the redraw is attempted,
and only then is anything written. A batch that would break the diagram leaves
the canvas and the saved spec untouched — no half-applied edits.

The explicitness earns its keep in practice. Asked to remove a captcha step, the
model first tried the box alone:

```
remove_elements({"ids":["captcha"]})
  -> error: that would leave "input->captcha", "captcha->validate" pointing at a
     box that no longer exists, so nothing was removed; name them in the same call
remove_elements({"ids":["captcha","input->captcha","captcha->validate"]})
  -> Removed 3 elements. Redrew 5 boxes and 4 arrows.
```

A cascading delete would have silently dropped two arrows the user never asked
about. The error is actionable enough that the model fixed it in one retry.

### The spec file

`generate_diagram` also writes `canvas.diagram.json`: the elements it drew from.
That file is what makes the CRUD tools possible — without it, changing one
label would mean handing the tool the entire diagram again, which is just
`generate_diagram` with extra steps. Both tools render through the same path, so
a modified diagram is identical to one drawn from scratch with the same
elements, and the spec is saved last, only after both renders succeed, so what's
on disk always describes the picture that's actually there.

### Drawn by Excalidraw

`canvas.svg` and `canvas.excalidraw` are produced by **Excalidraw itself** —
`convertToExcalidrawElements()` and `exportToSvg()` from `@excalidraw/excalidraw`
— so the stroke, the fonts, the arrow routing and the label placement are the
library's, not an imitation of them.

Excalidraw is a browser library and this is a Go program, so it runs as a Node
sidecar in `excalidraw/`. One-time setup:

```sh
cd excalidraw && npm install     # installs deps and builds both bundles
npm run smoke                          # optional: proves it renders
```

`npm install` builds two bundles from the same packages: `excalidraw-bundle.cjs`,
the rendering half loaded by the Node sidecar, and `editor-bundle.js` + `.css`,
the whole app for the browser UI's [live canvas](#the-canvas).

The sidecar is found from any working directory, in this order:

1. `$AGENT_EXCALIDRAW_RENDERER` — a path to `render.cjs`, or `off`.
2. Beside the source the binary was compiled from.
3. Beside the working directory: `excalidraw/render.cjs` at each level up, and
   `agent/excalidraw/render.cjs` too, so a checkout that nests the agent one
   level down is still found.

Source-relative comes before the working directory on purpose: it resolves to
the binary's *own* sidecar, so a binary built from one checkout can't end up
rendering with another checkout's build just because of where it was launched.

**Without a sidecar the built-in Go renderers draw instead** — a missing
optional npm package should cost fidelity, not the feature. Set
`AGENT_EXCALIDRAW_RENDERER=off` to force that path, and check which one drew a
given file with `grep -c svg-source:excalidraw canvas.svg`.

Getting Excalidraw to run headless takes a deliberate environment, and each
piece is there because the bundle fails without it: jsdom for the DOM (touched
at module scope, not just at call time), the native `canvas` package (jsdom's
`getContext("2d")` returns null otherwise, and the bundle evaluates
`"filter" in ctx` while loading), a bare `devicePixelRatio` global, and stubs
for `FontFace`/`document.fonts`. The bundle is built with esbuild because
Excalidraw's dist imports `roughjs/bin/rough`, which Node's ESM resolver rejects
for having no extension.

Three defects found by looking at the output rather than trusting it:

- **`exportToSvg` under jsdom emits `xmlns` twice** on the root element. That is
  not well-formed XML, so a browser shows "Attribute xmlns redefined" instead of
  the drawing — fatal for a file whose whole purpose is being opened in a
  browser. The sidecar de-duplicates root attributes.
- **Excalidraw binds arrows only to rectangle/ellipse/diamond.** Naming a line
  as an endpoint — which every custom shape is — crashed
  `convertToExcalidrawElements` with "Unhandled element start type undefined".
  Arrows to line shapes, tables and charts are left unbound; they still draw.
- **With `textAlign: center`, `x` is the centre anchor**, not the left edge.
  Subtracting an estimated half-width shifted every free label off by exactly
  that estimate.

### The canvas

`go run . -http :8080` shows the diagram next to the chat as a **live Excalidraw
canvas** — drag a box, retype a label, restyle the lot, exactly as on
excalidraw.com. Nothing here reimplements a drawing tool: it is the same package
the sidecar renders with, bundled for the browser by `excalidraw/editor.jsx` and
opened on `canvas.excalidraw`.

| Route | |
|---|---|
| `GET /canvas.excalidraw` | the scene the canvas opens |
| `POST /api/canvas` | the scene and the SVG, written back |
| `GET /editor/editor-bundle.js`, `.css` | the built editor |
| `GET /editor/assets/…` | Excalidraw's hand-drawn fonts, from `node_modules` |

- **Both halves are saved.** Saving writes `canvas.excalidraw` and re-exports
  `canvas.svg`, so the picture and the editable original never describe two
  different diagrams. The SVG is exported by Excalidraw *in the browser* — the
  elements are already there, and a Node round trip per save would only add
  latency.
- **The agent's redraws land on it.** A clean canvas reloads itself when a
  diagram tool runs; one with unsaved edits keeps them and offers **Reload**.
  Whichever way, the choice between your drawing and the agent's is made by
  clicking, never silently.
- **Offline like everything else.** The fonts come from the installed package,
  not from Excalidraw's CDN default, and the app is fetched the first time a
  diagram is shown rather than on page load — it is several megabytes, and most
  sessions never draw one.
- **Optional, like the sidecar.** No `npm install`, no `editor-bundle.js`, no
  live canvas: the panel falls back to the picture it always was.
- **A save waits for the agent.** It takes the same per-conversation lock a
  question does, so a hand edit can't land in the middle of a redraw; mid-turn it
  is refused with `409` and the page says why.
- **Hand edits and the CRUD tools don't merge.** `add_elements` and friends
  redraw from `canvas.diagram.json` — the spec the *model* drew from, which knows
  nothing about a box you moved. After editing by hand, asking the agent to
  change the same diagram replaces your edits with a fresh drawing (which is why
  a dirty canvas asks before reloading). Ask it for the diagram you want first,
  then move things; or move things, then keep going by hand.

### Shapes, tables and charts

Twelve shapes, chosen by meaning rather than geometry: `ellipse` start/end,
`diamond` decision, `cylinder` database, `parallelogram` input/output,
`document` a report, `note` an annotation, `cloud` an external service,
`hexagon` preparation, plus `circle`, `triangle`, `pill` and the default `box`.
Domain words map to the right shape — `"database"`, `"decision"`, `"input"` all
work — because that is what a model actually writes. They are coloured by role
(green terminators, amber branches, violet data), so a diagram reads at a glance
without the caller specifying styling.

**Tables** take `columns` and `rows`; column widths are measured from the widest
cell so nothing is clipped. **Charts** take `chart` (`bar` / `line` / `pie`) and
`data` as label/value pairs.

Tables and charts are *nodes*, not a separate mode: they get laid out in the
flow, arrows can point at them, and they can also stand alone — the
"unconnected boxes" guard is a flowchart rule and doesn't fire for them.

Two things learned from watching a real model call this:

- It writes `{"type":"cylinder"}` as often as `{"type":"box","shape":"cylinder"}`.
  Read literally the first is an unknown type, and the element used to degrade
  to a plain rectangle with no error — the caller asked for a database and got a
  box. A shape word in the `type` field is now recognised.
- A shape must fill the box it is measured against. The first cloud traced its
  arcs through only the lower band of its bounding box, so a centred label sat
  outside the outline.

### Two files, one layout

Both outputs come from the same laid-out nodes and edges, so they can never
disagree about the picture — only the serialization differs.

| | `canvas.svg` | `canvas.excalidraw` |
|---|---|---|
| Open with | any browser | the browser UI's canvas, or excalidraw.com (File → Open, or drag onto the canvas) |
| Update loop | **refresh the page** | re-import after a redraw |
| Editable | no | yes — drag boxes, retype labels, restyle |

The scene is authored as **Excalidraw element skeletons** — the format
documented at
[ExcalidrawElementSkeleton](https://docs.excalidraw.com/docs/@excalidraw/excalidraw/api/excalidraw-element-skeleton).
A skeleton says *what* to draw; everything mechanical is derived:

```go
{Type: "rectangle", X: x, Y: y, Width: w, Height: h,
 Label: &skeletonLabel{Text: "Create account"}}          // bound text, derived
{Type: "arrow", Start: &skeletonBinding{ID: "validate"},
 End: &skeletonBinding{ID: "create"},
 Label: &skeletonLabel{Text: "yes"}}                     // bindings, derived
```

Excalidraw ships `convertToExcalidrawElements()` to do that derivation. It is
JavaScript, and this is a Go program, so calling it is not an option — the tool
has to hand you a file that opens directly, not a skeleton plus instructions to
run npm. So the skeleton *format* is what this package authors, and the
conversion documented on that page is implemented in `diagram/skeleton.go`. It
derives three things, each of which used to be hand-wired:

1. **Ids**, when absent — stable rather than regenerated, so redrawing the same
   diagram is byte-identical instead of looking like a brand new scene.
2. **Bound labels** — the text element with its `containerId`, and the
   container's matching `boundElements` entry. Getting one half wrong is what
   makes labels vanish on import.
3. **Two-way arrow bindings** — `startBinding`/`endBinding` on the arrow *and*
   the arrow listed on both targets. Without the second half the arrow detaches
   the first time you drag a shape.

Adopting the format fixed a real defect on the way: edge labels used to be
emitted as loose text at an arrow's midpoint, which drifted away the moment
anything moved. A labelled arrow is a first-class skeleton, so `yes`/`no` now
binds to the arrow and travels with it.

The drawing is also written to `canvas.skeleton.json` in the authoring form, so
the same diagram can be fed to the real function in a JS project:

```js
import { convertToExcalidrawElements } from "@excalidraw/excalidraw";
convertToExcalidrawElements(JSON.parse(fs.readFileSync("canvas.skeleton.json")));
```

Nothing is uploaded: the file is written locally and you open it yourself. The
tool never talks to excalidraw.com.

Two things the tool is deliberately strict about, both learned from watching a
real model call it:

- It also reads a top-level `arrows` array. Models reach for one even though the
  schema says otherwise, and silently dropping those arrows produced a page of
  unconnected boxes *reported as a success*.
- More than one box with no arrows at all is rejected rather than drawn, for the
  same reason: a disconnected diagram is nearly always a caller mistake, and
  failing tells the model to fix it.

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
