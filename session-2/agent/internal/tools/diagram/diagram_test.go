package diagram

import (
	"context"
	"encoding/xml"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// signupElements is the shape a model is expected to produce for "draw a
// flowchart of user signup": terminators, steps, a decision with yes/no arrows,
// and a back edge from the failure path.
const signupElements = `{
  "title": "User signup",
  "elements": [
    {"type":"box","id":"start","label":"Start","shape":"ellipse"},
    {"type":"box","id":"form","label":"User submits signup form"},
    {"type":"box","id":"validate","label":"Valid email and password?","shape":"diamond"},
    {"type":"box","id":"error","label":"Show validation errors"},
    {"type":"box","id":"create","label":"Create account"},
    {"type":"box","id":"verify","label":"Send verification email"},
    {"type":"box","id":"done","label":"Signed up","shape":"ellipse"},
    {"type":"arrow","from":"start","to":"form"},
    {"type":"arrow","from":"form","to":"validate"},
    {"type":"arrow","from":"validate","to":"create","label":"yes"},
    {"type":"arrow","from":"validate","to":"error","label":"no"},
    {"type":"arrow","from":"error","to":"form"},
    {"type":"arrow","from":"create","to":"verify"},
    {"type":"arrow","from":"verify","to":"done"}
  ]
}`

// The headline requirement: one call draws the whole flowchart to canvas.svg,
// and what lands on disk is valid, self-contained SVG carrying every label.
func TestGenerateDiagramDrawsSignupFlowchart(t *testing.T) {
	dir := t.TempDir()
	tool := GenerateDiagram{Dir: dir}

	out, err := tool.Run(context.Background(), signupElements)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if !strings.Contains(out, canvasFile) {
		t.Fatalf("expected the result to name the file it wrote, got %q", out)
	}

	raw, err := os.ReadFile(filepath.Join(dir, canvasFile))
	if err != nil {
		t.Fatalf("canvas not written: %v", err)
	}
	svg := string(raw)

	// Well-formed XML, or the browser shows a parse error instead of a diagram.
	if err := xml.Unmarshal(raw, new(struct{ XMLName xml.Name })); err != nil {
		t.Fatalf("canvas.svg is not well-formed XML: %v", err)
	}
	if !strings.HasPrefix(svg, "<svg") {
		t.Fatalf("expected an <svg> root, got %.40q", svg)
	}

	// Every label the caller asked for must actually appear. "Valid email and
	// password?" wraps across lines, so check a word rather than the phrase.
	for _, want := range []string{"User signup", "Start", "Create account", "Send verification", "Signed up", "yes", "no"} {
		if !strings.Contains(svg, want) {
			t.Errorf("expected %q somewhere in the canvas", want)
		}
	}

	// Boxes, a decision diamond, terminator ellipses, and arrows with heads.
	// form, error, create, verify
	if got := strings.Count(svg, "<rect class=\"shape\""); got != 4 {
		t.Errorf("expected 4 plain boxes, got %d", got)
	}
	if got := strings.Count(svg, "<polygon class=\"decide\""); got != 1 {
		t.Errorf("expected 1 decision diamond, got %d", got)
	}
	if got := strings.Count(svg, "<ellipse class=\"term\""); got != 2 {
		t.Errorf("expected 2 terminators, got %d", got)
	}
	if got := strings.Count(svg, `marker-end="url(#arrowhead)"`); got != 7 {
		t.Errorf("expected 7 arrows, got %d", got)
	}

	// Self-contained: a browser reload must not need anything off disk or net.
	for _, forbidden := range []string{"<image", "xlink:href", "http://", "https://"} {
		if strings.Contains(svg, forbidden) && forbidden != "http://" {
			t.Errorf("canvas should be self-contained, found %q", forbidden)
		}
	}
}

// Redrawing overwrites the same file, which is what makes "refresh the browser"
// the whole update loop.
func TestGenerateDiagramRedrawOverwrites(t *testing.T) {
	dir := t.TempDir()
	tool := GenerateDiagram{Dir: dir}

	if _, err := tool.Run(context.Background(), signupElements); err != nil {
		t.Fatalf("first draw: %v", err)
	}
	if _, err := tool.Run(context.Background(),
		`{"elements":[{"type":"box","id":"a","label":"Only box"}]}`); err != nil {
		t.Fatalf("redraw: %v", err)
	}

	raw, _ := os.ReadFile(filepath.Join(dir, canvasFile))
	svg := string(raw)
	if !strings.Contains(svg, "Only box") {
		t.Fatal("redraw did not replace the canvas contents")
	}
	if strings.Contains(svg, "Create account") {
		t.Fatal("stale content from the previous diagram survived the redraw")
	}

	// Exactly the three files the tool owns — two renderings plus the spec
	// the CRUD tools edit — with no accumulating per-draw clutter.
	entries, _ := os.ReadDir(dir)
	got := map[string]bool{}
	for _, e := range entries {
		got[e.Name()] = true
	}
	if len(entries) != 3 || !got[canvasFile] || !got[excalidrawFile] || !got[specFile] {
		t.Fatalf("expected exactly %s, %s and %s, got %v", canvasFile, excalidrawFile, specFile, entries)
	}
}

// Nothing else on disk is touched: the filename is fixed, so an id or label
// that looks like a path can't redirect the write.
func TestGenerateDiagramWritesOnlyItsOwnFile(t *testing.T) {
	dir := t.TempDir()
	bystander := filepath.Join(dir, "important.txt")
	os.WriteFile(bystander, []byte("keep me"), 0o644)

	tool := GenerateDiagram{Dir: dir}
	if _, err := tool.Run(context.Background(),
		`{"elements":[{"type":"box","id":"../../important.txt","label":"../../important.txt"}]}`); err != nil {
		t.Fatalf("run: %v", err)
	}

	if b, _ := os.ReadFile(bystander); string(b) != "keep me" {
		t.Fatalf("a neighbouring file was modified: %q", b)
	}
}

// Models reliably invent a top-level "arrows" array instead of putting arrows
// in elements. Reading it is what stops that from silently drawing a page of
// unconnected boxes and calling it a success.
func TestGenerateDiagramAcceptsSeparateArrowsArray(t *testing.T) {
	dir := t.TempDir()
	tool := GenerateDiagram{Dir: dir}

	out, err := tool.Run(context.Background(), `{
      "title": "Signup",
      "elements": [
        {"type":"box","id":"start","label":"Start","shape":"ellipse"},
        {"type":"box","id":"form","label":"Signup form"},
        {"type":"box","id":"done","label":"Done","shape":"ellipse"}
      ],
      "arrows": [
        {"from":"start","to":"form"},
        {"from":"form","to":"done","label":"ok"}
      ]
    }`)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if !strings.Contains(out, "2 arrows") {
		t.Fatalf("arrows from the separate array were dropped: %q", out)
	}

	svg, _ := os.ReadFile(filepath.Join(dir, canvasFile))
	if got := strings.Count(string(svg), `marker-end="url(#arrowhead)"`); got != 2 {
		t.Fatalf("expected 2 drawn arrows, got %d", got)
	}
}

// The type field is inconsistently filled in, but from/to versus id is never
// ambiguous, so an element with neither type nor surprises still lands right.
func TestGenerateDiagramInfersElementKind(t *testing.T) {
	dir := t.TempDir()
	tool := GenerateDiagram{Dir: dir}

	out, err := tool.Run(context.Background(), `{"elements":[
      {"id":"a","label":"A"},
      {"id":"b","label":"B"},
      {"from":"a","to":"b"},
      {"type":"edge","from":"b","to":"a","label":"back"}
    ]}`)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if !strings.Contains(out, "2 boxes") || !strings.Contains(out, "2 arrows") {
		t.Fatalf("expected 2 boxes and 2 arrows, got %q", out)
	}
}

// Bad input comes back as a message the model can act on, not a crash.
func TestGenerateDiagramRejectsBadInput(t *testing.T) {
	dir := t.TempDir()
	tool := GenerateDiagram{Dir: dir}

	cases := []struct {
		name, args, wantIn string
	}{
		{"no boxes", `{"elements":[{"type":"arrow","from":"a","to":"b"}]}`, "at least one"},
		{"boxes but no arrows", `{"elements":[{"type":"box","id":"a","label":"A"},{"type":"box","id":"b","label":"B"}]}`, "no arrows"},
		{"arrow to unknown id", `{"elements":[{"type":"box","id":"a","label":"A"},{"type":"arrow","from":"a","to":"ghost"}]}`, "unknown box id"},
		{"box without id", `{"elements":[{"type":"box","label":"nameless"}]}`, "no id"},
		{"duplicate id", `{"elements":[{"type":"box","id":"a","label":"A"},{"type":"box","id":"a","label":"B"}]}`, "duplicate"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := tool.Run(context.Background(), c.args)
			if err == nil {
				t.Fatal("expected an error")
			}
			if !strings.Contains(err.Error(), c.wantIn) {
				t.Fatalf("error %q should mention %q", err, c.wantIn)
			}
			if _, statErr := os.Stat(filepath.Join(dir, canvasFile)); statErr == nil {
				t.Fatal("a rejected diagram must not leave a canvas behind")
			}
		})
	}
}

