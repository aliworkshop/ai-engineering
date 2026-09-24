# The loop (Go) — a prompt, a tool, and layers that point inward

> **Branch `session-7`.** Built up from nothing, one step at a time. Two
> dependencies and nothing clever. The other branches are finished agents:
> `session-6` is support triage on a full harness, `session-5` an English
> teacher, `session-1` the loop with tools.
>
> **Step 1** was the loop: talk to a model, keep the history. **Step 2** added
> one tool, and the loop that runs it. **Step 3** added a system prompt.
> **Step 4** is here: the same program, in layers that can be replaced one at
> a time.

An agent is a loop. You type, it goes to a model, the reply comes back, and the
conversation so far goes out with the next question. Give the model a tool and
one thing changes: a reply can now be a *request* instead of an answer — run
this, then ask me again — and the loop keeps going until the model stops
asking.

```sh
# needs OPENROUTER_API_KEY in .env
go run .
```

```
you> what is 12 * 9?

agent> 12 * 9 equals 108.

you> who won the 2026 winter olympics medal count?
  [web_search] {"query":"2026 Winter Olympics medal count winner"}

agent> Norway won the medal count at the 2026 Winter Olympics, with 41 medals…
       Sources: https://olympics.com/en/milano-cortina-2026/medals …
```

Two things are worth noticing in that transcript. The model answered `12 * 9`
by itself — a tool it holds is not a tool it has to use, and the system prompt
is what tells it so. And the search was its idea: nothing in this program
decides *when* to search, it only decides what happens when the model asks.

## Architecture

Dependencies point inward. The terminal knows the agent; the agent knows an
abstract tool box; the tools know nothing about either. Nothing inner imports
anything outer, so each layer can be tested — or replaced — on its own.

```
main.go                    wire the pieces together, then run
  └── internal/
        ui/       Console  the REPL. Owns stdin and stdout; nothing else prints
        agent/    Agent    the loop, the conversation, the system prompt
        tools/    Registry the Tool interface, and every tool
        llm/               the one place the OpenRouter client is built
```

`main.go` is now four lines of wiring, outermost last:

```go
client    := llm.NewOpenRouter(apiKey)
toolbox   := tools.Default(client)
assistant := agent.New(client, Model, toolbox)
console   := ui.New(os.Stdin, os.Stdout)

console.Run(context.Background(), assistant)
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

And one hook. The agent does not print — it calls `OnToolCall` if something is
listening, and the console decides what that looks like. That is what lets the
same loop be driven later by a test, a script, or a browser without touching a
line of it.

## The loop (`internal/agent`)

```go
for step := 0; step < maxSteps; step++ {
    reply := think(ctx)                  // history + tool specs out, one reply back
    history = append(history, reply)
    if len(reply.ToolCalls) == 0 {
        return reply.Text                // it answered: done
    }
    runTools(ctx, reply.ToolCalls)       // it asked: do the work, append results
}
```

Three things the loop owns rather than the model:

- **`maxSteps`** — a model that keeps asking for the same tool otherwise loops
  until your credit does.
- **Dispatch always returns a string.** A failure becomes `error: …` in the
  transcript, so the model can try something else. An error that propagated
  would end the conversation.
- **A failed turn rewinds the history** to where it started. A turn appends
  several messages; a tool call left with no result is a conversation the API
  refuses on the next question.

## The system prompt

Without one you get the model's defaults: a chatty assistant that restates your
question, offers further help, and reaches for the search tool because the tool
is there. The prompt is how you say otherwise.

```go
const SystemPrompt = `You are a command-line assistant. You answer questions, and you can search the web.

- Answer from your own knowledge when you can. Do NOT search for things you
  already know: arithmetic, definitions, how something works, general facts.
- Use web_search when the answer depends on something current or changing …
  Keep the source URLs in your reply when you do.
- Don't make things up. If you don't know and cannot find out, say so.
- Keep answers short: a few sentences, or a short list. No preamble, no
  restating the question, no offer of further help.`
```

Three things about it are worth more than the text itself:

- **It is message zero and it never moves.** The model re-reads it before every
  reply, which is what makes standing instructions stand. It is also therefore
  the most expensive text in the program — sent on every turn, forever — so it
  stays short. Anything that trims this history later has to keep it: an agent
  that compacts away its own instructions forgets what it is mid-conversation.
- **A tool in the list is an invitation.** "Answer from your own knowledge when
  you can" is the line that stops a model with a search tool from searching for
  `12 * 9`. Tool descriptions and the prompt are one system: the description
  says what the tool is for, the prompt says when *not* to reach for it.
- **The last line is a product decision.** "No preamble, no offer of further
  help" is what makes the replies above fit on a terminal. A prompt is mostly
  where the behaviour you want lives, and the first place to look when the
  behaviour you get is wrong.

Ask it something unknowable and the third line shows up instead of an
invention:

```
you> who is the current king of Narnia?

agent> Narnia is a fictional place from C.S. Lewis's "The Chronicles of
       Narnia". There isn't a current king — it's a fantasy world…
```

## The tool (`internal/tools/search.go`)

`web_search` searches through OpenRouter itself rather than a third-party
search API, so `OPENROUTER_API_KEY` remains the only key you need.

OpenRouter does not expose search as its own endpoint — it models search as a
**plugin on a chat request**. The tool sends the query as a one-off completion
with the `web` plugin attached; OpenRouter runs the search, injects the hits
into that request's prompt, and the model writes the findings back. So what
comes back is an already-summarized answer with source URLs rather than raw
ranked results, at the cost of one extra model call.

```go
Plugins: []components.ChatRequestPlugin{
    components.CreateChatRequestPluginWeb(components.WebSearchPlugin{
        ID:         components.WebSearchPluginIDWeb,
        MaxResults: openrouter.Int64(maxResults),
    }),
},
```

Three details that are easy to get wrong:

- **The description is the interface.** `Spec()` is everything the model knows
  about the tool. "Use it for current events, prices, releases, and
  anything else you might be out of date on" is what stops it searching for
  `12 * 9`, and it is program text, not a comment.
- **Ask for the URLs in the prose.** The plugin returns citations as
  annotations the SDK's assistant message does not expose, so the sub-request's
  system prompt asks for a `Sources:` list inside the text, where it can
  actually be read.
- **Arguments are model output.** `{"query":…}` is generated, not typed — it is
  unmarshalled defensively and an empty query is an error the model gets told
  about.

## What it cannot do

This is the useful part of starting here. Everything below is missing on
purpose, and each one is a session's worth of work:

| It cannot… | What fixes it |
|---|---|
| do anything but talk and search | **more tools** — files, shell, a private knowledge base |
| stay affordable in a long chat | **context management** — the history is sent whole and grows forever |
| survive being killed mid-task | **durable execution** — nothing is written down; a crash loses the conversation |
| be trusted with anything dangerous | **human-in-the-loop** — no gate, because there is nothing yet to gate |
| be shown to be working | **evals** — no tests, no scores, nothing but your own reading of the replies. The seams above are what make them cheap to write: a stub `ToolBox` is three lines |

Add them one at a time, and let each earn its place by fixing something you
have actually felt.
