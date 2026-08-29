# English teacher (Go) — terminal or browser, on a real harness

> **Branch `session-5`.** Each session of the course is a branch, with the agent
> at the repo root — `git switch session-5` then `go run .`. This is the newest:
> it takes the tools that changed the machine back out, leaving the harness and a
> small read-only toolset. `session-4` is the same harness with the file, shell
> and diagram tools still in it; `session-3` is the agent without the harness;
> `session-2` adds the built-in diagram renderers; `session-1` has no tools at
> all.

An **English teacher** you talk to in a loop, **in the terminal or in a
browser**. Paste it a paragraph and it comes back corrected, with a line per
change and the rule behind it; ask it a grammar question and it looks the rule
up in its own reference before answering. It **asks a human before doing
anything dangerous** — and holds no dangerous tool at all, which is a stronger
statement than the promise.

The teacher is one agent on a general-purpose harness. Swapping in a different
job means a prompt, a corpus and a tool list — everything below that line is
unchanged.

Underneath it is **the harness** — the runtime layer between the agent loop and
the real world, and the part most agents don't have until it bites them. One
sentence holds the whole design together:

> **The LLM decides the next semantic step. The harness owns execution.**

Kill the process mid-task and nothing is repeated when it resumes. Model-written
code runs in a sandbox with no credentials and a timer. Context is assembled per
turn against a token budget instead of growing forever. The agent you talk to
literally cannot change anything — it hands that to a specialist that can. And an
approval nobody answers parks the run on disk, where it costs nothing to wait
three days.

