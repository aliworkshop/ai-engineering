# AI Engineering Course — a CLI agent, session by session

Each session of the course is a **branch**, not a folder. Every branch has the
same shape — the agent at the repo root — so switching sessions is:

```sh
git switch session-2
go run .
```

No path to remember, and no way to accidentally run one session's agent from
another session's directory.

## The branches

| branch | what the agent can do |
|---|---|
| [`session-1`](../../tree/session-1) | The loop: a conversation with a model, plus tools — web search, weather, reading and writing files, running commands behind a human-in-the-loop approval gate. History compaction keeps a long chat from growing the token bill. |
| [`session-2`](../../tree/session-2) | Adds drawing. `generate_diagram` turns a prompt into a flowchart, table or chart in one call; `add_elements` / `update_elements` / `remove_elements` edit it in place. Renders to `canvas.svg` and `canvas.excalidraw`. |
| [`session-3`](../../tree/session-3) | The drawing is rendered by **Excalidraw itself** — `convertToExcalidrawElements()` and `exportToSvg()` running under Node — so the output has the real hand-drawn stroke and fonts. Falls back to the built-in renderers when the sidecar isn't installed. |

`main` holds this page and the shared `.gitignore`. It has no code, so nothing
is duplicated between it and the session branches.

## Running any session

```sh
git switch session-1        # or session-2, session-3
go run .
```

You need `OPENROUTER_API_KEY` in a `.env` file at the repo root. `.env` is
gitignored, so it stays put when you switch branches — set it once and every
session picks it up.

`session-3` additionally wants its Excalidraw renderer built, once:

```sh
cd excalidraw && npm install
```

Without it that session still draws, using the built-in renderers.

## Tests

```sh
go test ./... -short     # fast, offline, no API key
go test ./...            # everything, including live model calls
```

## Why branches

Folders meant every session's code sat in every checkout, and the import paths
carried a `session-N` segment that had to be rewritten each time a session was
copied forward. As branches, each session is the whole repository at that point
in the course: one module path, one layout, and `git diff session-1 session-2`
shows exactly what a session added.
