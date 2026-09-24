# The brittle agent (Go) — a harness jacket over a loop that loses everything

> **Branch `session-7`.** Built up from nothing, one step at a time. Two
> dependencies and nothing clever. The other branches are finished agents:
> `session-6` is support triage on a full harness, `session-5` an English
> teacher, `session-1` the loop with tools.
>
> **Step 1** was the loop: talk to a model, keep the history. **Step 2** added
> one tool. **Step 3** a system prompt. **Step 4** clean layers. **Step 5** is
> here: [week 3, part 1 — the brittle
> agent](https://github.com/pjsofts/ai-engineering-1/tree/main/week3-workshop/part1-brittle),
> where every move becomes an event and the dangerous tool runs with nothing
> in its way.

This is a **support triage agent**. Give it work items and it classifies each
one, reads the knowledge base, drafts a reply, and sends it. Three of those
tools are harmless. The fourth emails the customer.

```sh
# needs OPENROUTER_API_KEY in .env
go run . -sample     # the three sample items, one shot
go run .             # or talk to it
```

```
  ▶  workflow.started      6db483e3  {"input":"Handle these work items:…"}
  ⚙  tool.requested        6db483e3  {"name":"classifyItem","call":"call_MCa2…
  ✓  tool.completed        6db483e3  {"name":"classifyItem","call":"call_MCa2…
  ⚙  tool.requested        6db483e3  {"name":"sendReply","call":"call_lvcYHV8…
  ✓  tool.completed        6db483e3  {"name":"sendReply","call":"call_lvcYHV8…
  ✔  workflow.completed    6db483e3  {"output":"I processed three work items…"}
```

## It is wrong on purpose

Read the two `sendReply` lines again. It "really emails the customer", it
cannot be recalled, and between *requested* and *completed* there is nothing:
no policy, no human, no record that it happened. The model asked, and the mail
went out in the same millisecond.

That is one of two failures staged here deliberately:

| what is broken | what it costs you |
|---|---|
| **The state is a slice.** `Agent.history` lives in memory and nowhere else | Kill the process and the conversation is gone — mid-task, mid-spend, mid-`sendReply`, with no way to know which. Start again and everything runs a second time |
| **The dangerous tool is unmediated.** `sendReply` runs the instant it is requested | Nothing asks, nothing checks, nothing is written down. There is no log to audit and no way to tell a first send from a second |

Neither is an oversight, and neither is fixed by being careful. The prompt is
the only thing currently standing between the model and an irreversible
action, and *a prompt is not a control*. Both failures are what the harness
gets built to answer — and feeling them once is cheaper than being told about
them.

## Everything is an event (`internal/events`)

The harness never prints its feelings. It emits typed `Event` values, and a
sink decides what they look like:

```go
type Emitter interface{ Emit(Event) }
```

Today there is exactly one sink — a `Console` that renders a glyph, the type,
the workflow id, and whatever detail the event carries. But the reason to
build it this way is not prettier output. Progress that exists only as a
`fmt.Println` is invisible to everything except a human watching the screen.
An event is a value: it can be counted, written to a file, pushed down a
socket, or replayed. Every later sink is an addition rather than a rewrite.

The glyph table is deliberately wider than what anything emits —
`agent.handoff`, `approval.requested`, `plan.created`, `subagent.*` — because
that map is the roadmap, and a name reserved now cannot be spelled two ways
later.

One `Ask` is one **workflow**, with an eight-character id that every event in
the run carries. It is also the only thing about a run that outlives it, which
is another way of saying nothing does.

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
        agent/    Agent    the loop, the conversation, the system prompt
        tools/    Registry the Tool interface, and every tool
        events/   Event    the typed stream, and the glyphed console
        llm/               the one place the OpenRouter client is built
```

`events` sits *under* the agent and imports nothing of ours, so any layer may
emit and none of them has to know where the events go.

`main.go` is now four lines of wiring, outermost last:

```go
client    := llm.NewOpenRouter(apiKey)
toolbox   := tools.Default()
assistant := agent.New(client, Model, toolbox).
                 WithEvents(events.NewConsole(os.Stdout))

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

And one more seam: **`events.Emitter`**. The agent does not print — it emits,
and a nil emitter is a silent agent rather than a broken one. That is what
lets the same loop be driven later by a test, a script, or a browser without
touching a line of it.

## The loop (`internal/agent`)

```go
for step := 0; step < maxSteps; step++ {
    reply := think(ctx)                  // history + tool specs out, one reply back
    history = append(history, reply)
    if len(reply.ToolCalls) == 0 {
        return reply.Text                // it answered: done
    }
    runTools(ctx, wf, reply.ToolCalls)   // it asked: do the work, append results
}
```

Three things the loop owns rather than the model:

- **`maxSteps`** (10) — a model that keeps asking for the same tool otherwise
  loops until your credit does.
- **Dispatch always returns a string.** A failure becomes `error: …` in the
  transcript, so the model can try something else. An error that propagated
  would end the conversation.
- **A failed turn rewinds the history** to where it started. A turn appends
  several messages; a tool call left with no result is a conversation the API
  refuses on the next question.

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
| survive being killed mid-task | **durable execution** — checkpoint each step, so a resumed run replays instead of repeating |
| be trusted with `sendReply` | **human-in-the-loop** — a gate the dangerous tool has to pass, and that can wait for days |
| tell you what it did yesterday | **a durable event log** — the stream exists, but only ever reaches a terminal |
| stay affordable in a long chat | **context management** — the history is sent whole and grows forever |
| be shown to be working | **evals** — no tests, no scores, nothing but your own reading of the replies. The seams above are what make them cheap to write: a stub `ToolBox` is three lines |

Add them one at a time, and let each earn its place by fixing something you
have actually felt. The first two are what this part was built to make you
feel.
