# The loop (Go) — an agent with no prompt and no tools

> **Branch `session-7`.** The starting point, built up from here step by step.
> One file, two dependencies, and nothing clever. The other branches are
> finished agents: `session-6` is support triage on a full harness, `session-5`
> an English teacher, `session-1` the loop with tools.

An agent is a loop. You type, it goes to a model, the reply comes back, and the
conversation so far goes out with the next question. That is the whole thing —
everything else is an answer to a problem *this* version has.

```sh
# needs OPENROUTER_API_KEY in .env
go run .
```

```
you> what is 12 * 9?

agent> 12 * 9 equals 108.

you> and what did I just ask you?

agent> You asked what 12 multiplied by 9 is.
```

That second answer is the only feature here: the loop keeps a `history` slice
and sends it whole every turn. Take it away and each message arrives at a model
that has never met you.

## What is in it

```
main.go     ~95 lines — the loop, one model call, and reading the reply
go.mod      the OpenRouter SDK, and godotenv for the key
```

Three pieces, in reading order:

1. **the REPL** — read a line, skip blanks, `exit` quits.
2. **`ask`** — one model call. The whole history goes out, one reply comes back.
3. **`text`** — pulls the string out of the SDK's optional string-or-array
   content union.

A failed turn drops the question it was for, so the history never keeps a
message that was never answered.

## What it cannot do

This is the useful part of starting here. Everything below is missing on
purpose, and each one is a session's worth of work:

| It cannot… | What fixes it |
|---|---|
| do anything but talk | **tools** — describe a function to the model, run it when asked, feed the result back |
| stay affordable in a long chat | **context management** — the history is sent whole and grows forever |
| behave like anything in particular | **a system prompt** — there is none, so you get the model's defaults |
| survive being killed mid-task | **durable execution** — nothing is written down; a crash loses the conversation |
| be trusted with anything dangerous | **human-in-the-loop** — no gate, because there is nothing yet to gate |
| be shown to be working | **evals** — no tests, no scores, nothing but your own reading of the replies |

Add them one at a time, and let each earn its place by fixing something you
have actually felt.