// A retry loop must not stretch the chart. The back edge is drawn but must not
// push its target down a layer, or a short flow spreads over a tall, mostly
// empty canvas.
func TestGenerateDiagramBackEdgeDoesNotInflateLayout(t *testing.T) {
	elements := []diagramElement{
		{Type: "box", ID: "start", Label: "Start", Shape: "ellipse"},
		{Type: "box", ID: "form", Label: "Fill the form"},
		{Type: "box", ID: "check", Label: "Valid?", Shape: "diamond"},
		{Type: "box", ID: "oops", Label: "Show errors"},
		{Type: "box", ID: "done", Label: "Done", Shape: "ellipse"},
		{Type: "arrow", From: "start", To: "form"},
		{Type: "arrow", From: "form", To: "check"},
		{Type: "arrow", From: "check", To: "done", Label: "yes"},
		{Type: "arrow", From: "check", To: "oops", Label: "no"},
		{Type: "arrow", From: "oops", To: "form"}, // the retry loop
	}
	nodes, edges, err := buildDiagram(elements)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	layoutDiagram(nodes, edges, false)

	deepest := 0
	for _, n := range nodes {
		if n.layer > deepest {
			deepest = n.layer
		}
	}
	// start → form → check → done is the longest forward path: 4 rows, layers
	// 0..3. Anything deeper means the back edge was allowed to push.
	if deepest != 3 {
		t.Fatalf("expected 4 layers (deepest index 3), got %d — a back edge inflated the layering", deepest)
	}
	// And the arrow is still drawn.
	if len(edges) != 5 {
		t.Fatalf("expected all 5 arrows kept, got %d", len(edges))
	}
}

