# The durable agent (Go) — crash it mid-task and nothing happens twice

> **Branch `session-7`.** Built up from nothing, one step at a time. Two
> dependencies and nothing clever. The other branches are finished agents:
> `session-6` is support triage on a full harness, `session-5` an English
> teacher, `session-1` the loop with tools.
>
> **Steps 1–4** built the loop: a model, one tool, a system prompt, clean
> layers. **Step 5** was [part 1 — the brittle
> agent](https://github.com/pjsofts/ai-engineering-1/tree/main/week3-workshop/part1-brittle):
> every move an event, and two failures staged on purpose. **Step 6** is here —
> [part 2 — durable
> execution](https://github.com/pjsofts/ai-engineering-1/tree/main/week3-workshop/part2-durable),
> which fixes the first of them.

This is a **support triage agent**. Give it work items and it classifies each
one, reads the knowledge base, drafts a reply, and sends it. Three of those
tools are harmless. The fourth emails the customer.

```sh
# needs OPENROUTER_API_KEY in .env
go run . -sample          # the three sample items, one shot
go run .                  # recover anything that crashed, then talk to it
go run . -list            # what the runtime is holding
go run . -audit           # did any work happen twice?
go run . -crash-at 2      # die mid-task, after real side effects
```

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

## Durable execution (`internal/durable`)

The whole mechanism is one method:

```go
func Step[T any](w *Workflow, name string, fn func() (T, error)) (T, error) {
    if raw, cached := w.steps[name]; cached {
        return decode[T](raw), nil       // no model call, no side effect, no cost
    }
    out, err := fn()                     // non-determinism lives HERE
    if err != nil {
        return zero, err                 // a FAILED step is never checkpointed
    }
    w.steps[name] = encode(out)
    w.save()                             // checkpoint BEFORE moving on
    return out, nil
}
```

A workflow is a JSON file in `.harness/wf/<id>.json`. A step is a named unit
of work whose result is written down the instant it finishes. To recover you
**re-run the workflow body** — completed steps return their cached result
without executing, and execution races forward to exactly where it died.

There is no separate recovery path. **Resuming is running.**

That only works if the body is *deterministic*, which is the golden rule of
durable workflows: everything non-deterministic — the model call, the tool
call, the clock — happens inside a step, and everything outside one is rebuilt
identically on replay. It is also why the agent no longer keeps a conversation
in a field. A run's messages are local, rebuilt from the workflow's own input
and its steps, so a fresh process replays them exactly. The cost is that the
REPL stops remembering across turns; each line is a task now.

Three details that are the difference between this working and looking like it
works:

- **A failed step is never checkpointed.** Pinning a failure would make the
  crash permanent, and retrying the workflow has to mean retrying what broke.
- **Saves are temp-file-and-rename.** The failure this package exists to
  prevent is a crash at the worst possible moment; a torn state file turns one
  lost step into an unrecoverable workflow.
- **A corrupt file fails loudly** rather than quietly starting over — since
  starting over is precisely the bug.

## Watch it crash

```sh
rm -rf .harness
go run . -sample -crash-at 2
```

```
  ▪  step.completed        bb657985  {"name":"tool-call_ViS34Cf…"}
  ▪  step.completed        bb657985  {"name":"tool-call_3EGOyEB…"}
  💥 simulated crash before step 2 — the process is gone.
```

Eight steps had completed. Real tool calls ran. The process is dead.

```sh
go run . -list
#  ID         STATUS    STEPS  TASK
#  bb657985   running   8      Handle these work items: - item-1 (customer_mes…
```

`running` with nobody running it — that is what crashed looks like. Now just
start the agent again. **You are not asked to do anything:**

```
recovering bb657985 from its last completed step…
  ⟲  workflow.resumed      bb657985  {"input":"Handle these work items:…"}
  ⏩  step.cached           bb657985  {"name":"model-00"}
  ⏩  step.cached           bb657985  {"name":"tool-call_Wh2R6I7…"}
  ⏩  step.cached           bb657985  {"name":"model-01"}
  ⏩  step.cached           bb657985  {"name":"tool-call_ViS34Cf…"}
  ▪  step.completed        bb657985  {"name":"model-02","ms":3461}
  ⚙  tool.requested        bb657985  {"name":"draftReply",…}
  ✔  workflow.completed    bb657985  {"output":"Here's what I did…"}
```

Every `⏩` is work that did not happen again: a model turn not re-billed, a
tool call not re-run. Then live steps carry it to the end.

## Proving it: `-audit`

Watching it recover is not the same as knowing nothing ran twice. The event
log already holds the answer, so the check is a read rather than an
instrument:

```
.harness/events.jsonl — 68 events across 1 workflows

  step.cached            25
  step.completed         16
  tool.requested         12
  workflow.resumed       2
  workflow.started       1

  tool calls executed    12
  steps replayed         25  (work a resumed run did not redo)
  repeated side effects  0  ✔ nothing ran twice
```

That run survived two crashes and still sent each reply once.

The measurement is `tool.requested` grouped by the model's own **call id**, and
it works because of where that event is emitted: *inside* the tool's
checkpointed step. A replay does not re-run the step, so it does not re-emit
it — one line per call id means the tool ran once, ever. Emit it outside the
step and every successful recovery would look like a duplicate.

A tool **name** repeating is normal: three items, three `sendReply` calls. A
**call** repeating is a customer who got two emails.

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

Dependencies point inward. The terminal knows the agent; the agent knows an
abstract tool box; the tools know nothing about either. Nothing inner imports
anything outer, so each layer can be tested — or replaced — on its own.

```
main.go                    wire the pieces together, then run
  └── internal/
        ui/       Console  the REPL. Owns stdin
        agent/    Agent    the loop and the system prompt
        tools/    Registry the Tool interface, and every tool
        durable/  Workflow checkpoint · crash · resume          ┐ the
        events/   Event    the typed stream, console and log    ┘ harness
        llm/               the one place the OpenRouter client is built
```

`durable` and `events` sit *under* the agent — they know nothing about agents,
tools or terminals, which is what lets each be used and tested on its own.
Everything the runtime owns lives in `.harness/` as plain files: a workflow is
a JSON file, the log is JSONL. Swapping in Postgres later changes the storage,
not a single idea above it.

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

## The loop (`internal/agent`)

```go
working := []Msg{{system, SystemPrompt}, {user, wf.Input()}}

for step := 0; step < maxSteps; step++ {
    reply := durable.Step(wf, "model-NN", think)   // checkpointed
    working = append(working, reply)
    if len(reply.ToolCalls) == 0 {
        return reply.Text                          // it answered: done
    }
    for _, call := range reply.ToolCalls {
        durable.Step(wf, "tool-"+call.ID, run)     // checkpointed
    }
}
```

Three things the loop owns rather than the model:

- **`maxSteps`** (10) — a model that keeps asking for the same tool otherwise
  loops until your credit does.
- **Dispatch always returns a string.** A failure becomes `error: …` in the
  transcript, so the model can try something else. An error that propagated
  would end the conversation.
- **A failed run keeps its completed steps.** The workflow file stays on disk
  marked `failed`, which is what makes the next attempt cheap — it replays
  what already worked and retries only what broke.

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
| be trusted with `sendReply` | **human-in-the-loop** — a gate the dangerous tool has to pass, and that can wait for days without holding a process open |
| run model-written code safely | **a sandbox** — one mediated door, with the environment stripped and a timer |
| hold a conversation across tasks | **memory** — history, state and context kept apart, and compacted against a token budget |
| stay affordable in a long task | **context management** — the messages are sent whole and grow all run |
| be shown to be working | **evals** — no tests, no scores, nothing but your own reading of the replies. The seams above are what make them cheap to write: a stub `ToolBox` is three lines |

Add them one at a time, and let each earn its place by fixing something you
have actually felt. The first one is what part 1 was built to make you feel,
and it is the only one of the original two still standing.
