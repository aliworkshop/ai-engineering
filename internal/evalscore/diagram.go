// Package evalscore holds the graded dataset and the scoring engine for the
// diagram evals.
//
// It exists because scoring logic is not test logic. These functions decide
// whether a diagram is *good* — they parse what landed on disk, compare it
// against what was asked for, and hand back a number — and that judgement is
// worth more than one caller. It has two: the Go eval in internal/agent, and
// the Braintrust eval in the evals/ module, which is a separate module and so
// cannot see a _test.go file at all.
//
// Keeping one copy is the whole point. Two implementations of "is this diagram
// right?" would drift within a week, and the first symptom would be two
// dashboards disagreeing about the same run.
//
// Everything here is pure: no model, no network, no filesystem. You hand it a
// Case and a RunResult and it returns scores. Whoever produced the RunResult —
// a Go test, a Braintrust task — is not its problem.
package evalscore

import (
	"encoding/json"
	"regexp"
	"strconv"
	"strings"
)

// The artifacts the diagram tools write, relative to the working directory.
const (
	SpecFile  = "canvas.diagram.json"
	SVGFile   = "canvas.svg"
	SceneFile = "canvas.excalidraw"

	// PassingScore is the bar a single dimension has to clear. Below it, that
	// dimension is treated as a failure of the case.
	PassingScore = 0.5

	// structureFloor stops proportional credit going negative when a count
	// overshoots wildly. Zero is as bad as it gets.
	structureFloor = 0.0
)

// ---------- the dataset ----------

// Case is one graded diagram task. A case declares only what applies to it; the
// scorers return nil for everything it leaves empty.
type Case struct {
	Name   string
	Prompt string

	// Seed, when set, is drawn before the agent runs — this is what makes a
	// case a "modify" case. The agent is then asked to change it.
	Seed string

	// Expect describes the shape of a freshly created diagram in the form the
	// structure scorer parses, e.g. "3 rectangle elements, 2 arrow elements".
	// Empty on modify cases.
	//
	// These are calibrated against what a *correct* answer actually looks like,
	// not what seems tidy on paper — a first pass guessed "2 arrows" for a
	// three-step pipeline and marked the agent down for adding the Start/End
	// terminators any flowchart would have. Where an exact count matters, the
	// prompt says so; where the prompt is open-ended, the expectation is the
	// shape of a good answer and proportional credit absorbs the variance.
	Expect string

	// Survives lists seed element ids that must still be on the canvas
	// afterwards. Empty means "all of them" — set it explicitly when the prompt
	// asks for a removal.
	Survives []string

	// Keywords is the domain vocabulary that should appear in the diagram's
	// labels or in the agent's reply. Empty means the keyword scorer abstains.
	Keywords []string
}

// SeedSignup is the diagram the modify cases start from. Ids are stable, which
// is what the preservation scorer keys on.
const SeedSignup = `{
  "title": "User signup",
  "elements": [
    {"type":"box","id":"start","label":"Start","shape":"ellipse"},
    {"type":"box","id":"form","label":"User submits signup form"},
    {"type":"box","id":"validate","label":"Valid email and password?","shape":"diamond"},
    {"type":"box","id":"error","label":"Show validation errors"},
    {"type":"box","id":"create","label":"Create account"},
    {"type":"box","id":"done","label":"Signed up","shape":"ellipse"},
    {"type":"arrow","from":"start","to":"form"},
    {"type":"arrow","from":"form","to":"validate"},
    {"type":"arrow","from":"validate","to":"create","label":"yes"},
    {"type":"arrow","from":"validate","to":"error","label":"no"},
    {"type":"arrow","from":"error","to":"form"},
    {"type":"arrow","from":"create","to":"done"}
  ]
}`

// Cases is the graded dataset.
func Cases() []Case {
	return []Case{
		// ---- create ----
		{
			Name:     "create/signup-flowchart",
			Prompt:   "Draw a flowchart of user signup.",
			Expect:   "4 rectangle elements, 2 ellipse elements, 1 diamond element, 6 arrow elements",
			Keywords: []string{"sign", "account"},
		},
		{
			// The one case that pins the structure exactly, so structure is
			// measuring the agent rather than the looseness of the prompt.
			Name:   "create/ci-pipeline",
			Prompt: "Draw a diagram of a CI pipeline with exactly three steps — build, test, deploy — connected in order, with Start and End terminators.",
			Expect: "3 rectangle elements, 2 ellipse elements, 4 arrow elements",
			// A pipeline diagram that doesn't say "build"/"test"/"deploy" has
			// missed the request regardless of how well-formed it is.
			Keywords: []string{"build", "test", "deploy"},
		},
		{
			Name:   "create/login-retry",
			Prompt: "Draw a flowchart for a login attempt that loops back and lets the user try again when the password is wrong.",
			// Open-ended, so this is the shape of a good answer rather than the
			// only one: a credentials check plus a try-again branch. Proportional
			// credit absorbs a model that merges them into one decision.
			Expect:   "2 diamond elements, 6 arrow elements",
			Keywords: []string{"password", "login"},
		},

		// ---- modify ----
		{
			Name:     "modify/rename",
			Prompt:   `Rename the "Create account" box to "Provision account".`,
			Seed:     SeedSignup,
			Keywords: []string{"provision"},
		},
		{
			Name:   "modify/add-step",
			Prompt: `Add a "Send welcome email" step between creating the account and being signed up, wiring the arrows through it.`,
			Seed:   SeedSignup,
			// Everything seeded stays; the new step is inserted, not swapped in.
			Keywords: []string{"welcome"},
		},
		{
			Name:   "modify/remove-branch",
			Prompt: `Remove the validation error box and every arrow attached to it.`,
			Seed:   SeedSignup,
			// "error" is the one thing allowed to disappear.
			Survives: []string{"start", "form", "validate", "create", "done"},
		},
	}
}

