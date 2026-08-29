package events

import (
	"bufio"
	"encoding/json"
	"os"
	"sort"
)

// Audit reads the event log back and answers the one question durable execution
// exists to answer: was any unit of work performed twice?
//
// What it counts is step.completed, and the choice matters. A replayed run
// re-REQUESTS its tools — the agent emits tool.requested, then the step comes
// back from the checkpoint — so counting requests would report a duplicate
// every time recovery worked perfectly, which is the exact opposite of the
// claim. A step only completes when it actually ran. Counting those, per
// workflow and step name, is the measurement.
//
// It is deliberately a read over the plain JSONL file rather than a query
// against an index. Keeping the runtime's state in files is what makes an audit
// a hundred lines instead of a subsystem.
type Report struct {
	Path   string         `json:"path"`
	Total  int            `json:"total"`
	ByType map[string]int `json:"by_type"`

	// Executed counts step.completed per workflow and step name — the work that
	// really happened. Anything above one is a repeat.
	Executed   map[string]int `json:"executed"`
	Duplicated []Duplicate    `json:"duplicated"`

	// Replayed counts step.cached: work a resumed run did NOT have to redo, and
	// did not pay a model or a side effect for a second time.
	Replayed int `json:"replayed"`

	// Requested counts tool.requested per workflow and call id. Useful next to
	// Executed rather than instead of it: a call requested twice and executed
	// once is recovery working.
	Requested map[string]int `json:"requested"`

	Workflows map[string]int `json:"workflows"` // events per workflow
	Skipped   int            `json:"skipped"`   // lines that were not events
}

// Duplicate is one unit of work the log saw complete more than once.
type Duplicate struct {
	Workflow string `json:"workflow"`
	Step     string `json:"step"`
	Count    int    `json:"count"`
}

// Audit parses the JSONL log at path.
func Audit(path string) (Report, error) {
	f, err := os.Open(path)
	if err != nil {
		return Report{}, err
	}
	defer f.Close()

	report := Report{
		Path:      path,
		ByType:    map[string]int{},
		Executed:  map[string]int{},
		Requested: map[string]int{},
		Workflows: map[string]int{},
	}
	steps := map[string][2]string{} // key -> workflow, step name

	scan := bufio.NewScanner(f)
	// A tool result can be long, and a truncated line would be counted as a
	// parse failure rather than as the event it is.
	scan.Buffer(make([]byte, 0, 64<<10), 8<<20)
	for scan.Scan() {
		line := scan.Bytes()
		if len(line) == 0 {
			continue
		}
		var e Event
		if err := json.Unmarshal(line, &e); err != nil || e.Type == "" {
			report.Skipped++
			continue
		}

		report.Total++
		report.ByType[string(e.Type)]++
		if e.Workflow != "" {
			report.Workflows[e.Workflow]++
		}
		switch e.Type {
		case StepCompleted:
			key := e.Workflow + "/" + e.Name
			report.Executed[key]++
			steps[key] = [2]string{e.Workflow, e.Name}
		case StepCached:
			report.Replayed++
		case ToolRequested:
			// A call id is what identifies one request; an event logged before
			// they were recorded has nothing to key on and is left out rather
			// than counted by name, which would invent repeats.
			if e.Call != "" {
				report.Requested[e.Workflow+"/"+e.Call]++
			}
		}
	}
	if err := scan.Err(); err != nil {
		return report, err
	}

	for key, n := range report.Executed {
		if n > 1 {
			report.Duplicated = append(report.Duplicated,
				Duplicate{Workflow: steps[key][0], Step: steps[key][1], Count: n})
		}
	}
	sort.Slice(report.Duplicated, func(i, j int) bool {
		if report.Duplicated[i].Count != report.Duplicated[j].Count {
			return report.Duplicated[i].Count > report.Duplicated[j].Count
		}
		return report.Duplicated[i].Step < report.Duplicated[j].Step
	})
	return report, nil
}

// Clean reports whether nothing ran twice — the whole point of the exercise, as
// one boolean.
func (r Report) Clean() bool { return len(r.Duplicated) == 0 }
