package agent

// Diagram eval harness.
//
// Runs whole diagram tasks through the REAL agent loop and grades what lands on
// the canvas. Unlike the tools package's unit tests — which call the tools
// directly with known-good arguments — this measures the part that can actually
// regress silently: whether the model picks the right tool and fills it in
// sensibly from a plain-English prompt.
//
// Four scorers, each returning a score in 0..1 or nil for "doesn't apply to this
// case". One scorer set therefore covers the whole dataset, and a case only
// declares what's relevant to it:
//
//	schema       — is every element valid? Catches nothing / garbage. All cases.
//	structure    — expected characteristics ("3 rectangle elements") vs actual
//	               counts, proportional credit. Create cases only.
//	preservation — did the seeded diagram survive the edit, or did the agent nuke
//	               the canvas and start over? Modify cases only.
//	keywords     — does the domain vocabulary show up in the labels or the reply?
//	               Only cases that declare keywords.
//
// Run:  go test ./internal/agent -run EvalDiagram -v
// (needs OPENROUTER_API_KEY; skipped with -short)

import (
	"context"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"

	openrouter "github.com/OpenRouterTeam/go-sdk"
	"github.com/joho/godotenv"

	"github.com/aliworkshop/ai-engineering-course/session-2/agent/internal/llm"
	"github.com/aliworkshop/ai-engineering-course/session-2/agent/internal/tools"
)

// The artifacts the diagram tools write, relative to the working directory.
const (
	evalSpecFile   = "canvas.diagram.json"
	evalSVGFile    = "canvas.svg"
	evalSceneFile  = "canvas.excalidraw"
	passingScore   = 0.5 // below this, a scorer is treated as a failure
	structureFloor = 0.0
)

// ---------- the dataset ----------

// diagramCase is one graded diagram task. A case declares only what applies to
// it; the scorers return nil for everything it leaves empty.
type diagramCase struct {
	name   string
	prompt string

	// seed, when set, is drawn before the agent runs — this is what makes a
	// case a "modify" case. The agent is then asked to change it.
	seed string

	// expect describes the shape of a freshly created diagram in the form the
	// structure scorer parses, e.g. "3 rectangle elements, 2 arrow elements".
	// Empty on modify cases.
	//
	// These are calibrated against what a *correct* answer actually looks like,
	// not what seems tidy on paper — a first pass guessed "2 arrows" for a
	// three-step pipeline and marked the agent down for adding the Start/End
	// terminators any flowchart would have. Where an exact count matters, the
	// prompt says so; where the prompt is open-ended, the expectation is the
	// shape of a good answer and proportional credit absorbs the variance.
	expect string

	// survives lists seed element ids that must still be on the canvas
	// afterwards. Empty means "all of them" — set it explicitly when the prompt
	// asks for a removal.
	survives []string

	// keywords is the domain vocabulary that should appear in the diagram's
	// labels or in the agent's reply. Empty means the keyword scorer abstains.
	keywords []string
}

