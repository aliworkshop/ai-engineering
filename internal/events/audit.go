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
//
// Judgments are counted the same way and reported separately, because they are
// a different kind of repeat. Nobody is emailed twice when a judgment runs
// again — you are simply billed twice for a decision you had already made and
// written down, and after a crash that is the thing most worth proving did not
// happen. The key is jev.requested's call id, which names the DECISION rather
// than the question: "verify item-3" recurring is a redraft being checked, and
// that is the gate working, not a replay leaking.

// Report is what one pass over the log found.
type Report struct {
	Path      string
	Total     int
	Workflows int

	ByType     []TypeCount
	Calls      int // distinct tool calls that actually executed
	Replayed   int // steps served from a checkpoint instead of running
	Duplicated []Duplicate

	Judgments int // distinct judgments that were actually bought
	Rejudged  []Duplicate
}

// TypeCount is how many of one event type the log holds.
type TypeCount struct {
	Type  string
	Count int
}

// Duplicate is one call that happened more than once — a repeated side effect,
// or a judgment paid for twice.
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
	judgments := map[string]*Duplicate{}

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
			tally(requests, e)
		case JevRequested:
			tally(judgments, e)
		}
	}
	if err := scanner.Err(); err != nil {
		return Report{}, err
	}

	report.Workflows = len(workflows)
	report.Calls = len(requests)
	report.Judgments = len(judgments)
	report.Duplicated = repeats(requests)
	report.Rejudged = repeats(judgments)

	for name, count := range byType {
		report.ByType = append(report.ByType, TypeCount{Type: name, Count: count})
	}
	sort.Slice(report.ByType, func(i, j int) bool { return report.ByType[i].Type < report.ByType[j].Type })

	return report, nil
}

// tally records one request under its call id, which is what makes the tally
// "how many times did THIS one happen" rather than "how many of these were
// there".
func tally(into map[string]*Duplicate, e Event) {
	if e.Call == "" {
		return
	}
	if seen, ok := into[e.Call]; ok {
		seen.Count++
		return
	}
	into[e.Call] = &Duplicate{Call: e.Call, Name: e.Name, Workflow: e.Workflow, Count: 1}
}

// repeats pulls out the ones that happened more than once, worst first.
func repeats(from map[string]*Duplicate) []Duplicate {
	var out []Duplicate
	for _, d := range from {
		if d.Count > 1 {
			out = append(out, *d)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Count > out[j].Count })
	return out
}

// Clean reports whether the log shows no repeated side effects and no judgment
// bought twice.
func (r Report) Clean() bool { return len(r.Duplicated) == 0 && len(r.Rejudged) == 0 }