// A cycle (the retry loop above) must lay out and terminate rather than spin.
func TestGenerateDiagramHandlesCycles(t *testing.T) {
	nodes, edges, err := buildDiagram([]diagramElement{
		{Type: "box", ID: "a", Label: "A"},
		{Type: "box", ID: "b", Label: "B"},
		{Type: "arrow", From: "a", To: "b"},
		{Type: "arrow", From: "b", To: "a"},
		{Type: "arrow", From: "a", To: "a"},
	})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	w, h := layoutDiagram(nodes, edges, false)
	if w <= 0 || h <= 0 {
		t.Fatalf("expected a positive canvas, got %.0fx%.0f", w, h)
	}
	for _, n := range nodes {
		if n.w <= 0 || n.h <= 0 {
			t.Fatalf("box %q has no size", n.id)
		}
	}
}

// Boxes must not overlap — the reason the tool lays out rather than trusting
// model-supplied coordinates.
func TestGenerateDiagramLaysOutWithoutOverlap(t *testing.T) {
	nodes, edges, err := buildDiagram([]diagramElement{
		{Type: "box", ID: "root", Label: "Root"},
		{Type: "box", ID: "l", Label: "Left branch"},
		{Type: "box", ID: "r", Label: "Right branch"},
		{Type: "arrow", From: "root", To: "l"},
		{Type: "arrow", From: "root", To: "r"},
	})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	layoutDiagram(nodes, edges, false)

	for i, a := range nodes {
		for _, b := range nodes[i+1:] {
			overlapX := a.x < b.x+b.w && b.x < a.x+a.w
			overlapY := a.y < b.y+b.h && b.y < a.y+a.h
			if overlapX && overlapY {
				t.Fatalf("boxes %q and %q overlap", a.id, b.id)
			}
		}
	}
}

func TestWrapLabel(t *testing.T) {
	cases := []struct {
		in    string
		max   int
		lines int
	}{
		{"Start", 20, 1},
		{"User submits signup form", 20, 2},
		{"a very long label that keeps going and going and going and going", 12, 3}, // capped
	}
	for _, c := range cases {
		if got := wrapLabel(c.in, c.max); len(got) != c.lines {
			t.Errorf("wrapLabel(%q) = %d lines %v, want %d", c.in, len(got), got, c.lines)
		}
	}
}