// seedSignup is the diagram modify cases start from. Ids are stable, which is
// what the preservation scorer keys on.
const seedSignup = `{
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

func diagramCases() []diagramCase {
	return []diagramCase{
		// ---- create ----
		{
			name:     "create/signup-flowchart",
			prompt:   "Draw a flowchart of user signup.",
			expect:   "4 rectangle elements, 2 ellipse elements, 1 diamond element, 6 arrow elements",
			keywords: []string{"sign", "account"},
		},
		{
			// The one case that pins the structure exactly, so structure is
			// measuring the agent rather than the looseness of the prompt.
			name:   "create/ci-pipeline",
			prompt: "Draw a diagram of a CI pipeline with exactly three steps — build, test, deploy — connected in order, with Start and End terminators.",
			expect: "3 rectangle elements, 2 ellipse elements, 4 arrow elements",
			// A pipeline diagram that doesn't say "build"/"test"/"deploy" has
			// missed the request regardless of how well-formed it is.
			keywords: []string{"build", "test", "deploy"},
		},
		{
			name:   "create/login-retry",
			prompt: "Draw a flowchart for a login attempt that loops back and lets the user try again when the password is wrong.",
			// Open-ended, so this is the shape of a good answer rather than the
			// only one: a credentials check plus a try-again branch. Proportional
			// credit absorbs a model that merges them into one decision.
			expect:   "2 diamond elements, 6 arrow elements",
			keywords: []string{"password", "login"},
		},

		// ---- modify ----
		{
			name:     "modify/rename",
			prompt:   `Rename the "Create account" box to "Provision account".`,
			seed:     seedSignup,
			keywords: []string{"provision"},
		},
		{
			name:   "modify/add-step",
			prompt: `Add a "Send welcome email" step between creating the account and being signed up, wiring the arrows through it.`,
			seed:   seedSignup,
			// Everything seeded stays; the new step is inserted, not swapped in.
			keywords: []string{"welcome"},
		},
		{
			name:   "modify/remove-branch",
			prompt: `Remove the validation error box and every arrow attached to it.`,
			seed:   seedSignup,
			// "error" is the one thing allowed to disappear.
			survives: []string{"start", "form", "validate", "create", "done"},
		},
	}
}

// ---------- what a run produced ----------

// canvasSpec mirrors the spec file the diagram tools save. It's read here rather
// than imported so the eval grades the artifact a user would inspect, not the
// tool's internal types.
type canvasSpec struct {
	Title    string        `json:"title"`
	Elements []specElement `json:"elements"`
}

type specElement struct {
	Type  string `json:"type"`
	ID    string `json:"id"`
	Label string `json:"label"`
	Shape string `json:"shape"`
	From  string `json:"from"`
	To    string `json:"to"`
}

func (e specElement) isArrow() bool {
	if t := strings.ToLower(strings.TrimSpace(e.Type)); t == "arrow" {
		return true
	}
	return e.From != "" && e.To != ""
}

// id is how the element is addressed, matching the tools' own convention.
func (e specElement) id() string {
	if e.ID != "" {
		return e.ID
	}
	if e.isArrow() {
		return e.From + "->" + e.To
	}
	return ""
}

// runResult is everything a scorer gets to look at.
type runResult struct {
	reply     string
	spec      canvasSpec
	specRead  bool // false when no diagram was produced at all
	svgOK     bool // canvas.svg exists and is well-formed XML
	sceneOK   bool
	toolsUsed []string
	err       error
}

func (r runResult) labels() string {
	var b strings.Builder
	b.WriteString(r.spec.Title)
	for _, e := range r.spec.Elements {
		b.WriteByte(' ')
		b.WriteString(e.Label)
	}
	return b.String()
}

// ---------- the scorers ----------

// scorer grades one dimension of one run. Returning nil means "this dimension
// doesn't apply to this case" — that's what lets one scorer set cover a mixed
// dataset without every case having to declare every field.
type scorer struct {
	name  string
	score func(c diagramCase, r runResult) *float64
}

func score(f float64) *float64 { return &f }

func allScorers() []scorer {
	return []scorer{
		{"schema", scoreSchema},
		{"structure", scoreStructure},
		{"preservation", scorePreservation},
		{"keywords", scoreKeywords},
	}
}

// scoreSchema asks whether what landed on disk is a real diagram: it parses, it
// has boxes, every box is uniquely identified, and every arrow connects two
// boxes that exist. Applies to every case — this is the scorer that catches the
// agent producing nothing, or producing garbage.
func scoreSchema(_ diagramCase, r runResult) *float64 {
	// Nothing drawn, or the renderings are unusable: no partial credit. A
	// diagram a browser won't open is worth the same as no diagram.
	if !r.specRead || !r.svgOK || !r.sceneOK || len(r.spec.Elements) == 0 {
		return score(0)
	}

	boxes := map[string]bool{}
	for _, e := range r.spec.Elements {
		if !e.isArrow() && e.ID != "" {
			boxes[e.ID] = true
		}
	}
	if len(boxes) == 0 {
		return score(0) // arrows with nothing to connect isn't a diagram
	}

	seen := map[string]bool{}
	valid := 0
	for _, e := range r.spec.Elements {
		ok := false
		switch {
		case e.isArrow():
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
	return score(float64(valid) / float64(len(r.spec.Elements)))
}

// scoreStructure compares the case's expected characteristics against what was
// actually drawn, with proportional credit per element kind — a flowchart with
// 5 boxes where 4 were asked for is mostly right, not wrong. Abstains on modify
// cases, which declare no expected shape.
func scoreStructure(c diagramCase, r runResult) *float64 {
	if strings.TrimSpace(c.expect) == "" {
		return nil // not a create case
	}
	if !r.specRead {
		return score(0)
	}

	want := parseExpectation(c.expect)
	if len(want) == 0 {
		return nil // nothing parseable to compare against
	}
	got := actualCounts(r.spec)

	total := 0.0
	for kind, n := range want {
		total += proportionalCredit(n, got[kind])
	}
	return score(total / float64(len(want)))
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
func actualCounts(spec canvasSpec) map[string]int {
	got := map[string]int{}
	for _, e := range spec.Elements {
		if e.isArrow() {
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

// scorePreservation asks the question a modify case exists to ask: did the edit
// touch only what it was meant to, or did the agent redraw the whole thing from
// scratch and lose the rest? Abstains on create cases, which have no seed.
func scorePreservation(c diagramCase, r runResult) *float64 {
	if c.seed == "" {
		return nil // not a modify case
	}
	if !r.specRead {
		return score(0) // canvas gone entirely: the worst outcome
	}

	expected := c.survives
	if len(expected) == 0 {
		// No explicit list: everything seeded should still be there.
		var seeded canvasSpec
		if err := json.Unmarshal([]byte(c.seed), &seeded); err != nil {
			return nil
		}
		for _, e := range seeded.Elements {
			if !e.isArrow() && e.id() != "" {
				expected = append(expected, e.id())
			}
		}
	}
	if len(expected) == 0 {
		return nil
	}

	present := map[string]bool{}
	for _, e := range r.spec.Elements {
		if id := e.id(); id != "" {
			present[id] = true
		}
	}
	survived := 0
	for _, id := range expected {
		if present[id] {
			survived++
		}
	}
	return score(float64(survived) / float64(len(expected)))
}

// scoreKeywords checks that the domain vocabulary actually shows up — in the
// diagram's labels or in what the agent said. A structurally perfect diagram
// about the wrong subject should not score well. Abstains unless the case
// declares keywords.
func scoreKeywords(c diagramCase, r runResult) *float64 {
	if len(c.keywords) == 0 {
		return nil
	}
	haystack := strings.ToLower(r.labels() + " " + r.reply)
	found := 0
	for _, kw := range c.keywords {
		if strings.Contains(haystack, strings.ToLower(kw)) {
			found++
		}
	}
	return score(float64(found) / float64(len(c.keywords)))
}

// ---------- the harness ----------

func TestEvalDiagramTools(t *testing.T) {
	if testing.Short() {
		t.Skip("live eval; skipped in -short mode")
	}
	// Loaded before any t.Chdir below, while the relative path still resolves.
	godotenv.Load("../../.env")
	key := os.Getenv("OPENROUTER_API_KEY")
	if key == "" {
		t.Skip("OPENROUTER_API_KEY not set")
	}
	client := llm.NewOpenRouter(key)

	scorers := allScorers()
	cases := diagramCases()

	// totals[scorer] accumulates only the cases that scorer applied to, so an
	// abstention never drags an average down.
	totals := map[string]float64{}
	counts := map[string]int{}
	var rows []string

	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			// Each case gets its own directory, and the diagram tools write to
			// the working directory — so chdir is what isolates one case's
			// canvas from the next. t.Chdir restores it when the subtest ends.
			t.Chdir(t.TempDir())

			if c.seed != "" {
				if err := seedCanvas(c.seed); err != nil {
					t.Fatalf("seeding the canvas: %v", err)
				}
			}

			r := runCase(t, client, c)
			if r.err != nil {
				t.Errorf("agent error: %v", r.err)
			}

			var cells []string
			for _, s := range scorers {
				v := s.score(c, r)
				if v != nil && r.err != nil {
					// A run that errored scores zero across the board. Without
					// this, a modify case whose agent never got off the ground
					// scores preservation=1.00 for the seed "surviving" — an
					// agent that does nothing preserves everything perfectly.
					v = score(0)
				}
				if v == nil {
					cells = append(cells, fmt.Sprintf("%s=n/a", s.name))
					continue
				}
				totals[s.name] += *v
				counts[s.name]++
				cells = append(cells, fmt.Sprintf("%s=%.2f", s.name, *v))
				if *v < passingScore {
					t.Errorf("%s scored %.2f (below %.2f); tools used: %v", s.name, *v, passingScore, r.toolsUsed)
				}
			}
			row := fmt.Sprintf("%-24s %s", c.name, strings.Join(cells, "  "))
			rows = append(rows, row)
			t.Logf("%s | tools: %v", row, r.toolsUsed)
		})
	}

	t.Log("SCORECARD")
	for _, row := range rows {
		t.Log("  " + row)
	}
	names := make([]string, 0, len(totals))
	for name := range totals {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		t.Logf("  mean %-12s %.2f  (%d/%d cases applied)",
			name, totals[name]/float64(counts[name]), counts[name], len(cases))
	}
}

// seedCanvas draws the starting diagram for a modify case by calling the tool
// the agent would, so the seed is a real canvas and not a hand-written file
// that might not match what the tools produce.
func seedCanvas(seed string) error {
	reg := tools.Default(approve(true))
	if out := reg.Dispatch(context.Background(), "generate_diagram", seed); strings.HasPrefix(out, "error:") {
		return fmt.Errorf("%s", out)
	}
	return nil
}

// runCase drives one prompt through the real agent loop and collects everything
// the scorers need.
func runCase(t *testing.T, client *openrouter.OpenRouter, c diagramCase) runResult {
	t.Helper()

	toolbox := tools.Default(approve(true))
	ag := New(client, evalModel, toolbox)

	var r runResult
	ag.OnToolCall = func(name, _, _ string) { r.toolsUsed = append(r.toolsUsed, name) }

	answer, err := ag.Ask(context.Background(), c.prompt)
	r.reply, r.err = answer, err

	if raw, readErr := os.ReadFile(evalSpecFile); readErr == nil {
		if json.Unmarshal(raw, &r.spec) == nil {
			r.specRead = true
		}
	}
	if raw, readErr := os.ReadFile(evalSVGFile); readErr == nil && len(raw) > 0 {
		r.svgOK = xml.Unmarshal(raw, new(struct{ XMLName xml.Name })) == nil
	}
	if raw, readErr := os.ReadFile(evalSceneFile); readErr == nil && len(raw) > 0 {
		r.sceneOK = json.Valid(raw)
	}
	return r
}
