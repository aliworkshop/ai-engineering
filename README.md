# The loop (Go) — a prompt, a tool, and the loop that runs them

> **Branch `session-7`.** Built up from nothing, one step at a time. Two
> dependencies and nothing clever. The other branches are finished agents:
> `session-6` is support triage on a full harness, `session-5` an English
> teacher, `session-1` the loop with tools.
>
> **Step 1** was the loop: talk to a model, keep the history. **Step 2** added
> one tool, and the loop that runs it. **Step 3** is here: a system prompt.

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

## What is in it

```
main.go     the REPL, the agent loop, and dispatch
search.go   the one tool
go.mod      the OpenRouter SDK, and godotenv for the key
```

`main.go`, in reading order:

0. **`SystemPrompt`** — the standing instructions, seeded as message zero.
1. **the REPL** — read a line, skip blanks, `exit` quits.
2. **`turn`** — one question to completion: think, run whatever tools were
   asked for, think again with the results, stop when a reply has no tool
   calls. That sentence is the whole agent loop.
3. **`think`** — one model call: the history plus the tool specs go out, one
   reply comes back.
4. **`dispatch`** — runs the tool that was named, and *always* returns a
   string. A tool that returns an error message lets the model try something
   else; a tool that crashes takes the conversation with it.
5. **`text`** — pulls the reply out of the SDK's string-or-array union.

A failed turn rewinds the history to where it started, so a half-written turn —
a tool request with no result — never survives into the next question.

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

## The tool (`search.go`)

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

- **The description is the interface.** `searchSpec()` is everything the model
  knows about the tool. "Use it for current events, prices, releases, and
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
| be shown to be working | **evals** — no tests, no scores, nothing but your own reading of the replies |

Add them one at a time, and let each earn its place by fixing something you
have actually felt.
