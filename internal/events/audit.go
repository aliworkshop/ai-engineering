package events

import (
	"bufio"
	"encoding/json"
	"os"
	"sort"
)

// Audit reads the event log back and answers the question the durable harness
// is built around: did a crash ever cause a side effect to happen twice?
//
// The log already holds the answer — this is a read, not an instrument. That
// is the quiet dividend of making everything an event: because each line
// carries a type, a workflow and the model's own call id, the durability claim
// is checkable with a hundred lines and no new machinery.
//
// The measurement is tool.requested, grouped by call id, and the reason it
// works is a decision made in the agent: those events are emitted from INSIDE
// the tool's checkpointed step. A replay does not re-run the step, so it does
// not re-emit them. One line per call id therefore means the tool ran once,
// ever — and two means a customer got two emails.
//
// A tool NAME repeating is normal: a model may ask for sendReply three times
// in one task, for three different items. A CALL repeating is the bug.

// Report is what one pass over the log found.
type Report struct {
	Path      string
	Total     int
	Workflows int

	ByType     []TypeCount
	Calls      int // distinct tool calls that actually executed
	Replayed   int // steps served from a checkpoint instead of running
	Duplicated []Duplicate
}

// TypeCount is how many of one event type the log holds.
type TypeCount struct {
	Type  string
	Count int
}

// Duplicate is one tool call that ran more than once — a repeated side effect.
type Duplicate struct {
	Call     string
	Name     string
	Workflow string
	Count    int
}

// Audit parses the log at path. A half-written final line is counted rather
// than fatal: an audit that refuses to run because the log is torn is useless
// at exactly the moment it is needed, which is right after a crash.
func Audit(path string) (Report, error) {
	f, err := os.Open(path)
	if err != nil {
		return Report{}, err
	}
	defer f.Close()

	report := Report{Path: path}
	byType := map[string]int{}
	workflows := map[string]bool{}
	requests := map[string]*Duplicate{}

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		report.Total++

		var e Event
		if err := json.Unmarshal(line, &e); err != nil {
			continue // a torn line is one we cannot read, not a reason to stop
		}
		byType[e.Type]++
		if e.Workflow != "" {
			workflows[e.Workflow] = true
		}

		switch e.Type {
		case StepCached:
			report.Replayed++
		case ToolRequested:
			if e.Call == "" {
				continue
			}
			if seen, ok := requests[e.Call]; ok {
				seen.Count++
				continue
			}
			requests[e.Call] = &Duplicate{
				Call: e.Call, Name: e.Name, Workflow: e.Workflow, Count: 1,
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return Report{}, err
	}

	report.Workflows = len(workflows)
	report.Calls = len(requests)
	for _, d := range requests {
		if d.Count > 1 {
			report.Duplicated = append(report.Duplicated, *d)
		}
	}
	sort.Slice(report.Duplicated, func(i, j int) bool {
		return report.Duplicated[i].Count > report.Duplicated[j].Count
	})

	for name, count := range byType {
		report.ByType = append(report.ByType, TypeCount{Type: name, Count: count})
	}
	sort.Slice(report.ByType, func(i, j int) bool { return report.ByType[i].Type < report.ByType[j].Type })

	return report, nil
}

// Clean reports whether the log shows no repeated side effects.
func (r Report) Clean() bool { return len(r.Duplicated) == 0 }
