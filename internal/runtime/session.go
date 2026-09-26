package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/dbos-inc/dbos-transact-golang/dbos"

	"github.com/aliworkshop/ai-engineering-course/internal/agent"
)

// Session is a conversation: everything said so far, plus a way to say more.
//
// Each turn is still its own workflow — durability did not change. What a
// session adds is that the conversation travels into the next workflow as part
// of its INPUT, so the run is still rebuildable from what is checkpointed. A
// session is a convenience for the person typing; it is not a place state
// hides.
type Session struct {
	rt   *Runtime
	seed []agent.Msg
}

// Session starts an empty conversation.
func (r *Runtime) Session() *Session { return &Session{rt: r} }

// Ask works one task with the conversation so far in front of it, and folds
// the exchange into the conversation for next time.
//
// Only the question and the final answer are kept, not the tool traffic in
// between. A follow-up needs to know what was decided, not every call that got
// there — and the full transcript of a turn is already in Postgres if anyone
// wants it.
func (s *Session) Ask(ctx context.Context, text string) (string, error) {
	answer, err := s.rt.run(ctx, Task{Seed: s.seed, Text: text})
	if err != nil {
		// The turn did not happen, so the conversation did not move.
		return "", err
	}
	s.seed = append(s.seed,
		agent.Msg{Role: "user", Text: text},
		agent.Msg{Role: "assistant", Text: answer},
	)
	return answer, nil
}

// Continue picks a finished workflow back up as a conversation, rebuilt from
// its checkpoints.
//
// Nothing was kept in memory to make this work: the run may have been executed
// by a process that died days ago. Every model turn and every tool result was
// written down because they had to be, in order to replay — and that same
// record is a transcript. Reading it back is the whole implementation.
func (r *Runtime) Continue(workflowID string) (*Session, error) {
	session := r.Session()
	if workflowID == "" || r.dctx == nil {
		return session, nil
	}

	runs, err := dbos.ListWorkflows(r.dctx,
		dbos.WithFilterWorkflowIDs(workflowID),
		dbos.WithFilterLoadInput(true))
	if err != nil {
		return nil, err
	}
	if len(runs) == 0 {
		return nil, fmt.Errorf("no workflow %s to continue", workflowID)
	}

	var task Task
	if err := decodeStored(runs[0].Input, &task); err != nil {
		return nil, fmt.Errorf("workflow %s: reading its input: %w", workflowID, err)
	}

	steps, err := dbos.GetWorkflowSteps(r.dctx, workflowID)
	if err != nil {
		return nil, err
	}

	session.seed = append(session.seed, task.Seed...)
	session.seed = append(session.seed, agent.Msg{Role: "user", Text: task.Text})
	session.seed = append(session.seed, transcript(steps)...)
	return session, nil
}

// transcript turns checkpointed steps back into messages.
//
// The step names carry the structure: model-NN holds an assistant turn,
// tool-<call id> holds that call's result, and the bookkeeping steps hold
// nothing worth saying. Order is the order they completed, which is the order
// they happened.
//
// A step we cannot decode is skipped rather than fatal, with one exception
// that is enforced below: an assistant message asking for tools must be
// followed by a result for each of them, or the next model call is rejected.
// Dropping half a pair would turn a recovered conversation into an unusable one.
func transcript(steps []dbos.StepInfo) []agent.Msg {
	results := map[string]string{}
	for _, step := range steps {
		if id, ok := strings.CutPrefix(step.StepName, "tool-"); ok {
			var out string
			if decodeStored(step.Output, &out) == nil {
				results[id] = out
			}
		}
	}

	var out []agent.Msg
	for _, step := range steps {
		if !strings.HasPrefix(step.StepName, "model-") {
			continue
		}
		var reply agent.Msg
		if err := decodeStored(step.Output, &reply); err != nil {
			continue
		}

		// Every tool the reply asked for must have an answer here, or the
		// conversation is malformed and the model call that reads it will be
		// refused outright.
		answers := make([]agent.Msg, 0, len(reply.ToolCalls))
		complete := true
		for _, call := range reply.ToolCalls {
			result, ok := results[call.ID]
			if !ok {
				complete = false
				break
			}
			answers = append(answers, agent.Msg{Role: "tool", Text: result, ToolCallID: call.ID})
		}
		if !complete {
			continue
		}

		out = append(out, reply)
		out = append(out, answers...)
	}
	return out
}

// decodeStored reads a value the engine checkpointed for us.
//
// DBOS hands back whatever its serializer produced, which is a JSON string for
// our types — but a future serializer, or a different driver, could hand back
// an already-decoded value instead. Both are handled, because guessing wrong
// here would show up as a conversation that silently forgot half of itself.
func decodeStored(stored any, into any) error {
	switch v := stored.(type) {
	case nil:
		return fmt.Errorf("nothing stored")
	case string:
		return json.Unmarshal([]byte(v), into)
	case []byte:
		return json.Unmarshal(v, into)
	default:
		raw, err := json.Marshal(v)
		if err != nil {
			return err
		}
		return json.Unmarshal(raw, into)
	}
}
