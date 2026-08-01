package diagram

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// drawThenModify draws the signup flowchart, then applies one modification.
func drawThenModify(t *testing.T, modifyArgs string) (string, error, string) {
	t.Helper()
	dir := t.TempDir()
	if _, err := (GenerateDiagram{Dir: dir}).Run(context.Background(), signupElements); err != nil {
		t.Fatalf("initial draw: %v", err)
	}
	out, err := (ModifyDiagram{Dir: dir}).Run(context.Background(), modifyArgs)
	return out, err, dir
}

func readCanvas(t *testing.T, dir string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(dir, canvasFile))
	if err != nil {
		t.Fatalf("canvas: %v", err)
	}
	return string(raw)
}

// The headline case: rename one box without restating the diagram, and every
// other box survives untouched.
func TestModifyDiagramChangesOneLabel(t *testing.T) {
	out, err, dir := drawThenModify(t, `{"id":"create","updates":{"label":"Create the account"}}`)
	if err != nil {
		t.Fatalf("modify: %v", err)
	}
	if !strings.Contains(out, "Create account") || !strings.Contains(out, "Create the account") {
		t.Errorf("expected the result to report the before and after, got %q", out)
	}

	svg := readCanvas(t, dir)
	if !strings.Contains(svg, "Create the account") {
		t.Error("the new label is missing from the canvas")
	}
	if strings.Contains(svg, ">Create account<") {
		t.Error("the old label survived the redraw")
	}
	for _, untouched := range []string{"Start", "Signed up", "yes", "no"} {
		if !strings.Contains(svg, untouched) {
			t.Errorf("modifying one element dropped %q from the diagram", untouched)
		}
	}
}

// Shapes are editable too, and the change reaches both renderings.
func TestModifyDiagramChangesShape(t *testing.T) {
	_, err, dir := drawThenModify(t, `{"id":"create","updates":{"shape":"diamond"}}`)
	if err != nil {
		t.Fatalf("modify: %v", err)
	}

	svg := readCanvas(t, dir)
	if got := strings.Count(svg, `<polygon class="decide"`); got != 2 {
		t.Errorf("expected 2 diamonds after the change, got %d", got)
	}

	raw, _ := os.ReadFile(filepath.Join(dir, excalidrawFile))
	var scene excalidrawScene
	json.Unmarshal(raw, &scene)
	diamonds := 0
	for _, el := range scene.Elements {
		if el["type"] == "diamond" {
			diamonds++
		}
	}
	if diamonds != 2 {
		t.Errorf("excalidraw scene has %d diamonds, want 2", diamonds)
	}
}

// Arrows have no ids of their own, so they're addressed "from->to" — the form
// a model reaches for without being told.
func TestModifyDiagramRepointsArrow(t *testing.T) {
	out, err, dir := drawThenModify(t, `{"id":"create->verify","updates":{"to":"done","label":"skip email"}}`)
	if err != nil {
		t.Fatalf("modify: %v", err)
	}
	if !strings.Contains(out, "verify") || !strings.Contains(out, "done") {
		t.Errorf("expected the repoint to be reported, got %q", out)
	}
	if svg := readCanvas(t, dir); !strings.Contains(svg, "skip email") {
		t.Error("the new edge label is missing from the canvas")
	}
}

// Spacing around the arrow is noise, not meaning.
func TestModifyDiagramToleratesSpacedArrowID(t *testing.T) {
	if _, err, _ := drawThenModify(t, `{"id":"create -> verify","updates":{"label":"then"}}`); err != nil {
		t.Fatalf("expected a spaced arrow id to resolve: %v", err)
	}
}

// A modification that would break the diagram must leave the canvas and the
// saved spec exactly as they were — a half-applied edit is worse than none.
func TestModifyDiagramRejectsBreakingChangeAtomically(t *testing.T) {
	dir := t.TempDir()
	if _, err := (GenerateDiagram{Dir: dir}).Run(context.Background(), signupElements); err != nil {
		t.Fatalf("initial draw: %v", err)
	}
	before := readCanvas(t, dir)
	specBefore, _ := os.ReadFile(filepath.Join(dir, specFile))

	_, err := (ModifyDiagram{Dir: dir}).Run(context.Background(),
		`{"id":"create->verify","updates":{"to":"nowhere"}}`)
	if err == nil {
		t.Fatal("expected repointing an arrow at a missing box to fail")
	}
	if !strings.Contains(err.Error(), "nothing was modified") {
		t.Errorf("the error should say nothing changed, got %q", err)
	}

	if after := readCanvas(t, dir); after != before {
		t.Error("the canvas changed despite the failed modification")
	}
	specAfter, _ := os.ReadFile(filepath.Join(dir, specFile))
	if string(specAfter) != string(specBefore) {
		t.Error("the saved spec changed despite the failed modification")
	}
}

func TestModifyDiagramUnknownID(t *testing.T) {
	_, err, _ := drawThenModify(t, `{"id":"ghost","updates":{"label":"nope"}}`)
	if err == nil {
		t.Fatal("expected an error for an unknown id")
	}
	// The message has to list what IS addressable, or the model is guessing.
	for _, want := range []string{"ghost", "create", "start->form"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error should mention %q, got %q", want, err)
		}
	}
}

// Modifying before drawing points at the tool that fixes it.
func TestModifyDiagramWithoutADiagram(t *testing.T) {
	_, err := (ModifyDiagram{Dir: t.TempDir()}).Run(context.Background(),
		`{"id":"a","updates":{"label":"x"}}`)
	if err == nil {
		t.Fatal("expected an error when no diagram exists")
	}
	if !strings.Contains(err.Error(), "generate_diagram") {
		t.Errorf("error should point at generate_diagram, got %q", err)
	}
}

// A no-op edit reports itself rather than silently redrawing.
func TestModifyDiagramNoOp(t *testing.T) {
	out, err, _ := drawThenModify(t, `{"id":"create","updates":{"label":"Create account"}}`)
	if err != nil {
		t.Fatalf("modify: %v", err)
	}
	if !strings.Contains(out, "nothing to change") {
		t.Errorf("expected a no-op to say so, got %q", out)
	}
}

// generate_diagram must leave the spec behind, or modify_diagram has nothing to
// work from. The spec has to round-trip to the same drawing.
func TestGenerateDiagramSavesRedrawableSpec(t *testing.T) {
	dir := t.TempDir()
	if _, err := (GenerateDiagram{Dir: dir}).Run(context.Background(), signupElements); err != nil {
		t.Fatalf("draw: %v", err)
	}
	before := readCanvas(t, dir)

	spec, err := loadSpec(dir)
	if err != nil {
		t.Fatalf("spec not saved: %v", err)
	}
	if spec.Title != "User signup" {
		t.Errorf("spec title = %q", spec.Title)
	}

	redrawn := t.TempDir()
	if _, _, err := drawDiagram(redrawn, spec); err != nil {
		t.Fatalf("redraw from spec: %v", err)
	}
	if after := readCanvas(t, redrawn); after != before {
		t.Error("redrawing from the saved spec produced a different picture")
	}
}
