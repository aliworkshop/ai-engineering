// Package dbosrun runs the same agent on somebody else's durable engine.
//
// internal/durable is about a hundred and fifty lines: a workflow is a JSON
// file, a step is a named entry in it, and resuming means running the body
// again and reading the completed steps out of the cache. Having built that,
// it is worth seeing the industrial version — because the point is not that
// DBOS is better, it is that the thing in internal/durable is the real idea,
// and here it is with the volume turned up.
//
// The mapping is nearly line for line:
//
//	internal/durable                      DBOS Transact
//	---------------------------------------------------------------------
//	durable.Store + .harness/wf/*.json    dbos.NewContext + Postgres tables
//	durable.Step(wf, name, fn)            dbos.RunAsStep(ctx, fn, WithStepName)
//	store.Pending + recoverPending        automatic recovery inside dbos.Launch
//	events.jsonl + -audit                 ListWorkflows / GetWorkflowSteps
//	(we don't have)                       queues, timeouts, fork-from-step, cancel
//
// The golden rule is identical in both: the workflow body must be
// deterministic, and everything non-deterministic — every model call, every
// tool — has to happen inside a step, or replay will not match. DBOS enforces
// it harder than we do. Our steps are keyed by NAME; DBOS matches them by
// POSITION, so the body must issue the same steps in the same order on replay.
//
// What this costs is the honest part. The agent's own harness needs two
// dependencies and a directory; this needs a database, a driver, and a library
// that pulls thirty more. That trade is exactly why the hand-rolled version is
// worth understanding first, and why this is a flag rather than the default.
package dbosrun

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
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
// here.
const DatabaseEnv = "DBOS_SYSTEM_DATABASE_URL"

// AppName identifies this application to DBOS. It is part of how recovery
// decides which workflows are ours, so it has to stay stable.
const AppName = "support-triage"

// AppVersion is pinned, and that is not cosmetic.
//
// DBOS only recovers workflows whose application version matches the running
// code, and by default that version is a HASH OF YOUR CODE — so editing any
// file between the crash and the recovery orphans the parked workflow, and
// the demo silently does nothing. Pinning it makes recovery work across an
// edit.
//
// In production the default is the right behaviour and this would be a
// deploy identifier: a bad deploy must not half-replay workflows that were
// written by different code.
const AppVersion = "session-7"

// maxSteps stops a loop that will not converge, exactly as the agent's own
// loop does. A runaway agent is a billing incident.
const maxSteps = 10

// The workflow body has to be a plain function DBOS can register and, later,
// recover on its own — it cannot close over a client passed in at call time,
// because recovery happens inside Launch with no caller in sight. So the
// dependencies live here, set once before Launch. It reads like a global
// because it is one; the alternative is a workflow DBOS cannot resurrect.
var deps struct {
	client   *openrouter.OpenRouter
	model    string
	registry *tools.Registry
	bus      events.Emitter
	crashAt  int
}

// Options is what a caller has to supply to run on this engine.
type Options struct {
	Client   *openrouter.OpenRouter
	Model    string
	Registry *tools.Registry
	Bus      events.Emitter

	// CrashAt exits the process before this step, simulating a machine dying
	// mid-task. Negative means never. It is the demo hook, same as the
	// agent's own — and it is more interesting here, because recovering needs
	// no id and no task, just a launch.
	CrashAt int
}