Model access goes through the [OpenRouter Go SDK](https://github.com/OpenRouterTeam/go-sdk)
(`github.com/OpenRouterTeam/go-sdk`), which needs **Go 1.25+**.

## Run

```sh
# needs OPENROUTER_API_KEY in .env

go run .                 # terminal: paste text or ask a question, 'exit' to quit
go run . -http :8080     # browser: open http://localhost:8080
go run . -ask "..."      # one turn, prints only the answer (what the evals drive)
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

## The English teacher (`corpus/`, `tools/knowledge.go`, `agent.TeacherPrompt`)

The agent has one job: **correct English text, and answer questions about
English grammar**. Paste a paragraph and it comes back corrected, with a line
per change and the rule behind it; ask about a rule and you get the rule, an
example, and the file it came from.

```
$ go run . -ask "she dont like when i writes letters, i have went there since 3 years"

**Corrected**
She doesn't like it when I write letters. I have been going there for three years.

**Changes**
- "dont" -> "doesn't" — subject-verb agreement (source: subject-verb-agreement)
- "writes" -> "write" — the verb agrees with I (source: subject-verb-agreement)
- "have went" -> "have been going" — past participle after have (source: verb-tenses)
- "since 3 years" -> "for three years" — since takes a point, for takes a duration (source: verb-tenses)
```

Three decisions carry it.

**The output shape is fixed.** Corrected text first and whole, so it can be
pasted straight back; then the changes, each naming the rule. A correction the
user has to hunt for is worth less than one they can use.

**It corrects the text; it does not answer it.** "where is the station" is a
sentence to punctuate, not a question to answer — a failure mode every
correction agent has, and one line of prompt fixes.

**It stops when the question is answered.** No tour of neighbouring rules, no
recap. That sentence is not politeness: it is what the relevancy score below
is measuring.

The agent holds exactly two tools — `search_knowledge` and `handoff` — and the
roster is why. A teacher with a weather tool and a file writer is a teacher that
can be talked into using them; least privilege here is as much about staying on
the subject as about safety.

## RAG: the private reference (`corpus/`)

Nine markdown files, 200–500 words each, one rule per file: subject–verb
agreement, articles, verb tenses, prepositions, commas and punctuation,
conditionals, pronouns, common L2 errors, modifiers and word order, style and
register. `search_knowledge` embeds the query, ranks the corpus, and returns the
best three with a similarity score each.

This is the third way of getting information in front of a model, and the three
do not substitute for each other:

| Pattern | When the model sees it | Use for |
|---|---|---|
| Context (the system prompt) | Every turn | What the agent ALWAYS needs — role, output shape |
| Web search | On demand, model decides | Fresh public facts |
| **RAG** (this) | On demand, model decides | Your private corpus, in your wording |

**Retrieval is TF-IDF cosine, not embeddings.** OpenRouter serves chat
completions only — there is no embeddings endpoint behind this key — so the
vectors are built locally: sublinear term frequency, smoothed IDF, L2-normalized,
cosine as a dot product. The *shape* is the one a hosted vector store hides
(turn the query into a vector, rank by cosine, return the top few with scores),
and the tool's interface is unchanged, so swapping in
`text-embedding-3-small` later means rewriting `weigh()` and nothing else.

**No stopword list, deliberately.** In a grammar reference the function words
*are* the subject matter — "a", "an", "the", "since", "for". IDF already pushes
a word that appears in every document to nearly zero weight, which is the job a
stopword list would do badly here.

**topK = 3.** One result is brittle (top match wrong → nothing to fall back on);
five is noise on a corpus this size. Three, with the score attached, lets the
model see how good its best match was.

**The score ranks results against each other, not against an absolute bar.**
Cosine against a 300-word document is small for any short query — *"a vs an"*
scores 0.13 on exactly the right file. So the prompt tells the agent to decide
coverage by *reading* what came back, and to cite a file only if its text states
the rule being used. An earlier version leaned on the number and produced a
confident citation of `commas-and-punctuation` for a question about *who* vs
*whom*.

**When a rule is missing, the fix is the corpus, not the code.** That who/whom
answer is what `corpus/pronouns.md` exists for. Add a file, re-run the evals.

## Answer relevancy: deepeval's metric, in Go (`internal/evalscore`)

```sh
go test ./internal/agent -run TestEvalAgentBehavior -v   # scorecard + a reason per case
cd evals && go test -run TestBehaviorEval -v             # the same score, into Braintrust
```

```
[PASS] relevancy 1.00 — tools used: [search_knowledge]
[PASS] relevancy 0.75 — tools used: []
SCORECARD: 5/5 scenarios passed, mean relevancy 0.95
```

`AnswerRelevancyMetric` asks one question: **is the reply about what was
asked?** It splits the answer into statements and judges each one against the
input; the score is the fraction that address it. So it does *not* check that
the grammar advice is correct — an agent that teaches a wrong rule fluently and
on topic scores 1.00. What it catches is padding: the neighbouring rule nobody
asked about, the correction that drifts into a style rewrite. That is why the
teacher's prompt says *answer the question that was asked, then stop* — this
score is what that sentence is accountable to.

**Why it is written here rather than imported.** deepeval is Python and has no
Go SDK; Confident AI's Go story is OpenTelemetry tracing into their platform,
not the metrics. But the metric is a recipe, not a library trick, and a short
one: statements → per-statement verdicts → relevant ÷ total → one call for the
reason. Four model calls, ~200 lines, and the whole repo stays `go test ./...`
with no virtualenv.

The port scored the same mean (0.95) as the Python original on the same
answers — but its judging prompts are ours, not deepeval's internals, so
individual scores land close rather than identical. Treat the series as the
signal, which is true of any LLM judge.

Two details that cost real debugging:

- **The rubric has to be explicit about what counts.** A first version scored a
  perfectly good correction 0.60, because the judge read example sentences and
  per-change bullets as "not directly answering". The prompt now names them —
  corrected text, one item in a list of corrections, a supporting example, the
  rule's name, the citation — as relevant.
- **Verdicts carry the statement's number.** Asked for five verdicts a model
  will occasionally return four, or six; matching by position then scores the
  wrong statements silently. With indices a miscount is detectable, worth one
  retry, and an error rather than a bogus zero if it happens twice.

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
▶ workflow.started    wf=acca4cb5 text=change the config to light mode
▪ step.completed      wf=acca4cb5 name=model-00 ms=837
↪ agent.handoff       wf=acca4cb5 from=teacher to=operator
✋ approval.requested  wf=acca4cb5 text=Change the config?
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
report = agent_tools.call("get_weather", location="Tokyo")
print(report)
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
teacher   search_knowledge · handoff
operator  run_code   ← plus every tool that changes the machine
```

The teacher cannot be *talked into* deleting something, because the tool that
would do it is not in its list to call. Note the second thing that list buys:
an agent that holds a weather tool and a web search will eventually reach for
them mid-lesson. Least privilege keeps it safe *and* keeps it on the subject. It hands over instead, and the switch is
recorded in the workflow so a crash resumes as the operator. Run with `-v` to
see the roster printed at startup.

The operator's own list is down to `run_code` while the toolset carries nothing
dangerous — the split is deliberately left wired up, so a new tool that changes
the machine is added to `tools.Default` with an `Approver` and to
`agent.OperatorTools` by name, and the routing, the gate and the durable park
all apply to it from its first line.

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

The toolset ships with nothing dangerous in it, so the walkthrough below runs
`change_thing` — the stub `tools/tools_test.go` gates to exercise exactly this
path. Give the operator a real tool and the lines are identical with its name in
them.

```
$ go run .
you> change the config to light mode
  ↪ agent.handoff       from=teacher to=operator
⚠️  Approve this action?
    Change the config?
    [y/N]:
(no answer — parking this for later)
⏸  awaiting approval: Change the config?
   go run . -approve acca4cb5      (or -deny acca4cb5)

$ # the process EXITED. nothing is running. nothing was changed.
$ # hours or days pass. the server can reboot; it changes nothing.

$ go run . -approve acca4cb5
  ⟲ workflow.resumed    wf=acca4cb5 agent=operator
  ⏩ step.cached         wf=acca4cb5 name=model-00        # replayed, not re-billed
  ⏩ step.cached         wf=acca4cb5 name=tool-call_2D4S  # the handoff, replayed
  ↪ agent.handoff       wf=acca4cb5 from=teacher to=operator
  ⏩ step.cached         wf=acca4cb5 name=model-01
  🖊 approval.resolved   wf=acca4cb5 approved=true
  ✓ tool.completed      wf=acca4cb5 name=change_thing result=Changed the config.
  ✔ workflow.completed  wf=acca4cb5
```

Say `-deny` instead and the model is told a human refused — so it explains that
the change needs manual review rather than retrying blindly, and nothing
happens.

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

## What it does → where it lives

| # | Capability | Where |
|---|---|---|
| 1 | Correct text, answer grammar questions | `agent.TeacherPrompt` + `agent.TeacherTools` — `agent/roster.go` |
| 2 | Look a rule up in a private reference | `SearchKnowledge` over `corpus/` — `tools/knowledge.go` |
| 3 | Answer from own knowledge (no tool) | the loop returns when there are no tool calls — `agent/agent.go` |
| 4 | Search the web | `NativeWebSearch` — OpenRouter's own `web` plugin — `tools/nativesearch.go` |
| 5 | Run code in a sandbox | `RunCode` + the tool bridge — `tools/code.go`, `sandbox/` |
| 6 | Human-in-the-loop before danger | the `Approver` port every sensitive tool takes — `tools/tools.go`, `ui/console.go`, `web/session.go`; made durable by `approval/` |
| 7 | Eval suite | `tools/*_test.go` + `agent/eval*_test.go` + `evalscore/` (relevancy) + `evals/` (Braintrust) |

## Beyond the basics

- **`get_weather`** — current temperature and wind for a place, via Open-Meteo's
  free, keyless APIs (geocode the name, then fetch conditions). Read-only, no
  approval — `tools/weather.go`.
- **Browser UI** — `go run . -http :8080` serves the same agent as a chat page:
  tool calls appear as they run and dangerous ones stop for an Approve/Deny
  click. See **Browser UI** below — `web/`.
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

One page, embedded in the binary with `go:embed` — no build step and no CDN.
`web.Session` is to the browser what `ui.Console` is to the terminal: it is the
approver the tools ask, and the hooks the agent reports progress to.

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
  refused, and browsers don't share a history but follow-ups do.
- **`tools` package** — unit evals: a gated tool acts only on a yes and does
  nothing on a no, a read-only tool never reaches the human, unknown tool
  handled, live web search, and the least-privilege seams — a sensitive tool
  declares itself, sandboxed code never sees one, and a restricted subset cannot
  dispatch what it does not hold. The gated tool these use is a stub defined in
  the test file, so the seams stay covered no matter which real tools the
  toolset happens to carry.
- **`tools` retrieval** — the ranking, against the *real* corpus rather than a
  fixture, because "does the question a learner asks reach the file that answers
  it" is the only thing this tool does: eight questions, eight expected files.
  Plus the contract around it — at most three hits, ranked, scored in (0,1],
  never more confident than it should be on an off-topic query, and a missing
  corpus is an error rather than a quiet empty result.
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
- **`evals/` module** — the same live evals, reported to Braintrust so scores
  accumulate across runs instead of scrolling away. Separate module on purpose:
  the SDK pulls ~60 dependencies and the agent keeps its two. See
  [`evals/README.md`](evals/README.md).
- **`evalscore` package** — deepeval's `AnswerRelevancyMetric`, ported to Go and
  run by both eval front-ends, so the Go scorecard and the Braintrust dashboard
  can never disagree about the same answer. See **Answer relevancy** above.
- **`agent` package, live evals** (skipped with `-short` or without a key):
  - *behavioral* (`eval_test.go`) — whole tasks through the real teacher, built
    by `agent.TeacherRoster` so the graded agent is the one that ships: did it
    look the rule up, correct the text instead of answering it, cite the file
    the rule actually came from — and, on every case, is the reply about what
    was asked (`evalscore.AnswerRelevancy`).
  - *tool selection* (`eval_single_test.go`) — one-shot: does the model pick the
    right tool, with the right arguments, on the first step? Never executes,
    which is what makes it safe to ask the teacher to delete a file and check
    that it hands off instead.
  - *context budget* (`compact_smoke_test.go`) — asks five questions under an
    absurdly small budget and checks that the context stops growing, that the
    older turns survive as a summary rather than being dropped, and that the last
    question is still there verbatim.

  The behavioral and tool-selection evals print a scorecard. The same datasets
  are graded again in `evals/`, reported to Braintrust — one set of cases, two
  front-ends, so the two can never disagree about the same run.

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