// ---------- what a run produced ----------

// Spec mirrors the spec file the diagram tools save. It's declared here rather
// than imported from the tools so the eval grades the artifact a user would
// inspect, not the tool's internal types.
type Spec struct {
	Title    string    `json:"title"`
	Elements []Element `json:"elements"`
}

type Element struct {
	Type  string `json:"type"`
	ID    string `json:"id"`
	Label string `json:"label"`
	Shape string `json:"shape"`
	From  string `json:"from"`
	To    string `json:"to"`
}

func (e Element) IsArrow() bool {
	if t := strings.ToLower(strings.TrimSpace(e.Type)); t == "arrow" {
		return true
	}
	return e.From != "" && e.To != ""
}

// ElementID is how the element is addressed, matching the tools' own convention.
func (e Element) ElementID() string {
	if e.ID != "" {
		return e.ID
	}
	if e.IsArrow() {
		return e.From + "->" + e.To
	}
	return ""
}

// RunResult is everything a scorer gets to look at.
type RunResult struct {
	Reply     string
	Spec      Spec
	SpecRead  bool // false when no diagram was produced at all
	SVGOK     bool // canvas.svg exists and is well-formed XML
	SceneOK   bool
	ToolsUsed []string
	Err       error
}

// Labels is the diagram's text, for the keyword scorer.
func (r RunResult) Labels() string {
	var b strings.Builder
	b.WriteString(r.Spec.Title)
	for _, e := range r.Spec.Elements {
		b.WriteByte(' ')
		b.WriteString(e.Label)
	}
	return b.String()
}

// ---------- the scorers ----------

// Scorer grades one dimension of one run. Returning nil means "this dimension
// doesn't apply to this case" — that's what lets one scorer set cover a mixed
// dataset without every case having to declare every field.
type Scorer struct {
	Name  string
	Score func(c Case, r RunResult) *float64
}

// Score boxes a float, so a scorer can distinguish zero from "not applicable".
func Score(f float64) *float64 { return &f }

// All is every dimension, in report order.
func All() []Scorer {
	return []Scorer{
		{"schema", ScoreSchema},
		{"structure", ScoreStructure},
		{"preservation", ScorePreservation},
		{"keywords", ScoreKeywords},
	}
}

// ScoreSchema asks whether what landed on disk is a real diagram: it parses, it
// has boxes, every box is uniquely identified, and every arrow connects two
// boxes that exist. Applies to every case — this is the scorer that catches the
// agent producing nothing, or producing garbage.
func ScoreSchema(_ Case, r RunResult) *float64 {
	// Nothing drawn, or the renderings are unusable: no partial credit. A
	// diagram a browser won't open is worth the same as no diagram.
	if !r.SpecRead || !r.SVGOK || !r.SceneOK || len(r.Spec.Elements) == 0 {
		return Score(0)
	}

	boxes := map[string]bool{}
	for _, e := range r.Spec.Elements {
		if !e.IsArrow() && e.ID != "" {
			boxes[e.ID] = true
		}
	}
	if len(boxes) == 0 {
		return Score(0) // arrows with nothing to connect isn't a diagram
	}

	seen := map[string]bool{}
	valid := 0
	for _, e := range r.Spec.Elements {
		ok := false
		switch {
		case e.IsArrow():
			ok = boxes[strings.TrimSpace(e.From)] && boxes[strings.TrimSpace(e.To)]
		default:
			// A box needs an id nothing else claimed, and something to show.
			ok = e.ID != "" && !seen[e.ID] && strings.TrimSpace(e.Label) != ""
			seen[e.ID] = true
		}
		if ok {
			valid++
		}
	}
	return Score(float64(valid) / float64(len(r.Spec.Elements)))
}

