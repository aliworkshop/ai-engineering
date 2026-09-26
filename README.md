# The durable agent (Go) — crash it mid-task and nothing happens twice

> **Branch `session-7`.** Built up from nothing, one step at a time. Two
> dependencies and nothing clever. The other branches are finished agents:
> `session-6` is support triage on a full harness, `session-5` an English
> teacher, `session-1` the loop with tools.
>
> **Steps 1–4** built the loop: a model, one tool, a system prompt, clean
> layers. **Step 5** was [part 1 — the brittle
> agent](https://github.com/pjsofts/ai-engineering-1/tree/main/week3-workshop/part1-brittle):
> every move an event, and two failures staged on purpose. **Step 6** fixed the
> first with [durable
> execution](https://github.com/pjsofts/ai-engineering-1/tree/main/week3-workshop/part2-durable)
> — first hand-rolled over JSON files, now on
> [DBOS Transact](https://docs.dbos.dev/), which is what runs here.
>
> The forty-line version is still in the history, and building it first is the
> only reason the rest of this reads as obvious. `git log` for
> `internal/durable`.

This is a **support triage agent**. Give it work items and it classifies each
one, reads the knowledge base, drafts a reply, and sends it. Three of those
tools are harmless. The fourth emails the customer.

```sh
# needs OPENROUTER_API_KEY, and a Postgres for durability
export DBOS_SYSTEM_DATABASE_URL="postgresql://postgres:secret@localhost:5432/agent_dbos?sslmode=disable"

go run . -sample          # the three sample items, one shot
go run .                  # recover anything that crashed, then talk to it
go run . -recover         # launch and do nothing else; watch it finish itself
go run . -inspect [id]    # the engine's own receipts, out of Postgres
go run . -audit           # did any work happen twice?
go run . -crash-at 2      # die mid-task, after real side effects
```

Leave `DBOS_SYSTEM_DATABASE_URL` unset and the same loop still runs — in
memory, checkpointing nothing, and saying so on the first line. The agent
works on a laptop with no database; it just cannot survive a crash there.

```
  ▶  workflow.started      6db483e3  {"input":"Handle these work items:…"}
  ⚙  tool.requested        6db483e3  {"name":"classifyItem","call":"call_MCa2…
  ✓  tool.completed        6db483e3  {"name":"classifyItem","call":"call_MCa2…
  ⚙  tool.requested        6db483e3  {"name":"sendReply","call":"call_lvcYHV8…
  ✓  tool.completed        6db483e3  {"name":"sendReply","call":"call_lvcYHV8…
  ✔  workflow.completed    6db483e3  {"output":"I processed three work items…"}
```

## One failure fixed, one to go

Part 1 staged two failures deliberately. This part answers the first.

| | part 1 | now |
|---|---|---|
| **State is a slice** | kill the process and the conversation is gone; start again and everything runs a second time | a workflow is a file, every model turn and tool call is a checkpointed step, and a resumed run replays them from disk |
| **`sendReply` is unmediated** | runs the instant the model asks: no policy, no human, no record | **still true.** There is now a *record* — the event log — but nothing in the way |

So the remaining hole is narrower and sharper: the agent can no longer email
a customer *twice by accident*. It can still email them *once without asking*.
That is what part 7's approval gate is for.

## Durable execution (`internal/runtime`)

A workflow is a task. A **step** is a named unit of work inside it whose result
is checkpointed the instant it finishes. To recover, the body is simply re-run:
completed steps return their stored result without executing — no model call,
no side effect, no cost — and execution races forward to exactly where it died.

There is no separate recovery path. **Resuming is running.**

That only works if the body is *deterministic*, which is the golden rule of
durable workflows: everything non-deterministic — the model call, the tool
call, the clock — happens inside a step, and everything outside one is rebuilt
identically on replay. DBOS enforces it harder than a hand-rolled engine does:
steps are matched by **position**, not by name, so the body must issue the same
steps in the same order every time.

### One loop, two steppers

The loop does not branch on durability. It asks a `stepper` to run each named
unit of work, and there are two of them:

```go
type stepper interface {
    id() string
    msg(name string, fn func(context.Context) (agent.Msg, error)) (agent.Msg, error)
    text(name string, fn func(context.Context) (string, error)) (string, error)
}
```

`dbosSteps` checkpoints into Postgres. `plainSteps` runs the work and hands it
back. Same body, same steps, same order — what changes is only whether
finishing a step writes anything down, and therefore whether a crash costs a
replay or a repeat.

Two methods rather than one generic one because Go has no type parameters on
interface methods, and the loop only ever checkpoints two things: a model turn
and a tool result.

### Why the loop lives here and not in `agent`

DBOS can only recover a workflow that is a **plain package-level function it
can find by name**. Recovery happens inside `Launch`, with no caller in sight,
so the body cannot be a method and cannot close over a client passed in at call
time — which is why its dependencies are a package-level var. It reads like a
global because it is one; the alternative is a workflow the engine cannot
resurrect.

So `internal/agent` keeps what the agent *is* — `Msg`, `ToolBox`, the prompt,
and `Think` — and `internal/runtime` owns how it runs.

## Watch it crash

```sh
go run . -sample -crash-at 2
```

```
  ▪  step.completed        bb657985  {"name":"tool-call_ViS34Cf…"}
  ▪  step.completed        bb657985  {"name":"tool-call_3EGOyEB…"}
  💥 simulated crash before step 2 — the process is gone.
```

Eight steps had completed. Real tool calls ran. The process is dead, and the
workflow is sitting in Postgres marked `PENDING`.

Now start the agent again with **no task and no workflow id**:

```sh
go run . -recover        # or just `go run .`, which recovers and then talks
```

```
launched — DBOS is recovering anything left PENDING (waiting 1m0s)
  ⚙  tool.requested        6625d36d…  {"name":"draftReply",…}
  ✓  tool.completed        6625d36d…  {"name":"draftReply",…}
  ⚙  tool.requested        6625d36d…  {"name":"sendReply",…}
  ✔  workflow.completed    6625d36d…  {"output":"I processed the work items…"}
```

Nothing was asked of you. Connecting *is* recovering: `Launch` finds every
`PENDING` workflow, resumes it from the exact step where the process died, and
the drafts and the sends go out. The eight pre-crash steps are not in that
output because they did not happen again — see `-inspect` below.

## Proving it: `-audit` and `-inspect`

Watching it recover is not the same as knowing nothing ran twice. There are two
records, and they answer different questions.

**`-audit` reads our own event log.** The measurement is `tool.requested`
grouped by the model's own **call id**, and it works because of where that
event is emitted: *inside* the step. A replay does not re-run the step, so it
does not re-emit it — one line per call id means the tool ran once, ever.

```
.harness/events.jsonl — 112 events across 4 workflows

  tool calls executed    26
  repeated side effects  0  ✔ nothing ran twice
```

A tool **name** repeating is normal: three items, three `sendReply` calls. A
**call** repeating is a customer who got two emails.

**`-inspect` asks the engine.** DBOS already recorded every step's name, output
and duration, because it needed them in order to replay:

```
  #    STEP                                     MS       OUTPUT
  0    started                                  3        ""
  7    tool-call_BgTXl6gqqfa3VACeM4QTbX8J       2        "{\"articles\":[\"The Safari export failure…
  8    tool-call_oDTE7UKYScx5c3J1p17EFl1e       2        "{\"articles\":[\"Team plans are $20/seat…
  ---- 7s gap: the process was dead here; everything above was replayed from Postgres ----
  9    model-02                                 3309     {"role":"assistant","tool_calls":[…
```

That gap line is the whole lesson in one row. In a live run consecutive steps
are *milliseconds* apart — everything slow is itself a step, so it sits inside
the window rather than between two. A gap you can see is a gap where the
program was not running.

One honest cost of handing durability to a real engine: **the replay is
invisible in our own stream.** The hand-rolled store emitted `⏩ step.cached`
for every step it served from disk, and you could watch recovery scroll past.
DBOS returns the cached value without telling us, so `-inspect` is now where
you go to see what was replayed.

## Everything is an event (`internal/events`)

The harness never prints its feelings. It emits typed `Event` values, and a
sink decides what they look like:

```go
type Emitter interface{ Emit(Event) }
```

There are two sinks now, behind a `Bus` that fans out to both: a `Console`
that renders a glyph, the type, the workflow id and the detail, and a `JSONL`
file that outlives the process. Adding the second one changed nothing else,
which was the whole point of making events values in the first place.

The reason to build it this way is not prettier output. Progress that exists
only as a `fmt.Println` is invisible to everything except a human watching the
screen. An event is a value: it can be counted, written to a file, pushed down
a socket, or read back — and because each line carries a timestamp and a
workflow id, `-audit` is a script over a file rather than new instrumentation.

The glyph table is deliberately wider than what anything emits —
`agent.handoff`, `approval.requested`, `plan.created`, `subagent.*` — because
that map is the roadmap, and a name reserved now cannot be spelled two ways
later.

One `Ask` is one **workflow**, with an eight-character id that every event in
the run carries — and, since part 2, a file on disk that carries the same id.
Two runs interleaved in the log can still be told apart, and a run that died
can still be found.

## The toolbox (`internal/tools/support.go`)

| tool | what it does | dangerous? |
|---|---|---|
| `classifyItem` | files an item under billing / technical / sales / other | no |
| `searchKnowledgeBase` | returns the house answer for a topic | no |
| `draftReply` | writes a reply — sends nothing | no |
| `sendReply` | **emails the customer. Irreversible, and nothing checks first** | **yes** |

Two things about that table are worth more than the code under it.

**Nothing at the call site announces the difference.** `SendReply.Run` looks
exactly like `DraftReply.Run` — decode the arguments, return some JSON. The
danger is not visible where the tool runs, which is why it has to be handled
by the harness around it rather than by whoever is reading the code.

**The description is the interface.** `Spec()` is everything the model knows
about a tool: the name, one sentence, and a JSON Schema. `classifyItem`'s
schema pins `category` to an enum rather than a free string, so the model
cannot invent a fifth. That schema is program text, not documentation.

**Retrieval is four keys and a substring match**, and it misses on purpose
sometimes: an article matches only when the query mentions its topic, so a
search for "charged twice" finds nothing under `billing`. A miss returns *"No
exact match - use your judgment"* rather than an empty list, because a model
handed `[]` quietly invents a policy and one handed a sentence tends to admit
it is unsure. The interesting failure in this scenario is never the ranking.

## Architecture

Dependencies point inward. The terminal knows an assistant; the runtime knows
the agent; the agent knows an abstract tool box; the tools know nothing about
any of it. Nothing inner imports anything outer, so each layer can be tested —
or replaced — on its own.

```
main.go                     wire the pieces together, then run
  └── internal/
        ui/        Console  the REPL. Owns stdin, knows only an Assistant
        runtime/   Runtime  HOW a task runs: the loop, steps, recovery
        agent/              WHAT the agent is: Msg · ToolBox · prompt · Think
        tools/     Registry the Tool interface, and every tool
        events/    Event    the typed stream, console and log
        llm/                the one place the OpenRouter client is built
```

The split between `runtime` and `agent` is the one worth understanding, and it
was forced by the engine rather than chosen for tidiness — see **Why the loop
lives here** above.

`events` sits under everything and imports nothing of ours, so any layer may
emit. Workflow state lives in Postgres; `.harness/` holds only the event log.

`main.go` is now four lines of wiring, outermost last:

```go
bus       := events.Bus{events.NewConsole(os.Stdout), events.NewJSONL(log)}
store, _  := durable.NewStore(harnessPath("wf"), bus)
assistant := agent.New(llm.NewOpenRouter(apiKey), Model, tools.Default()).
                 WithEvents(bus).
                 WithStore(store)

recoverPending(assistant, store)     // recover FIRST, then take new work
ui.New(os.Stdin, os.Stdout).Run(context.Background(), assistant)
```

Three seams carry the whole thing:

- **`tools.Tool`** — one capability: `Spec()` describes it to the model,
  `Run()` does the work. Two methods on one type, so a description cannot
  drift away from the code it describes. Add a capability by writing one
  struct and naming it in `tools.Default`; nothing else changes.
- **`agent.ToolBox`** — what the loop needs from its tools (`Specs`,
  `Dispatch`), declared in the agent package and satisfied by
  `tools.Registry`. The loop names no tool anywhere, so it cannot be broken by
  one.
- **`agent.Msg`** — the conversation in our own vocabulary, not the SDK's. A
  turn is a plain Go value: readable in a debugger, buildable in a test,
  writable to disk later. The SDK's union types live in two translation
  functions at the boundary.

Two more seams: **`events.Emitter`** and **`agent.Store`**. The agent does not
print — it emits — and it does not know what durability is, it just opens a
workflow. Both are optional: a nil emitter is a silent agent, and **a nil
store is the part 1 agent**, still there and still runnable, because the
durability claim means nothing until you can run the other one.

## The loop (`internal/runtime`)

```go
working := []agent.Msg{{system, agent.SystemPrompt}, {user, task}}

for i := 0; i < maxSteps; i++ {
    reply := s.msg(fmt.Sprintf("model-%02d", i), think)   // checkpointed
    working = append(working, reply)
    if len(reply.ToolCalls) == 0 {
        return reply.Text                                 // it answered: done
    }
    for _, call := range reply.ToolCalls {
        s.text("tool-"+call.ID, run)                      // checkpointed
    }
}
```

Three things the loop owns rather than the model:

- **`maxSteps`** (10) — a model that keeps asking for the same tool otherwise
  loops until your credit does.
- **Dispatch always returns a string.** A failure becomes `error: …` in the
  transcript, so the model can try something else. An error that propagated
  would end the conversation — and, here, would be a step that refuses to
  checkpoint.
- **A failed run keeps its completed steps.** The workflow stays in Postgres
  with everything that finished, which is what makes the next attempt cheap:
  it replays what already worked and retries only what broke.

### Two things that will bite you

- **The application version is pinned** (`AppVersion = "session-7"`). By
  default it is a hash of your code, and DBOS only recovers workflows whose
  version matches — so an edit between a crash and the recovery silently
  orphans the parked workflow and `-recover` does nothing at all. In production
  that default is a feature: a bad deploy must not half-replay workflows
  written by different code.
- **The engine's logger is silenced at shutdown, and only then.** Cancelling
  the context makes DBOS's queue runner report its in-flight work as failed —
  `context canceled`, at WARN and ERROR — which is true, dull, and
  indistinguishable from a crash to anyone reading the terminal. Anything
  logged while it is actually running still reaches you.

### What it costs

`go.mod` has three direct dependencies where the pitch was two, and `go list -m
all` reports seventy-odd modules where it used to report a handful. A local
Postgres is now part of running the thing durably at all. The hand-rolled
engine was a directory and a hundred and fifty lines — building that first is
the only reason any of the above reads as obvious rather than as magic.

## The system prompt

Without one you get the model's defaults: a chatty assistant that restates your
question and offers further help. The prompt is how you say otherwise — here,
that the job is a four-step pipeline and that every item gets worked.

```go
const SystemPrompt = `You are a support triage agent.
For each work item the user gives you:
1. Classify it with classifyItem.
2. Search the knowledge base with searchKnowledgeBase if it helps.
3. Draft a reply with draftReply, then send it with sendReply.
Work through every item, then briefly summarize what you did.`
```

Three things about it are worth more than the text itself:

- **It is message zero and it never moves.** The model re-reads it before every
  reply, which is what makes standing instructions stand. It is also therefore
  the most expensive text in the program — sent on every turn, forever — so it
  stays short. Anything that trims this history later has to keep it: an agent
  that compacts away its own instructions forgets what it is mid-conversation.
- **A tool in the list is an invitation.** The prompt and the tool descriptions
  are one system: the description says what a tool is for, the prompt says when
  to reach for it. Line 3 is the only reason `sendReply` ever runs.
- **And line 3 is also the whole problem.** One sentence of English is what
  currently governs an irreversible action. Change the prompt and the behaviour
  changes; jailbreak the prompt and the behaviour changes. A prompt is where
  behaviour *lives*, but it is not a place to put a *control*.

## What it cannot do

This is the useful part of starting here. Everything below is missing on
purpose, and each one is a session's worth of work:

| It cannot… | What fixes it |
|---|---|
| be trusted with `sendReply` | **human-in-the-loop** — a gate the dangerous tool has to pass, and that can wait for days without holding a process open. DBOS has the pieces already: `send`/`recv` and a durable `sleep` |
| run model-written code safely | **a sandbox** — one mediated door, with the environment stripped and a timer |
| hold a conversation across tasks | **memory** — history, state and context kept apart, and compacted against a token budget |
| stay affordable in a long task | **context management** — the messages are sent whole and grow all run |
| be shown to be working | **evals** — no tests, no scores, nothing but your own reading of the replies. The seams above are what make them cheap to write: a stub `ToolBox` is three lines |

Add them one at a time, and let each earn its place by fixing something you
have actually felt. The first one is what part 1 was built to make you feel,
and it is the only one of the original two still standing.
