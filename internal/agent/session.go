package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/aliworkshop/ai-engineering-course/internal/durable"
)

// Session is a conversation: everything said so far, plus a way to say more.
//
// Each turn is still its own workflow — durability did not change. What a
// session adds is that the conversation travels INTO the next workflow, where
// it is checkpointed with everything else. A session is a convenience for the
// person typing; it is not a place state hides.
type Session struct {
	ag   *Agent
	seed []Msg
}

// Session starts an empty conversation.
func (a *Agent) Session() *Session { return &Session{ag: a} }

// Ask works one task with the conversation so far in front of it, and folds
// the exchange into the conversation for next time.
//
// Only the question and the final answer are kept, not the tool traffic in
// between. A follow-up needs to know what was decided, not every call that got
// there — and the full transcript of a turn is already on disk if anyone wants
// it, which is exactly what Continue reads.
func (s *Session) Ask(ctx context.Context, text string) (string, error) {
	answer, err := s.ag.run(ctx, newWorkflowID(), text, s.seed)
	if err != nil {
		// The turn did not happen, so the conversation did not move.
		return "", err
	}
	s.seed = append(s.seed,
		Msg{Role: "user", Text: text},
		Msg{Role: "assistant", Text: answer},
	)
	return answer, nil
}

// Continue picks a finished workflow back up as a conversation, rebuilt from
// its checkpoints.
//
// Nothing was kept in memory to make this work: the run may have been executed
// by a process that died an hour ago. Every model turn and every tool result
// was written down because they HAD to be, in order to replay — and that same
// record is a transcript. Reading it back is the whole implementation.
func (a *Agent) Continue(workflowID string) (*Session, error) {
	session := a.Session()
	if workflowID == "" || a.store == nil {
		return session, nil
	}

	row, steps, err := a.store.Read(workflowID)
	if err != nil {
		return nil, fmt.Errorf("agent: reading workflow %s: %w", workflowID, err)
	}

	session.seed = append(session.seed, storedSeed(steps)...)
	session.seed = append(session.seed, Msg{Role: "user", Text: row.Input})
	session.seed = append(session.seed, transcript(steps)...)
	return session, nil
}

// storedSeed is the conversation the crashed run was itself working from, so a
// recovered conversation is the whole thread rather than its last turn.
func storedSeed(steps []durable.StepInfo) []Msg {
	for _, step := range steps {
		if step.Name != "seed" {
			continue
		}
		var seed []Msg
		if err := json.Unmarshal(step.Output, &seed); err != nil {
			return nil
		}
		return seed
	}
	return nil
}

// transcript turns checkpointed steps back into messages.
//
// The step names carry the structure: model-NN holds an assistant turn,
// tool-<call id> holds that call's result, and the bookkeeping steps hold
// nothing worth saying. Order is the order they completed, which is the order
// they happened.
//
// A step we cannot decode is skipped rather than fatal, with one exception
// enforced below: an assistant message asking for tools is only legal if a
// result for EACH of them follows it. Rebuilding half a pair produces a
// conversation the next model call refuses outright, so a model turn whose
// results are missing is dropped rather than emitted alone.
func transcript(steps []durable.StepInfo) []Msg {
	results := map[string]string{}
	for _, step := range steps {
		id, ok := strings.CutPrefix(step.Name, "tool-")
		if !ok {
			continue
		}
		var out string
		if json.Unmarshal(step.Output, &out) == nil {
			results[id] = out
		}
	}

	var out []Msg
	for _, step := range steps {
		if !strings.HasPrefix(step.Name, "model-") {
			continue
		}
		var reply Msg
		if err := json.Unmarshal(step.Output, &reply); err != nil {
			continue
		}

		answers := make([]Msg, 0, len(reply.ToolCalls))
		complete := true
		for _, call := range reply.ToolCalls {
			result, ok := results[call.ID]
			if !ok {
				complete = false
				break
			}
			answers = append(answers, Msg{Role: "tool", Text: result, ToolCallID: call.ID})
		}
		if !complete {
			continue
		}

		out = append(out, reply)
		out = append(out, answers...)
	}
	return out
}
