// Package dbosrun runs the same agent on somebody else's durable engine.
//
// internal/durable is about forty lines: a workflow is a JSON file, a step is a
// named entry in it, and resuming means running the body again and reading the
// completed steps out of the cache. Having built that, it is worth seeing the
// industrial version — because the point is not that DBOS is better, it is that
// the thing in internal/durable is the real idea, and here it is with the
// volume turned up.
//
// The mapping is nearly line for line:
//
//	internal/durable                      DBOS Transact
//	---------------------------------------------------------------------
//	durable.Store + .harness/wf/*.json    dbos.NewContext + Postgres tables
//	durable.Step(wf, name, fn)            dbos.RunAsStep(ctx, fn, WithStepName)
//	store.Resume / the -resume flag       automatic recovery inside dbos.Launch
//	events.jsonl + `-audit`               ListWorkflows / workflow steps in SQL
//	(we don't have)                       queues, timeouts, fork-from-step, cancel
//
// The golden rule is identical in both: the workflow body must be
// deterministic, and everything non-deterministic — every model call, every
// tool — has to happen inside a step, or replay will not match.
//
// What this costs is the honest part. The agent's own harness needs two
// dependencies and a directory; this needs a database, a driver, and a library
// that pulls thirty more. That trade is exactly why the hand-rolled version is
// worth understanding first.
package dbosrun

import (
	"context"
	"fmt"
	"os"
	"strings"
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

// maxSteps stops a loop that will not converge, exactly as the agent's own loop
// does. A runaway agent is a billing incident.
const maxSteps = 12

// The workflow body has to be a plain function DBOS can register and, later,
// recover on its own — it cannot close over a client passed in at call time,
// because recovery happens inside Launch with no caller in sight. So the
// dependencies live here, set once before Launch. It reads like a global
// because it is one; the alternative is a workflow DBOS cannot resurrect.
var (
	deps struct {
		client   *openrouter.OpenRouter
		model    string
		registry *tools.Registry
		bus      events.Emitter
	}
)

// Run answers one question with the teacher, checkpointing every model call and
// every tool call into Postgres through DBOS.
func Run(ctx context.Context, client *openrouter.OpenRouter, model string, registry *tools.Registry, bus events.Emitter, question string) (string, error) {
	url := os.Getenv(DatabaseEnv)
	if url == "" {
		return "", fmt.Errorf("%s is not set — DBOS keeps its state in Postgres, e.g. postgres://user@localhost:5432/agent_dbos", DatabaseEnv)
	}

	deps.client, deps.model, deps.registry, deps.bus = client, model, registry, bus

	dctx, err := dbos.NewContext(ctx, dbos.Config{
		AppName:     "english-teacher",
		DatabaseURL: url,
	})
	if err != nil {
		return "", fmt.Errorf("dbos: %w", err)
	}
	defer func() { _ = dbos.Shutdown(dctx, 5*time.Second) }()

	// Registration before launch, always: Launch is also what starts recovery,
	// and it can only resurrect workflows whose function it can find by name.
	dbos.RegisterWorkflow(dctx, teach)
	if err := dbos.Launch(dctx); err != nil {
		return "", fmt.Errorf("dbos launch: %w", err)
	}

	handle, err := dbos.RunWorkflow(dctx, teach, question)
	if err != nil {
		return "", err
	}
	return handle.GetResult()
}

// teach is the agent loop, with DBOS holding the checkpoints.
//
// Compare it to agent.Ask: the shape is the same because the idea is the same —
// think, act, repeat, and make sure the two things that must not happen twice
// are steps.
func teach(ctx dbos.Context, question string) (string, error) {
	working := []agent.Msg{
		{Role: "system", Text: agent.TeacherPrompt},
		{Role: "user", Text: question},
	}

	for step := 0; step < maxSteps; step++ {
		// The model call is a step. Not because it has a side effect on the
		// world, but because it costs money and because a replay that asked a
		// second time would get different tool call ids and diverge.
		reply, err := dbos.RunAsStep(ctx, func(c context.Context) (agent.Msg, error) {
			return agent.Think(c, deps.client, deps.model, working, deps.registry.Specs())
		}, dbos.WithStepName(fmt.Sprintf("model-%02d", step)))
		if err != nil {
			return "", err
		}
		working = append(working, reply)

		events.Emit(deps.bus, events.Event{
			Type: events.ModelCompleted, Workflow: workflowID(ctx),
			Agent: agent.TeacherName, Text: strings.TrimSpace(reply.Text),
		})

		// No tool calls means the model is answering rather than acting, and
		// the loop is done.
		if len(reply.ToolCalls) == 0 {
			return reply.Text, nil
		}

		for _, call := range reply.ToolCalls {
			events.Emit(deps.bus, events.Event{
				Type: events.ToolRequested, Workflow: workflowID(ctx),
				Agent: agent.TeacherName, Name: call.Name, Call: call.ID, Args: call.Args,
			})

			// The tool is a step for the reason the whole course exists: this
			// is the half that touches the world, and a crash between running
			// it and recording it is how a side effect happens twice.
			result, err := dbos.RunAsStep(ctx, func(c context.Context) (string, error) {
				return deps.registry.Dispatch(c, call.Name, call.Args), nil
			}, dbos.WithStepName("tool-"+call.ID))
			if err != nil {
				return "", err
			}

			events.Emit(deps.bus, events.Event{
				Type: events.ToolCompleted, Workflow: workflowID(ctx),
				Agent: agent.TeacherName, Name: call.Name, Call: call.ID,
			})
			working = append(working, agent.Msg{Role: "tool", Text: result, ToolCallID: call.ID})
		}
	}
	return "", fmt.Errorf("stopped after %d steps without an answer", maxSteps)
}

// workflowID labels events with the run they belong to, so a DBOS run reads the
// same way in the terminal as a run on the repo's own engine.
func workflowID(ctx dbos.Context) string {
	id, err := dbos.GetWorkflowID(ctx)
	if err != nil {
		return ""
	}
	return id
}
