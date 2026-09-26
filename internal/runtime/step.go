package runtime

import (
	"context"

	"github.com/dbos-inc/dbos-transact-golang/dbos"

	"github.com/aliworkshop/ai-engineering-course/internal/agent"
)

// stepper is how the loop runs a named unit of work without caring whether it
// is being checkpointed.
//
// Two methods rather than one generic method, because Go does not allow type
// parameters on interface methods and the loop only ever checkpoints two
// things: a model turn and a tool result. The alternative — everything through
// []byte and re-asserted by the caller — trades a duplicated signature for a
// lost type, which is the worse deal.
type stepper interface {
	// id names the run every event in it is tagged with.
	id() string

	msg(name string, fn func(context.Context) (agent.Msg, error)) (agent.Msg, error)
	text(name string, fn func(context.Context) (string, error)) (string, error)
}

// dbosSteps checkpoints into Postgres. A completed step returns its stored
// result and fn is never called, which is the whole of durable execution.
type dbosSteps struct{ ctx dbos.Context }

func (s dbosSteps) id() string {
	id, err := dbos.GetWorkflowID(s.ctx)
	if err != nil {
		return ""
	}
	return id
}

func (s dbosSteps) msg(name string, fn func(context.Context) (agent.Msg, error)) (agent.Msg, error) {
	return dbos.RunAsStep(s.ctx, fn, dbos.WithStepName(name))
}

func (s dbosSteps) text(name string, fn func(context.Context) (string, error)) (string, error) {
	return dbos.RunAsStep(s.ctx, fn, dbos.WithStepName(name))
}

// plainSteps checkpoints nothing: it runs the work and hands back the result.
//
// This is the whole of the undurable path, and its smallness is the point. The
// loop above does not branch on durability — the same body, the same steps, in
// the same order. What changes is only whether finishing a step writes
// anything down, and therefore whether a crash costs a replay or a repeat.
type plainSteps struct {
	ctx context.Context
	wid string
}

func (s plainSteps) id() string { return s.wid }

func (s plainSteps) msg(_ string, fn func(context.Context) (agent.Msg, error)) (agent.Msg, error) {
	return fn(s.ctx)
}

func (s plainSteps) text(_ string, fn func(context.Context) (string, error)) (string, error) {
	return fn(s.ctx)
}
