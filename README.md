# CLI AI Agent (Go)

> **Branch `session-3`.** Each session of the course is a branch, with the agent
> at the repo root — `git switch session-3` then `go run .`. This is the newest:
> `session-2` is the same agent with the built-in diagram renderers, `session-1`
> has no tools at all.

A command-line AI agent you talk to in a loop. It answers from its own
knowledge, searches the web, reports the weather, writes and runs scripts, edits
files, and **asks a human before doing anything dangerous**. It also compacts its
own conversation history so a long session doesn't keep growing the token bill.

Model access goes through the [OpenRouter Go SDK](https://github.com/OpenRouterTeam/go-sdk)
(`github.com/OpenRouterTeam/go-sdk`), which needs **Go 1.25+**.

## Run

```sh
# needs OPENROUTER_API_KEY in .env
go run .
```

Type questions; type `exit` to quit.

## Architecture

Dependencies point inward. The UI knows the agent; the agent knows an abstract
tool box; the tools know an abstract approver. Nothing inner imports anything
outer, so each layer is testable on its own.

```
main.go                       wire everything together, then run
  └── internal/
        ui/          Console   REPL + terminal human-approver (owns stdin)
        agent/       Agent     the think → run-tools → repeat loop
        tools/       Registry  Tool interface + all tools + Approver port
        llm/                   OpenRouter Go SDK client factory
```

Key seams (interfaces):

- **`tools.Approver`** — the human-in-the-loop gate. `ui.Console` implements it
  for real; tests pass a yes/no stub. Tools never import the UI.
- **`agent.ToolBox`** — what the agent needs from its tools (`Specs`,
  `Dispatch`). `tools.Registry` satisfies it; the agent never imports concrete
  tools.
- **`tools.Tool`** — one capability. Add a feature by writing one struct with
  `Spec()` + `Run()` and listing it in `tools.Default`.

## The requirements → where they live

| # | Requirement | Where |
|---|---|---|
| 1 | Answer from own knowledge (no tool) | `agent.SystemPrompt` + loop returns when there are no tool calls — `agent/agent.go` |
| 2 | Search the web | `NativeWebSearch` — OpenRouter's own `web` plugin — `tools/nativesearch.go` |
| 3 | Write scripts and run them | `WriteFile` + `RunCommand` + `ReadFile` — `tools/files.go`, `tools/shell.go` |
| 4 | Edit existing files | `ReadFile` + `EditFile` — `tools/files.go` |
| 5 | Human-in-the-loop before danger | `Approver` gate on write/edit/delete/run — `tools/*.go`, `ui/console.go` |
| 6 | Eval suite | `tools/tools_test.go` + `agent/eval_test.go` + `agent/eval_single_test.go` |
| 7 | Draw a diagram from a prompt | `GenerateDiagram` → `canvas.svg` + `canvas.excalidraw` — `tools/diagram/` |
| 8 | Edit it in place | `AddElements` / `UpdateElements` / `RemoveElements` — `tools/diagram/crud.go` |

## Beyond the six

- **`generate_diagram`** — draw anything visual in one call from a prompt like
  *"Draw a flowchart of user signup"*: twelve shapes, tables, and bar/line/pie
  charts, mixed freely on one canvas. Writes two files from one layout:
  `canvas.svg` (open in a browser, refresh after each redraw) and
  `canvas.excalidraw` (open at [excalidraw.com](https://excalidraw.com) to edit
  it by hand). The model
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
- **History compaction** — every `CompactEvery` questions (default 5), the agent
  folds the older part of the conversation into one summary message and keeps the
  system prompt and the most recent turn verbatim. This caps per-request token
  growth without blurring the turn a follow-up is most likely to reference. It
  runs only at a safe boundary (after a final answer, so no tool call is left
  awaiting its result). Best-effort: if summarizing fails, the full history is
  kept — `agent/agent.go`.

## How the loop works (`agent/agent.go`)

`Agent.Ask` runs `think → runTools` until the model replies with no tool calls:

1. **think** — send the conversation + tool specs to the model.
2. If the reply has no tool calls, it's the final answer — return it.
3. **runTools** — run each requested tool, append results as `role: tool`
   messages, and loop (capped at `maxSteps`).

When a turn ends with a final answer, the agent counts the turn and compacts the
history if it has hit the interval (see **Beyond the six**).

The model never runs code itself — it only *asks*. A dangerous tool first calls
`Approver.Confirm` for a y/n on the terminal.

## Tests

```sh
go test ./...            # everything (also live web search + a real model call)
go test ./... -short     # fast, offline, deterministic (no key, no network)
```

- **`ui` package** — spinner unit tests: it animates and clears, stays silent
  off a terminal, and the console pauses it before printing mid-turn.
- **`tools` package** — unit evals: script roundtrip, edit, denial blocks the
  action, read-only tools never prompt, unknown tool handled, live web search,
  the diagram tool (valid SVG on disk, no overlap, cycles, bad input), the
  Excalidraw scene (envelope, element counts, bindings resolve, determinism),
  and the CRUD tools (batch add/update/remove, atomic rollback on a breaking
  batch, refusal to cascade a delete, edits composing across calls, spec
  round-trips to the same drawing).
- **`agent` package** — live evals (skipped with `-short` or without a key):
  - *behavioral* (`eval_test.go`) — whole tasks through the real model, graded
    on which tools it chose, its answer, and the actual side effects on disk.
  - *tool selection* (`eval_single_test.go`) — one-shot: does the model pick the
    right tool, with the right arguments, on the first step? Never executes.
  - *compaction* (`compact_smoke_test.go`) — asks five questions and checks the
    history folds down to the system prompt, a summary, and the last turn.

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
cd excalidraw && npm install     # installs deps and builds the bundle
npm run smoke                          # optional: proves it renders
```

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
| Open with | any browser | excalidraw.com (File → Open, or drag onto the canvas) |
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
