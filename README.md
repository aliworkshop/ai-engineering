# The durable agent (Go) — crash it mid-task and nothing happens twice

> **Branch `session-7-jev`.** Built up from nothing, one step at a time. Two
> dependencies and nothing clever. `session-7` is this agent without the
> judgment model, `session-7-durable-dbos` is the same durability on a real
> engine, and the other branches are finished agents: `session-6` is support
> triage on a full harness, `session-5` an English teacher, `session-1` the
> loop with tools.
>
> **Steps 1–4** built the loop: a model, one tool, a system prompt, clean
> layers. **Step 5** was [part 1 — the brittle
> agent](https://github.com/pjsofts/ai-engineering-1/tree/main/week3-workshop/part1-brittle):
> every move an event, and two failures staged on purpose. **Step 6** was
> [part 2 — durable
> execution](https://github.com/pjsofts/ai-engineering-1/tree/main/week3-workshop/part2-durable),
> which fixed the first. **Step 7** is here —
> [part 2+jev](https://github.com/pjsofts/ai-engineering-1/tree/main/week3-workshop/part2-jev):
> three of the agent's jobs move off the language model onto one that answers
> with numbers instead of sentences.

This is a **support triage agent**. Give it work items and it classifies each
one, finds the knowledge-base articles that bear on it, drafts a reply, checks
the draft, and sends it. Only the last of those is dangerous, and it is the
only one the language model is still trusted to trigger.

```sh
# needs OPENROUTER_API_KEY in .env
go run . -sample          # the three sample items, one shot
go run .                  # recover anything that crashed, then talk to it
go run . -list            # what the runtime is holding
go run . -audit           # did any work — or any judgment — happen twice?
go run . -bench           # how good are the judgments? (-jev-only skips the baseline)
go run . -crash-at 2      # die mid-task, after real side effects
go run . -no-jev          # the agent from before all this, still runnable
```

```
  ▶  workflow.started      6db483e3  {"input":"Handle these work items:…"}
  ⚙  tool.requested        6db483e3  {"name":"classifyItem","call":"call_MCa2…
  ✓  tool.completed        6db483e3  {"name":"classifyItem","call":"call_MCa2…
  ⚙  tool.requested        6db483e3  {"name":"sendReply","call":"call_lvcYHV8…
  ✓  tool.completed        6db483e3  {"name":"sendReply","call":"call_lvcYHV8…
  ✔  workflow.completed    6db483e3  {"output":"I processed three work items…"}
```

## Two failures, one and a half fixed

Part 1 staged two failures deliberately.

| | part 1 | now |
|---|---|---|
| **State is a slice** | kill the process and the conversation is gone; start again and everything runs a second time | a workflow is a file; every model turn, tool call and judgment is a checkpointed step, and a resumed run replays them from disk |
| **`sendReply` is unmediated** | runs the instant the model asks: no policy, no human, no record | **half fixed.** Two questions now stand between a draft and the customer, and the code that reads their answers is in Go. Nobody is asked |

The remaining hole is narrower every time. The agent can no longer email a
customer twice by accident, and it can no longer email them something it made
up. It can still email the *wrong customer*, confidently and correctly, the
instant the model asks — because nothing in this program has ever asked a
person. That is what part 7 is for.

## Judgment is not text (`internal/jev`, `internal/triage`)

Three of this agent's four jobs were never really language problems. Read what
they used to be:

- `classifyItem` was a tool that asked the model to **write down a category it
  had already decided**, and handed it back with no confidence attached.
- `searchKnowledgeBase` matched **substrings against four map keys**. "I was
  charged twice" retrieved nothing at all; "your pricing page is broken in
  Safari" retrieved the price list.
- Nothing checked a draft. The prompt asked for accuracy, and a prompt is not a
  control.

All three are now questions for **[Jev](https://docs.typesafe.ai/)**, a System
One model: you give it state and typed questions, and it returns typed answers
with **probabilities**. It never writes a sentence, so there is nothing to
parse and nothing to be talked out of. It is reached through OpenRouter's
`/v1/systemone` path, so the key this agent already had is the only credential
and `go.mod` does not grow.

```
  ?  jev.requested   {"name":"triage item-2","call":"triage-item-2","args":"category,kb_billing,kb_export,kb_pricing,kb_refund"}
  ⚡ jev.answered    {"name":"triage item-2","output":"category=technical/1.00 kb_export=0.98 kb_billing=0.02 …","ms":325}
  🏷 jev.triaged     {"name":"item-2","output":"category=technical confidence=1.00 articles=[export] (keep>=0.50, low<0.60)"}
```

Two events, and the gap between them is the whole idea. `jev.answered` is what
the model **said**: numbers. `jev.triaged` is what our code **decided** with
them. When the agent surprises you, that gap is where you look.

### The thresholds are the policy, and they are in Go

```go
const (
    ArticleThreshold = 0.5   // include an article when P(helps) is at least this
    GateThreshold    = 0.5   // block a send when P(grounded) or P(on-topic) is below this
    LowConfidence    = 0.6   // warn the drafting model about a category below this
)
```

Three constants, and every decision this agent makes about relevance and safety
is one of them compared against a probability. They are not in a prompt. You can
read them, test them, and — see `-bench` — sweep them and find out what they
cost.

### The gate

Before every send, two more questions: is every factual claim in this draft
supported by the articles it was given, and does it answer what the customer
actually asked. Code compares both against `GateThreshold`. A draft that fails
never reaches the tool; the model gets told why, in the same JSON shape it
would have been told success, and redrafts.

Asked for a draft telling a customer that 50 seats gets 40% off at $12/seat:

```
  ⚙ tool.requested  {"name":"draftReply","args":"…qualifies you for a 40% discount, bringing the price to $12 per seat…"}
  🛡 jev.gate        {"name":"item-3","output":"blocked grounded=0.03 on_topic=0.90 (>=0.50) — the draft states facts the KB articles do not support"}
  ⚙ tool.requested  {"name":"draftReply","args":"…team plans start at $20 per seat per month with a volume discount…"}
  🛡 jev.gate        {"name":"item-3","output":"pass grounded=0.78 on_topic=0.86 (>=0.50)"}
  ⚙ tool.requested  {"name":"sendReply","call":"call_iHeVyaG…"}
```

The prompt never mentioned the price. The block did. And note what is *missing*
from the blocked round: there is no `tool.requested` for that first send,
because nothing ran.

**The loop still names no tool.** It asks `triage.Drafted` and `triage.Sending`
what a call is, and `triage` can answer because it already imports the toolbox
to read the knowledge base. A loop that hard-coded `"sendReply"` would be one
rename away from silently ungating itself.

### Is it actually better? `-bench`

54 hand-labelled cases, both systems asked the *same* questions — the ones
`internal/triage` exports, so the benchmark grades what the agent really asks —
and the obvious alternative is given every advantage: the same questions as a
strict JSON schema, choices as enums, nouls as booleans, temperature 0.

```
  metric                   jev     llm
  category accuracy       100%     93%
  articles exact           97%     87%
  grounded accuracy        92%     71%
  gate decision            92%     71%
  bad drafts caught       100%     53%
  good drafts passed       78%    100%
  latency p50 (ms)         306    1162

  Jev Brier score: articles 0.006, gate 0.087 (0 = perfect, 0.25 = a coin flip)
```

**Bad drafts caught: 100% against 53%.** A gate built on the baseline lets half
of them through, which is not a gate. It is also four times slower, which is
what decides whether you can afford to run one before every send.

And then the table the baseline cannot produce at all, because a label has no
threshold to move:

```
  threshold articles exact  gate decision   bad caught   good passed
        0.2            97%           100%         100%          100%
        0.5            97%            92%         100%           78%
        0.9            97%            79%         100%           44%
```

Our 0.5 is wrong. At 0.2 the gate decides correctly every time and stops
blocking good replies; at 0.5 it turns away nearly a quarter of them. That is a
tuning decision with a number attached, which is the difference between
engineering a gate and arguing about one. (It is still 0.5 in the code. Moving
it should be its own commit, with this table in the message.)

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
.harness/events.jsonl — 61 events across 1 workflows

  jev.answered           6
  jev.requested          6
  step.cached            14
  tool.requested         6
  workflow.resumed       1
  workflow.started       1

  tool calls executed    6
  judgments bought       6
  steps replayed         14  (work a resumed run did not redo)
  repeated side effects  0  ✔
  repeated judgments     0  ✔
```

That run was killed mid-task after all three replies had gone out, then
recovered. Six tool calls, six judgments, each exactly once.

Two claims now, because they fail differently. A repeated **tool call** is a
customer who got two emails. A repeated **judgment** only bills you twice for a
decision you had already made and written down — which is the failure a replay
would actually cause, and the one worth watching now that thinking costs money
before any tool runs.

The key is the part that took a second try. Grouping judgments by the *question*
would be wrong: a blocked draft is rewritten and checked again, and that second
check is a different question about a different draft. So every judgment carries
an id for the **decision** it is — the item for a triage, the model's own call id
for a send being checked. Two lines with one id is the bug; two lines with one
purpose is the gate working.

The measurement is `tool.requested` grouped by the model's own **call id**, and
it works because of where that event is emitted: *inside* the tool's
checkpointed step. A replay does not re-run the step, so it does not re-emit
it — one line per call id means the tool ran once, ever. Emit it outside the
step and every successful recovery would look like a duplicate.

A tool **name** repeating is normal: three items, three `sendReply` calls. A
**call** repeating is a customer who got two emails.

The `⏩ step.cached` lines are worth watching scroll past during a recovery,
because they name what was *not* redone — `jev-triage-item-1` served from a
file instead of bought again.

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

| tool | what it does | advertised? |
|---|---|---|
| `classifyItem` | files an item under billing / technical / sales / other | only with `-no-jev` |
| `searchKnowledgeBase` | returns the house answer for a topic | only with `-no-jev` |
| `draftReply` | writes a reply — sends nothing | yes |
| `sendReply` | **emails the customer. Irreversible, and checked by code first** | yes, **and it is the dangerous one** |

The first two are still here, still working, and the model is no longer told
they exist — `tools.Reply()` advertises two tools where `tools.Default()`
advertises four. Taking them away is not tidiness: **a tool in the list is an
invitation**, and a model that still had `classifyItem` would go on calling it,
filing each item a second time under a category nothing would read.

Three things about that table are worth more than the code under it.

**Nothing at the call site announces the difference.** `SendReply.Run` looks
exactly like `DraftReply.Run` — decode the arguments, return some JSON. The
danger is not visible where the tool runs, which is why it has to be handled
by the harness around it rather than by whoever is reading the code.

**The description is the interface.** `Spec()` is everything the model knows
about a tool: the name, one sentence, and a JSON Schema. `classifyItem`'s
schema pins `category` to an enum rather than a free string, so the model
cannot invent a fifth. That schema is program text, not documentation.

**Retrieval used to be four keys and a substring match**, and it missed
constantly: an article matched only when the query happened to mention its
topic, so "charged twice" found nothing under `billing` and "your pricing page
is broken in Safari" found the price list. It is still in `searchKB`, and
`-no-jev` still runs it, because the replacement is only interesting next to
what it replaced — 97% exact retrieval against a matcher that cannot read.

## Architecture

Dependencies point inward. The terminal knows the agent; the agent knows an
abstract tool box; the tools know nothing about either. Nothing inner imports
anything outer, so each layer can be tested — or replaced — on its own.

```
main.go                    wire the pieces together, then run
  └── internal/
        ui/       Console  the REPL. Owns stdin
        agent/    Agent    the loop and the system prompt
        triage/   Triager  the three judgment jobs, and the thresholds
        bench/             the labelled cases, and what they say
        tools/    Registry the Tool interface, and every tool
        durable/  Workflow checkpoint · crash · resume          ┐ the
        events/   Event    the typed stream, console and log    ┘ harness
        jev/      Client   the one place the judgment client is built
        llm/               the one place the OpenRouter client is built
```

`jev` sits beside `llm`, not above it: two model clients, one that writes and
one that decides. `triage` is the layer that knows what an answer *means* —
which is also why the loop can stay ignorant of tool names, since `triage`
already imports the toolbox and can be asked what a call is. It knows nothing
about durability, which is what lets `bench` import it and measure the agent's
real questions without opening a store.

`durable` and `events` sit *under* the agent — they know nothing about agents,
tools or terminals, which is what lets each be used and tested on its own.
Everything the runtime owns lives in `.harness/` as plain files: a workflow is
a JSON file, the log is JSONL. Swapping in Postgres later changes the storage,
not a single idea above it.

`main.go` is wiring, and it holds exactly one interesting decision — which of
the two agents this is:

```go
bus      := events.Bus{events.NewConsole(os.Stdout), events.NewJSONL(log)}
store, _ := durable.NewStore(harnessPath("wf"), bus)

registry, judge := tools.Reply(), triage.New(jev.New(apiKey).WithEvents(bus), bus)
if *noJev {
    registry, judge = tools.Default(), nil
}

assistant := agent.New(llm.NewOpenRouter(apiKey), Model, registry).
                 WithEvents(bus).
                 WithStore(store).
                 WithJudge(judge)

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

Three more seams: **`events.Emitter`**, **`agent.Store`** and
**`triage.Asker`**. The agent does not print — it emits — and it does not know
what durability is, it just opens a workflow. `triage` likewise depends on an
interface it declares itself, satisfied by `*jev.Client` and by four lines of
fake in its tests, which is why the thresholds can be tested without a network.

All three are optional, and that is the pattern the whole branch is built on: a
nil emitter is a silent agent, **a nil store is the part 1 agent**, and **a nil
judge is the part 2 agent**, behind `-no-jev`. Every claim in this README is a
comparison, and a comparison needs the other thing to still run.

## The loop (`internal/agent`)

```go
items := triage.ParseItems(input)                       // pure: safe outside a step
for _, item := range items {
    step(wf, "jev-triage-"+item.ID, classify)           // checkpointed
}
working := []Msg{{system, a.prompt()}, {user, input + briefing}}

for i := 0; i < maxSteps; i++ {
    reply := step(wf, "model-NN", think)                // checkpointed
    working = append(working, reply)
    if len(reply.ToolCalls) == 0 {
        return reply.Text                               // it answered: done
    }
    for _, call := range reply.ToolCalls {
        verdict := a.gate(...)                          // checkpointed, if it is a send
        step(wf, "tool-"+call.ID, runOrBlock)           // checkpointed
    }
}
```

`ParseItems` runs *outside* a step, and that is only allowed because it is
pure: the same task yields the same items in the same order on a first run and
on a replay a week later. Everything non-deterministic — a model call, a tool,
a judgment — happens inside one.

Four things the loop owns rather than the model:

- **`maxSteps`** (12) — a model that keeps asking for the same tool otherwise
  loops until your credit does. Twelve rather than ten because a blocked draft
  costs one round to rewrite and one to send again, and a ceiling that cut that
  short would look exactly like the gate failing.
- **Dispatch always returns a string.** A failure becomes `error: …` in the
  transcript, so the model can try something else. An error that propagated
  would end the conversation.
- **A failed run keeps its completed steps.** The workflow file stays on disk
  marked `failed`, which is what makes the next attempt cheap — it replays
  what already worked and retries only what broke.
- **A blocked send still gets a `tool-` step.** Tempting to skip it — nothing
  ran, after all. But a call with no `tool-` step leaves the model asking for
  something nothing ever answered, and the next model call rejects the whole
  conversation. The step returns the blocked JSON and emits no
  `tool.requested`, which keeps `-audit` honest in the other direction.

One thing this engine gets for free that a real one does not: **steps are
matched by NAME.** A workflow is a JSON object keyed by step name, so inserting
a triage step ahead of the model turns cannot mis-align a workflow written by
older code. `session-7-durable-dbos` matches by *position*, where the same edit
means a pinned application version and a parked workflow that quietly never
recovers.

## The system prompt

Without one you get the model's defaults: a chatty assistant that restates your
question and offers further help. The prompt is how you say otherwise — here,
that the job is a four-step pipeline and that every item gets worked.

```go
const JevSystemPrompt = `You are a support reply agent.
Every work item has already been triaged by Jev (a fast classifier): its
category, the classifier's confidence, and the relevant knowledge-base
articles are listed below the task. Do not reclassify or search.
For each item:
1. Draft a reply with draftReply using ONLY facts from that item's articles
   (if it has none, acknowledge the request and say a specialist will follow up).
2. Send it with sendReply.
A quality check runs before every send. If a send comes back blocked, read the
reason, redraft that item once, and send again.
Finish with a brief summary of what you did.`
```

Read it against the one it replaced (`SystemPrompt`, still there for `-no-jev`)
and the shape of the change is visible in the text. Steps 1 and 2 are gone,
replaced by a line telling the model *not* to do them — because a model handed a
briefing it thinks it should double-check will double-check it, expensively, by
guessing.

Three things about it are worth more than the text itself:

- **It is message zero and it never moves.** The model re-reads it before every
  reply, which is what makes standing instructions stand. It is also therefore
  the most expensive text in the program — sent on every turn, forever — so it
  stays short. Anything that trims this history later has to keep it: an agent
  that compacts away its own instructions forgets what it is mid-conversation.
- **A tool in the list is an invitation.** The prompt and the tool descriptions
  are one system: the description says what a tool is for, the prompt says when
  to reach for it. One line is the only reason `sendReply` ever runs.
- **The last paragraph is the interesting one — and note what it is not.** It
  is not an instruction to be truthful, and it does not ask the model to check
  its own work. The check is code. It runs whether or not that paragraph is
  there, and jailbreaking the paragraph does not switch it off. All the prompt
  does is tell the model how to recover from a refusal it cannot argue with,
  which is the right thing to put in a prompt — and a good test of whether
  something else should have been code instead.
- **What is left is still the whole problem.** "Send it with sendReply" is one
  sentence of English governing an irreversible action. It is now a *checked*
  irreversible action, which is narrower, not fixed: a well-grounded, on-topic
  reply to the wrong customer passes the gate at 0.99 and goes out.

## What it cannot do

This is the useful part of starting here. Everything below is missing on
purpose, and each one is a session's worth of work:

| It cannot… | What fixes it |
|---|---|
| ask a **person** before `sendReply` | **human-in-the-loop** — a gate the dangerous tool has to pass, and that can wait for days without holding a process open. The quality gate is code checking code; this is the one that needs a human, and the two are not substitutes |
| gate a send that was never drafted | the gate runs only when there is a draft to check, so a model that calls `sendReply` without `draftReply` walks straight past it. The prompt asks for a draft first and `sendReply` wants a `draft_id`, which is not the same as being stopped |
| run model-written code safely | **a sandbox** — one mediated door, with the environment stripped and a timer |
| hold a conversation across tasks | **memory** — history, state and context kept apart, and compacted against a token budget |
| stay affordable in a long task | **context management** — the messages are sent whole and grow all run |
| be shown to be working | **evals** — no tests, no scores, nothing but your own reading of the replies. The seams above are what make them cheap to write: a stub `ToolBox` is three lines |

Add them one at a time, and let each earn its place by fixing something you
have actually felt. The first one is what part 1 was built to make you feel,
and after this part it is the only half of the original two still standing.
