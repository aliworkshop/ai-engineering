// Package runtime executes the agent. It owns everything about HOW a task
// runs — durability, recovery, the event stream — and nothing about what the
// agent is, which lives one layer in, in package agent.
//
// Durable execution is DBOS Transact: a workflow and every step inside it are
// checkpointed into Postgres, and a crash costs a replay rather than a second
// email to the customer. The golden rule is the one every durable engine has:
// the workflow body must be deterministic, and everything non-deterministic —
// every model call, every tool — has to happen inside a step, or replay will
// not match.
//
// DBOS enforces that harder than a hand-rolled engine does. Steps are matched
// by POSITION, not by name, so the body must issue the same steps in the same
// order on every replay.
//
// With no database configured the same loop still runs, in memory, checkpoint-
// ing nothing. That path exists so the agent works on a laptop with no
// Postgres — and because being able to run the undurable version is what makes
// the durable one mean something.
package runtime

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"os"
	"sync/atomic"
	"time"

	openrouter "github.com/OpenRouterTeam/go-sdk"
	"github.com/dbos-inc/dbos-transact-golang/dbos"

	"github.com/aliworkshop/ai-engineering-course/internal/agent"
	"github.com/aliworkshop/ai-engineering-course/internal/events"
	"github.com/aliworkshop/ai-engineering-course/internal/tools"
)

// DatabaseEnv is where the Postgres URL comes from — the same variable DBOS
// reads everywhere else, so a connection string that works for their CLI works
// here. Unset means "run without checkpoints".
const DatabaseEnv = "DBOS_SYSTEM_DATABASE_URL"

// AppName identifies this application to DBOS. It is part of how recovery
// decides which workflows are ours, so it has to stay stable.
const AppName = "support-triage"

// AppVersion is pinned, and that is not cosmetic.
//
// DBOS only recovers workflows whose application version matches the running
// code, and by default that version is a HASH OF YOUR CODE — so editing any
// file between a crash and the recovery orphans the parked workflow, and the
// recovery silently does nothing. Pinning it makes recovery work across an
// edit.
//
// In production the default is the right behaviour and this would be a deploy
// identifier: a bad deploy must not half-replay workflows that were written by
// different code.
const AppVersion = "session-7"

// maxSteps caps how many tool rounds one task may take before we give up.
// Without it, a model that keeps asking for the same tool loops until your
// credit does.
const maxSteps = 10

// Options is everything the runtime needs to execute the agent.
type Options struct {
	Client   *openrouter.OpenRouter
	Model    string
	Registry *tools.Registry
	Bus      events.Emitter

	// CrashAt exits the process before this step, simulating a machine dying
	// mid-task after real side effects have already happened. Negative means
	// never. It is a demo hook, and it is here because the durability claim is
	// worth nothing until you have watched it fail and recover.
	CrashAt int
}

// The workflow body has to be a plain function DBOS can register and, later,
// recover on its own — it cannot close over a client passed in at call time,
// because recovery happens inside Launch with no caller in sight. So the
// dependencies live here, set once before Launch. It reads like a global
// because it is one; the alternative is a workflow DBOS cannot resurrect.
var deps Options

// Runtime is a connected execution engine. Durable when a database was
// configured, in memory when not.
type Runtime struct {
	dctx     dbos.Context // nil when running without checkpoints
	shutdown func()
}