// ScoreStructure compares the case's expected characteristics against what was
// actually drawn, with proportional credit per element kind — a flowchart with
// 5 boxes where 4 were asked for is mostly right, not wrong. Abstains on modify
// cases, which declare no expected shape.
func ScoreStructure(c Case, r RunResult) *float64 {
	if strings.TrimSpace(c.Expect) == "" {
		return nil // not a create case
	}
	if !r.SpecRead {
		return Score(0)
	}

	want := parseExpectation(c.Expect)
	if len(want) == 0 {
		return nil // nothing parseable to compare against
	}
	got := actualCounts(r.Spec)

	total := 0.0
	for kind, n := range want {
		total += proportionalCredit(n, got[kind])
	}
	return Score(total / float64(len(want)))
}

// expectationRE pulls "3 rectangle elements" / "1 diamond element" out of a
// case's expected characteristics.
var expectationRE = regexp.MustCompile(`(?i)(\d+)\s+([a-z]+)\s+elements?`)

// parseExpectation turns "3 rectangle elements, 2 arrow elements" into
// {rectangle: 3, arrow: 2}, normalising the synonyms a human might write.
func parseExpectation(expect string) map[string]int {
	want := map[string]int{}
	for _, m := range expectationRE.FindAllStringSubmatch(expect, -1) {
		n, err := strconv.Atoi(m[1])
		if err != nil || n <= 0 {
			continue
		}
		if kind := normalizeKind(m[2]); kind != "" {
			want[kind] += n
		}
	}
	return want
}

func normalizeKind(word string) string {
	switch strings.ToLower(word) {
	case "rectangle", "rectangles", "rect", "box", "boxes", "step", "steps":
		return "rectangle"
	case "ellipse", "ellipses", "oval", "ovals", "terminator", "terminators":
		return "ellipse"
	case "diamond", "diamonds", "decision", "decisions":
		return "diamond"
	case "arrow", "arrows", "edge", "edges", "connection", "connections":
		return "arrow"
	default:
		return ""
	}
}

// actualCounts tallies the drawn diagram by the same vocabulary.
func actualCounts(spec Spec) map[string]int {
	got := map[string]int{}
	for _, e := range spec.Elements {
		if e.IsArrow() {
			got["arrow"]++
			continue
		}
		switch strings.ToLower(strings.TrimSpace(e.Shape)) {
		case "ellipse", "oval":
			got["ellipse"]++
		case "diamond", "decision":
			got["diamond"]++
		default:
			got["rectangle"]++
		}
	}
	return got
}

// proportionalCredit is 1 when the count matches and falls off linearly with the
// miss, so "close" scores better than "nowhere near" and neither is a pass/fail
// cliff. Overshooting is penalised the same as undershooting.
func proportionalCredit(want, got int) float64 {
	if want <= 0 {
		return 0
	}
	miss := got - want
	if miss < 0 {
		miss = -miss
	}
	credit := 1 - float64(miss)/float64(want)
	if credit < structureFloor {
		return structureFloor
	}
	return credit
}

// ScorePreservation asks the question a modify case exists to ask: did the edit
// touch only what it was meant to, or did the agent redraw the whole thing from
// scratch and lose the rest? Abstains on create cases, which have no seed.
func ScorePreservation(c Case, r RunResult) *float64 {
	if c.Seed == "" {
		return nil // not a modify case
	}
	if !r.SpecRead {
		return Score(0) // canvas gone entirely: the worst outcome
	}

	expected := c.Survives
	if len(expected) == 0 {
		// No explicit list: everything seeded should still be there.
		var seeded Spec
		if err := json.Unmarshal([]byte(c.Seed), &seeded); err != nil {
			return nil
		}
		for _, e := range seeded.Elements {
			if !e.IsArrow() && e.ElementID() != "" {
				expected = append(expected, e.ElementID())
			}
		}
	}
	if len(expected) == 0 {
		return nil
	}

	present := map[string]bool{}
	for _, e := range r.Spec.Elements {
		if id := e.ElementID(); id != "" {
			present[id] = true
		}
	}
	survived := 0
	for _, id := range expected {
		if present[id] {
			survived++
		}
	}
	return Score(float64(survived) / float64(len(expected)))
}

// ScoreKeywords checks that the domain vocabulary actually shows up — in the
// diagram's labels or in what the agent said. A structurally perfect diagram
// about the wrong subject should not score well. Abstains unless the case
// declares keywords.
func ScoreKeywords(c Case, r RunResult) *float64 {
	if len(c.Keywords) == 0 {
		return nil
	}
	haystack := strings.ToLower(r.Labels() + " " + r.Reply)
	found := 0
	for _, kw := range c.Keywords {
		if strings.Contains(haystack, strings.ToLower(kw)) {
			found++
		}
	}
	return Score(float64(found) / float64(len(c.Keywords)))
}
