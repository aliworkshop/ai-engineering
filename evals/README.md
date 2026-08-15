# Braintrust evals

The agent's live evals, reported to [Braintrust](https://www.braintrust.dev) so a
score becomes a point in a series instead of a line of test output that scrolls
away.

```sh
cd evals
go test -v                              # all three suites
go test -run TestToolSelectionEval -v   # the cheap one (8 model calls, ~4s)
```

Needs `OPENROUTER_API_KEY` and `BRAINTRUST_API_KEY` in `../.env`. Missing either
one skips rather than fails, with a message naming which.

## The three suites

| Suite | Cases | Scores | Notes |
|---|---|---|---|
| `tool-selection` | 8 | `correct_tool`, `correct_args` | One model call each, nothing executes. Cheapest signal, most sensitive to prompt edits. |
| `behavior` | 5 | `tool_choice`, `answer_match`, `side_effect` | Whole tasks through the real loop, graded on what landed on disk. |
| `diagram` | 6 | `schema`, `structure`, `preservation`, `keywords` | Four dimensions per case; the suite that most wants a dashboard. |

Scores abstain individually — `structure` applies only to create cases,
`preservation` only to modify cases. Braintrust handles that natively: a score
that is not in the list is not averaged, which is exactly the `n/a` the Go
scorecard prints.

## Why this is a separate module

The Braintrust SDK pulls ~60 modules (gRPC, protobuf, OpenTelemetry, Google
Cloud). The agent has two direct dependencies and keeps them — "raw SDK, no
frameworks" is a property of this repo, not an accident.

A nested module keeps both. Go's `internal/` rule is path-prefix based rather
than module based, so this module can import the parent's internal packages,
while the parent's `go build ./...` and `go test ./...` ignore nested modules
entirely. Nothing in here touches the agent's `go.mod`.

The OpenRouter SDK is **pinned to the parent's version**. Minimal version
selection would otherwise resolve a newer one here, and an eval that grades a
different build of the agent than you ship is worse than no eval. If you bump it
in the parent, bump it here too.

## Relationship to the Go eval tests

Additive, not a replacement. `internal/agent/eval*_test.go` still run offline
with `-short` and still print their scorecards. Same datasets, same scorers —
the diagram scoring is *literally the same code*, imported from
`internal/evalscore`, so the two can never disagree about the same run.

What Braintrust adds is history: which case flipped, when, and against which
model. The diagram eval is stochastic — it has scored anywhere from 4/6 to 6/6
on an unchanged agent — and telling variance from regression by re-running a
git worktree four times is not a process.

## Reading the results from the CLI

The score summary is not in the default API response. Ask for it:

```sh
curl -s -H "Authorization: Bearer $BRAINTRUST_API_KEY" \
  "https://api.braintrust.dev/v1/experiment/<id>/summarize?summarize_scores=true"
```

Without `summarize_scores=true` you get the experiment's name and URL and no
scores at all, which looks exactly like a broken integration and isn't.

## Gotchas worth knowing

- **The diagram suite runs serially and must.** The diagram tools write to the
  working directory, so each case `chdir`s into a temp dir — and the working
  directory belongs to the process, not the goroutine. `Parallelism: 1`.
- **The behavior suite runs serially too**, because its cases share a directory
  and the denial case asserts that a file does *not* exist.
- **Flush before exit.** `tp.Shutdown` is what sends the spans. Without it a
  fast test exits before the exporter drains and the experiment shows up empty.