// New connects the runtime. With DatabaseEnv set it opens DBOS, registers the
// workflow, and launches — which is also what starts recovery, so anything
// left PENDING by an earlier crash begins replaying immediately, unasked.
//
// Without it, the runtime is honest about running undurably and starts anyway.
func New(ctx context.Context, opt Options) (*Runtime, error) {
	deps = opt

	url := os.Getenv(DatabaseEnv)
	if url == "" {
		return &Runtime{shutdown: func() {}}, nil
	}

	quiet := newShutdownQuietHandler(os.Stderr, slog.LevelWarn)
	dctx, err := dbos.NewContext(ctx, dbos.Config{
		AppName:            AppName,
		ApplicationVersion: AppVersion,
		DatabaseURL:        url,
		// DBOS narrates its own startup at INFO, which buries the harness's
		// event stream under connection strings and version hashes. Warn keeps
		// the terminal about the agent; Inspect is where you go to ask the
		// engine about itself.
		Logger: slog.New(quiet),
	})
	if err != nil {
		return nil, fmt.Errorf("dbos: %w", err)
	}

	// Registration before launch, always: Launch is also what starts recovery,
	// and it can only resurrect workflows whose function it can find by name.
	dbos.RegisterWorkflow(dctx, work)
	if err := dbos.Launch(dctx); err != nil {
		return nil, fmt.Errorf("dbos launch: %w", err)
	}

	return &Runtime{dctx: dctx, shutdown: func() {
		// Silence the engine BEFORE asking it to stop. Cancelling the context
		// makes the queue runner log its in-flight work as failed — "context
		// canceled", at WARN and ERROR — which is true, dull, and reads like a
		// crash to anyone watching. We only drop what is logged after we asked
		// it to stop; anything that goes wrong while it runs still reaches you.
		quiet.silence()
		_ = dbos.Shutdown(dctx, 5*time.Second)
	}}, nil
}

// Durable reports whether work is being checkpointed.
func (r *Runtime) Durable() bool { return r.dctx != nil }

// Close shuts the engine down.
func (r *Runtime) Close() { r.shutdown() }

// Ask works one task and returns the answer. It satisfies what the console
// needs, so the terminal drives the runtime without knowing which engine is
// underneath.
func (r *Runtime) Ask(ctx context.Context, task string) (string, error) {
	if r.dctx == nil {
		return loop(plainSteps{ctx: ctx, wid: newRunID()}, task)
	}
	handle, err := dbos.RunWorkflow(r.dctx, work, task)
	if err != nil {
		return "", err
	}
	return handle.GetResult()
}

// Wait blocks, which is all recovery needs from us.
//
// This is the moment worth pausing on. Recovery takes no task and no workflow
// id: New already launched, DBOS already found every PENDING workflow in
// Postgres and resumed it from the exact step where the process died. All that
// is left is to stay alive while the drafts and the sends go out. None of the
// pre-crash tool calls run twice.
func (r *Runtime) Wait(d time.Duration) {
	if r.dctx == nil {
		fmt.Println("nothing to recover: this runtime is not durable.")
		return
	}
	fmt.Printf("launched — DBOS is recovering anything left PENDING (waiting %s)\n", d)
	time.Sleep(d)
}

// work is the durable entry point: the function DBOS registers, runs, and
// resurrects. It is a plain function for exactly that reason.
func work(ctx dbos.Context, task string) (string, error) {
	return loop(dbosSteps{ctx: ctx}, task)
}