// Connect opens the DBOS context and registers the workflow. The returned
// close function shuts the engine down.
//
// Registration before launch, always: Launch is also what starts recovery, and
// it can only resurrect workflows whose function it can find by name.
func Connect(ctx context.Context, opt Options) (dbos.Context, func(), error) {
	url := os.Getenv(DatabaseEnv)
	if url == "" {
		return nil, nil, fmt.Errorf(
			"%s is not set — DBOS keeps its state in Postgres, e.g. postgres://user:pass@localhost:5432/agent_dbos",
			DatabaseEnv)
	}

	deps.client, deps.model, deps.registry, deps.bus = opt.Client, opt.Model, opt.Registry, opt.Bus
	deps.crashAt = opt.CrashAt

	quiet := newShutdownQuietHandler(os.Stderr, slog.LevelWarn)

	dctx, err := dbos.NewContext(ctx, dbos.Config{
		AppName:            AppName,
		ApplicationVersion: AppVersion,
		DatabaseURL:        url,
		// DBOS narrates its own startup at INFO, which buries the harness's
		// event stream under connection strings and version hashes. Warn
		// keeps the terminal about the agent; -dbos-inspect is where you go
		// to ask the engine about itself.
		Logger: slog.New(quiet),
	})
	if err != nil {
		return nil, nil, fmt.Errorf("dbos: %w", err)
	}

	dbos.RegisterWorkflow(dctx, work)
	if err := dbos.Launch(dctx); err != nil {
		return nil, nil, fmt.Errorf("dbos launch: %w", err)
	}
	return dctx, func() {
		// Silence the engine BEFORE asking it to stop. Cancelling the context
		// makes the queue runner log its in-flight work as failed — "context
		// canceled", at WARN and ERROR — which is true and completely
		// uninteresting, and reads like a crash to anyone watching. We only
		// drop what is logged after we asked it to stop; anything that goes
		// wrong while it is running still reaches you.
		quiet.silence()
		_ = dbos.Shutdown(dctx, 5*time.Second)
	}, nil
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

// Run works one task with DBOS holding the checkpoints.
func Run(ctx context.Context, opt Options, task string) (string, error) {
	dctx, closeDBOS, err := Connect(ctx, opt)
	if err != nil {
		return "", err
	}
	defer closeDBOS()

	handle, err := dbos.RunWorkflow(dctx, work, task)
	if err != nil {
		return "", err
	}
	return handle.GetResult()
}

// Recover launches the engine and does nothing else.
//
// This is the moment worth pausing on. It takes no task and no workflow id: it
// only connects. DBOS finds every PENDING workflow in Postgres, resumes it in
// the background from the exact step where the process died, and the drafts
// and the sends go out. None of the pre-crash tool calls run twice.
func Recover(ctx context.Context, opt Options, wait time.Duration) error {
	_, closeDBOS, err := Connect(ctx, opt)
	if err != nil {
		return err
	}
	defer closeDBOS()

	fmt.Printf("launched — DBOS is recovering anything left PENDING (waiting %s)\n", wait)
	time.Sleep(wait)
	return nil
}

// work is the agent loop, with DBOS holding the checkpoints.
//
// Compare it to agent.Agent.loop: the shape is the same because the idea is
// the same — think, act, repeat, and make sure the two things that must not
// happen twice are steps.
func work(ctx dbos.Context, task string) (string, error) {
	working := []agent.Msg{
		{Role: "system", Text: agent.SystemPrompt},
		{Role: "user", Text: task},
	}

	for i := 0; i < maxSteps; i++ {
		maybeCrash(i)

		// The model call is a step. Not because it has a side effect on the
		// world, but because it costs money and because a replay that asked a
		// second time would get different tool call ids and diverge.
		reply, err := dbos.RunAsStep(ctx, func(c context.Context) (agent.Msg, error) {
			return agent.Think(c, deps.client, deps.model, working, deps.registry.Specs())
		}, dbos.WithStepName(fmt.Sprintf("model-%02d", i)))
		if err != nil {
			return "", err
		}
		working = append(working, reply)

		events.Emit(deps.bus, events.Event{
			Type: events.ModelCompleted, Workflow: workflowID(ctx),
			Output: strings.TrimSpace(reply.Text),
		})

		// No tool calls means the model is answering rather than acting.
		if len(reply.ToolCalls) == 0 {
			return reply.Text, nil
		}

		for _, call := range reply.ToolCalls {
			// The tool is a step for the reason the whole week exists: this is
			// the half that touches the world, and a crash between running it
			// and recording it is how a customer gets two emails.
			//
			// The events are emitted INSIDE the step, exactly as they are in
			// the agent's own loop, so a replay does not re-emit them and the
			// log keeps meaning what -audit thinks it means.
			result, err := dbos.RunAsStep(ctx, func(c context.Context) (string, error) {
				events.Emit(deps.bus, events.Event{
					Type: events.ToolRequested, Workflow: workflowID(ctx),
					Name: call.Name, Call: call.ID, Args: call.Args,
				})
				out := deps.registry.Dispatch(c, call.Name, call.Args)
				events.Emit(deps.bus, events.Event{
					Type: events.ToolCompleted, Workflow: workflowID(ctx),
					Name: call.Name, Call: call.ID,
				})
				return out, nil
			}, dbos.WithStepName("tool-"+call.ID))
			if err != nil {
				return "", err
			}
			working = append(working, agent.Msg{Role: "tool", Text: result, ToolCallID: call.ID})
		}
	}
	return "", fmt.Errorf("hit the step limit after %d tool rounds", maxSteps)
}

// maybeCrash is the demo hook. Unlike the agent's own, it does not need to
// check whether the step was already done: DBOS serves a completed step from
// Postgres before the body reaches this line on a replay… which is precisely
// why it does NOT — the body runs from the top every time, and it is the step
// calls that short-circuit. So we count crashes instead, and fire once.
func maybeCrash(i int) {
	if deps.crashAt < 0 || i != deps.crashAt || crashed {
		return
	}
	crashed = true
	fmt.Printf("\n  💥 simulated crash before step %d — the process is gone.\n", i)
	os.Exit(1)
}

var crashed bool

// workflowID labels events with the run they belong to, so a DBOS run reads
// the same way in the terminal as a run on the repo's own engine.
func workflowID(ctx dbos.Context) string {
	id, err := dbos.GetWorkflowID(ctx)
	if err != nil {
		return ""
	}
	return id
}
