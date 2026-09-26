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
	"errors"
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

// Task is what a workflow is given, and it is a struct rather than a string
// for one reason: a conversation has to travel INSIDE the input.
//
// The body must be deterministic, so it cannot read a seed from a field on
// some object in this process — a recovered run has no such object. Putting
// the prior messages in the input means they are checkpointed with everything
// else, and a replay a week later rebuilds exactly the same context.
type Task struct {
	// Seed is the conversation so far. Empty for a fresh task.
	Seed []agent.Msg `json:"seed,omitempty"`

	// Text is what the user just asked.
	Text string `json:"text"`
}

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

// run works one task and returns the answer. Sessions call it; nothing else
// should, because a task with no session is a conversation with no memory.
func (r *Runtime) run(ctx context.Context, task Task) (string, error) {
	if r.dctx == nil {
		return loop(plainSteps{ctx: ctx, wid: newRunID()}, task)
	}
	handle, err := dbos.RunWorkflow(r.dctx, work, task)
	if err != nil {
		return "", err
	}
	return handle.GetResult()
}

// Recover waits for whatever the last process left unfinished, and returns as
// soon as it is done.
//
// The recovery itself already happened: New launched, and DBOS found every
// PENDING workflow in Postgres and resumed it from the exact step where the
// process died. All this does is stay alive until those runs finish — by
// waiting on their actual results, not by sleeping for a fixed window and
// hoping. A run that finishes in four seconds returns in four seconds.
//
// It also explains the one thing that makes recovery silently do nothing: a
// workflow written by a different application version. DBOS will not touch
// those, so waiting on one would hang until the timeout for no reason.
// It returns the id of the last workflow it saw through to an answer, so the
// caller can carry that conversation into a session and keep talking.
func (r *Runtime) Recover(timeout time.Duration) (string, error) {
	if r.dctx == nil {
		fmt.Println("nothing to recover: this runtime is not durable.")
		return "", nil
	}

	pending, err := dbos.ListWorkflows(r.dctx, dbos.WithFilterStatus(unfinished...))
	if err != nil {
		return "", err
	}
	if len(pending) == 0 {
		fmt.Println("nothing to recover: no workflow was left unfinished.")
		return "", nil
	}

	var recovered string
	for _, wf := range pending {
		if wf.ApplicationVersion != AppVersion {
			fmt.Printf("  %s was written by app version %q, not %q — DBOS will not recover it.\n",
				wf.ID, wf.ApplicationVersion, AppVersion)
			continue
		}
		fmt.Printf("recovering %s from its last completed step…\n", wf.ID)

		answer, err := awaitResult(r.dctx, wf.ID, timeout)
		switch {
		case errors.Is(err, errStillRunning):
			fmt.Printf("  %s did not finish within %s — it stays unfinished and the next launch picks it up.\n",
				wf.ID, timeout)
		case err != nil:
			fmt.Printf("  %s failed: %v\n", wf.ID, err)
		default:
			fmt.Println("\nagent>", answer)
			recovered = wf.ID
		}
	}
	return recovered, nil
}

// unfinished is every status that is not an outcome.
//
// PENDING alone is not enough, and getting that wrong is easy: a crashed
// workflow is PENDING only until the next Launch, whose recovery pass moves it
// to ENQUEUED so a worker can pick it up. Since Launch happens in New — before
// anything here runs — by the time we look, the thing we are about to wait for
// has usually already left the status we were filtering on.
var unfinished = []dbos.WorkflowStatusType{
	dbos.WorkflowStatusPending,
	dbos.WorkflowStatusEnqueued,
	dbos.WorkflowStatusDelayed,
}

// errStillRunning means the timeout won the race, not that anything is wrong.
var errStillRunning = errors.New("still running")

// awaitResult blocks on one workflow's result, with a ceiling.
//
// GetResult blocks until the workflow reaches a terminal state, which is
// exactly the semantics we want and the reason this is not a poll. The timeout
// is there for the case where the run cannot finish at all — the model API is
// down, say — so a CLI does not hang forever. Losing the goroutine is fine: we
// are about to exit, and the workflow is safe in Postgres either way.
func awaitResult(dctx dbos.Context, workflowID string, timeout time.Duration) (string, error) {
	type outcome struct {
		answer string
		err    error
	}
	done := make(chan outcome, 1)

	go func() {
		handle, err := dbos.RetrieveWorkflow[string](dctx, workflowID)
		if err != nil {
			done <- outcome{err: err}
			return
		}
		answer, err := handle.GetResult()
		done <- outcome{answer: answer, err: err}
	}()

	select {
	case got := <-done:
		return got.answer, got.err
	case <-time.After(timeout):
		return "", errStillRunning
	}
}

// work is the durable entry point: the function DBOS registers, runs, and
// resurrects. It is a plain function for exactly that reason.
func work(ctx dbos.Context, task Task) (string, error) {
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
func loop(s stepper, task Task) (string, error) {
	// Emitting through a step means workflow.started appears exactly ONCE per
	// workflow, however many times the body is replayed. An event that
	// re-fires on every recovery is a log that cannot be counted.
	_, _ = s.text("started", func(context.Context) (string, error) {
		events.Emit(deps.Bus, events.Event{
			Type: events.WorkflowStarted, Workflow: s.id(), Input: task.Text,
		})
		return "", nil
	})

	// The context is rebuilt from the input alone, which is what makes a
	// replay exact: same seed, same question, same messages, every time.
	working := make([]agent.Msg, 0, len(task.Seed)+2)
	working = append(working, agent.Msg{Role: "system", Text: agent.SystemPrompt})
	working = append(working, task.Seed...)
	working = append(working, agent.Msg{Role: "user", Text: task.Text})

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