// loop is the agent, and there is only one of it. Think, run whatever tools
// the model asked for, think again with the results, and stop when a reply
// carries no tool calls.
//
// Everything non-deterministic happens inside a step. Everything outside one —
// this slice, this counter, this if — is rebuilt identically on a replay,
// which is the golden rule of durable workflows and the only reason recovery
// can be this simple.
func loop(s stepper, task string) (string, error) {
	// Emitting through a step means workflow.started appears exactly ONCE per
	// workflow, however many times the body is replayed. An event that
	// re-fires on every recovery is a log that cannot be counted.
	_, _ = s.text("started", func(context.Context) (string, error) {
		events.Emit(deps.Bus, events.Event{
			Type: events.WorkflowStarted, Workflow: s.id(), Input: task,
		})
		return "", nil
	})

	working := []agent.Msg{
		{Role: "system", Text: agent.SystemPrompt},
		{Role: "user", Text: task},
	}

	for i := 0; i < maxSteps; i++ {
		maybeCrash(i)

		// The model call is a step. Not because it touches the world, but
		// because it costs money and because a replay that asked a second time
		// would get different tool call ids and diverge from here on.
		reply, err := s.msg(fmt.Sprintf("model-%02d", i), func(c context.Context) (agent.Msg, error) {
			return agent.Think(c, deps.Client, deps.Model, working, deps.Registry.Specs())
		})
		if err != nil {
			return "", failed(s.id(), err)
		}
		working = append(working, reply)

		// No tool calls means the model is answering rather than acting.
		if len(reply.ToolCalls) == 0 {
			_, _ = s.text("completed", func(context.Context) (string, error) {
				events.Emit(deps.Bus, events.Event{
					Type: events.WorkflowCompleted, Workflow: s.id(), Output: reply.Text,
				})
				return "", nil
			})
			return reply.Text, nil
		}

		for _, call := range reply.ToolCalls {
			// The tool is a step for the reason this whole layer exists: it is
			// the half that touches the world, and a crash between running it
			// and recording it is how a customer gets two emails.
			//
			// The events are emitted INSIDE the step, which is a small decision
			// with a large consequence: a replay does not re-run the step, so
			// it does not re-emit them, and a tool.requested in the log is
			// therefore a tool that actually ran. That is what makes -audit's
			// count mean "a side effect happened twice" rather than "recovery
			// worked".
			result, err := s.text("tool-"+call.ID, func(c context.Context) (string, error) {
				events.Emit(deps.Bus, events.Event{
					Type: events.ToolRequested, Workflow: s.id(),
					Name: call.Name, Call: call.ID, Args: call.Args,
				})
				// Dispatch turns a failure into text rather than an error, so a
				// broken tool becomes something the model can read and work
				// around instead of a step that refuses to checkpoint.
				out := deps.Registry.Dispatch(c, call.Name, call.Args)
				events.Emit(deps.Bus, events.Event{
					Type: events.ToolCompleted, Workflow: s.id(),
					Name: call.Name, Call: call.ID,
				})
				return out, nil
			})
			if err != nil {
				return "", failed(s.id(), err)
			}
			working = append(working, agent.Msg{Role: "tool", Text: result, ToolCallID: call.ID})
		}
	}
	return "", failed(s.id(), fmt.Errorf("hit the step limit after %d tool rounds", maxSteps))
}

// failed reports the end of a run and hands the error back unchanged.
func failed(id string, err error) error {
	events.Emit(deps.Bus, events.Event{
		Type: events.WorkflowFailed, Workflow: id, Error: err.Error(),
	})
	return err
}

// maybeCrash is the demo hook, and it fires once per process. On a recovery
// run the step it guards is served from Postgres before the body gets here —
// which is exactly why the guard has to count rather than inspect: the body
// runs from the top every time, and it is the step calls that short-circuit.
func maybeCrash(i int) {
	if deps.CrashAt < 0 || i != deps.CrashAt || crashed.Load() {
		return
	}
	crashed.Store(true)
	fmt.Printf("\n  💥 simulated crash before step %d — the process is gone.\n", i)
	os.Exit(1)
}

var crashed atomic.Bool

// newRunID names an undurable run, so its events still read like everything
// else in the stream even though nothing is written down.
func newRunID() string {
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "00000000"
	}
	return hex.EncodeToString(b[:])
}

// shutdownQuietHandler is an slog.Handler that stops passing records on once
// silence() is called.
type shutdownQuietHandler struct {
	inner  slog.Handler
	silent *atomic.Bool
}

func newShutdownQuietHandler(w io.Writer, level slog.Level) *shutdownQuietHandler {
	return &shutdownQuietHandler{
		inner:  slog.NewTextHandler(w, &slog.HandlerOptions{Level: level}),
		silent: new(atomic.Bool),
	}
}

func (h *shutdownQuietHandler) silence() { h.silent.Store(true) }

func (h *shutdownQuietHandler) Enabled(ctx context.Context, l slog.Level) bool {
	return !h.silent.Load() && h.inner.Enabled(ctx, l)
}

func (h *shutdownQuietHandler) Handle(ctx context.Context, r slog.Record) error {
	if h.silent.Load() {
		return nil
	}
	return h.inner.Handle(ctx, r)
}

func (h *shutdownQuietHandler) WithAttrs(as []slog.Attr) slog.Handler {
	return &shutdownQuietHandler{inner: h.inner.WithAttrs(as), silent: h.silent}
}

func (h *shutdownQuietHandler) WithGroup(name string) slog.Handler {
	return &shutdownQuietHandler{inner: h.inner.WithGroup(name), silent: h.silent}
}
