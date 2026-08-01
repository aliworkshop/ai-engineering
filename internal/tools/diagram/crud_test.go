package diagram

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// drawn returns a directory holding the signup flowchart, ready to be edited.
func drawn(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if _, err := (GenerateDiagram{Dir: dir}).Run(context.Background(), signupElements); err != nil {
		t.Fatalf("initial draw: %v", err)
	}
	return dir
}

func readCanvas(t *testing.T, dir string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(dir, canvasFile))
	if err != nil {
		t.Fatalf("canvas: %v", err)
	}
	return string(raw)
}

// ---------- add_elements ----------

// A batch add is one call, and it leaves everything already there alone.
func TestAddElementsBatch(t *testing.T) {
	dir := drawn(t)
	out, err := (AddElements{Dir: dir}).Run(context.Background(), `{"elements":[
      {"type":"box","id":"captcha","label":"Solve captcha"},
      {"type":"arrow","from":"form","to":"captcha"},
      {"type":"arrow","from":"captcha","to":"validate"}
    ]}`)
	if err != nil {
		t.Fatalf("add: %v", err)
	}
	if !strings.Contains(out, "captcha") {
		t.Errorf("expected the additions to be reported, got %q", out)
	}

	svg := readCanvas(t, dir)
	if !strings.Contains(svg, "Solve captcha") {
		t.Error("the new box is missing from the canvas")
	}
	for _, untouched := range []string{"Start", "Create account", "Signed up", "yes", "no"} {
		if !strings.Contains(svg, untouched) {
			t.Errorf("adding dropped %q from the diagram", untouched)
		}
	}
}

// Additive means additive: a colliding id is a mistake, not an upsert, because
// silently overwriting would lose whatever was already there.
func TestAddElementsRejectsExistingID(t *testing.T) {
	dir := drawn(t)
	_, err := (AddElements{Dir: dir}).Run(context.Background(),
		`{"elements":[{"type":"box","id":"create","label":"Duplicate"}]}`)
	if err == nil {
		t.Fatal("expected adding an existing id to fail")
	}
	if !strings.Contains(err.Error(), "update_elements") {
		t.Errorf("the error should point at update_elements, got %q", err)
	}
	if strings.Contains(readCanvas(t, dir), "Duplicate") {
		t.Error("the rejected element reached the canvas")
	}
}

// An arrow to a box that doesn't exist must not half-apply the batch.
func TestAddElementsAtomicOnBadArrow(t *testing.T) {
	dir := drawn(t)
	before := readCanvas(t, dir)

	_, err := (AddElements{Dir: dir}).Run(context.Background(), `{"elements":[
      {"type":"box","id":"captcha","label":"Solve captcha"},
      {"type":"arrow","from":"captcha","to":"nowhere"}
    ]}`)
	if err == nil {
		t.Fatal("expected an arrow to a missing box to fail")
	}
	if !strings.Contains(err.Error(), "nothing was added") {
		t.Errorf("the error should say nothing was added, got %q", err)
	}
	if readCanvas(t, dir) != before {
		t.Error("the canvas changed despite the failed batch — the good half was applied")
	}
}

func TestAddElementsRejectsEmpty(t *testing.T) {
	if _, err := (AddElements{Dir: drawn(t)}).Run(context.Background(), `{"elements":[]}`); err == nil {
		t.Fatal("expected an empty batch to be rejected")
	}
}

// ---------- update_elements ----------

// Several elements change in one call; nothing else moves.
func TestUpdateElementsBatch(t *testing.T) {
	dir := drawn(t)
	out, err := (UpdateElements{Dir: dir}).Run(context.Background(), `{"updates":[
      {"id":"create","label":"Provision account"},
      {"id":"error","shape":"diamond"},
      {"id":"validate->create","label":"looks good"}
    ]}`)
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	for _, want := range []string{"create", "error", "validate->create"} {
		if !strings.Contains(out, want) {
			t.Errorf("expected %q in the report, got %q", want, out)
		}
	}

	svg := readCanvas(t, dir)
	if !strings.Contains(svg, "Provision account") {
		t.Error("the new label is missing")
	}
	if strings.Contains(svg, ">Create account<") {
		t.Error("the old label survived")
	}
	if !strings.Contains(svg, "looks good") {
		t.Error("the new edge label is missing")
	}
	if got := strings.Count(svg, `<polygon class="decide"`); got != 2 {
		t.Errorf("expected 2 diamonds after the shape change, got %d", got)
	}
}

// The change has to reach both renderings, not just the SVG.
func TestUpdateElementsReachesExcalidraw(t *testing.T) {
	dir := drawn(t)
	if _, err := (UpdateElements{Dir: dir}).Run(context.Background(),
		`{"updates":[{"id":"create","label":"Provision account"}]}`); err != nil {
		t.Fatalf("update: %v", err)
	}

	raw, _ := os.ReadFile(filepath.Join(dir, excalidrawFile))
	var scene excalidrawScene
	json.Unmarshal(raw, &scene)
	for _, el := range scene.Elements {
		if s, ok := el["text"].(string); ok && s == "Provision account" {
			return
		}
	}
	t.Error("the updated label never reached the excalidraw scene")
}

// Spacing around an arrow id is noise, not meaning.
func TestUpdateElementsToleratesSpacedArrowID(t *testing.T) {
	dir := drawn(t)
	if _, err := (UpdateElements{Dir: dir}).Run(context.Background(),
		`{"updates":[{"id":"create -> verify","label":"then"}]}`); err != nil {
		t.Fatalf("expected a spaced arrow id to resolve: %v", err)
	}
}

// A whole batch is rejected if any single change would break the diagram.
func TestUpdateElementsAtomic(t *testing.T) {
	dir := drawn(t)
	before := readCanvas(t, dir)
	specBefore, _ := os.ReadFile(filepath.Join(dir, specFile))

	_, err := (UpdateElements{Dir: dir}).Run(context.Background(), `{"updates":[
      {"id":"create","label":"This one is fine"},
      {"id":"create->verify","to":"nowhere"}
    ]}`)
	if err == nil {
		t.Fatal("expected repointing at a missing box to fail")
	}
	if !strings.Contains(err.Error(), "nothing was modified") {
		t.Errorf("the error should say nothing changed, got %q", err)
	}
	if readCanvas(t, dir) != before {
		t.Error("the canvas changed despite the failed batch")
	}
	specAfter, _ := os.ReadFile(filepath.Join(dir, specFile))
	if string(specAfter) != string(specBefore) {
		t.Error("the saved spec changed despite the failed batch")
	}
}

func TestUpdateElementsUnknownID(t *testing.T) {
	_, err := (UpdateElements{Dir: drawn(t)}).Run(context.Background(),
		`{"updates":[{"id":"ghost","label":"nope"}]}`)
	if err == nil {
		t.Fatal("expected an error for an unknown id")
	}
	// The message must list what IS addressable, or the model is guessing.
	for _, want := range []string{"ghost", "create", "start->form"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error should mention %q, got %q", want, err)
		}
	}
}

func TestUpdateElementsNoOp(t *testing.T) {
	out, err := (UpdateElements{Dir: drawn(t)}).Run(context.Background(),
		`{"updates":[{"id":"create","label":"Create account"}]}`)
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if !strings.Contains(out, "nothing to change") {
		t.Errorf("expected a no-op to say so, got %q", out)
	}
}

// ---------- remove_elements ----------

// Removing a box and its arrows together, in one explicit call.
func TestRemoveElementsBatch(t *testing.T) {
	dir := drawn(t)
	out, err := (RemoveElements{Dir: dir}).Run(context.Background(),
		`{"ids":["error","validate->error","error->form"]}`)
	if err != nil {
		t.Fatalf("remove: %v", err)
	}
	if !strings.Contains(out, "error") {
		t.Errorf("expected the removals to be reported, got %q", out)
	}

	svg := readCanvas(t, dir)
	if strings.Contains(svg, "Show validation") {
		t.Error("the removed box is still on the canvas")
	}
	for _, kept := range []string{"Start", "Create account", "Signed up"} {
		if !strings.Contains(svg, kept) {
			t.Errorf("removal dropped %q, which was not named", kept)
		}
	}
}

// Explicit, not cascading: removing a box whose arrows weren't named is an
// error that names them, rather than a silent deletion of edges the caller
// never mentioned.
func TestRemoveElementsRefusesToCascade(t *testing.T) {
	dir := drawn(t)
	before := readCanvas(t, dir)

	_, err := (RemoveElements{Dir: dir}).Run(context.Background(), `{"ids":["error"]}`)
	if err == nil {
		t.Fatal("expected removing a box with live arrows to fail")
	}
	for _, want := range []string{"validate->error", "error->form", "nothing was removed"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error should mention %q, got %q", want, err)
		}
	}
	if readCanvas(t, dir) != before {
		t.Error("the canvas changed despite the refusal")
	}
}

// Removing just an arrow needs no ceremony — no box is orphaned.
func TestRemoveElementsArrowOnly(t *testing.T) {
	dir := drawn(t)
	if _, err := (RemoveElements{Dir: dir}).Run(context.Background(),
		`{"ids":["error->form"]}`); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if got := strings.Count(readCanvas(t, dir), `marker-end="url(#arrowhead)"`); got != 6 {
		t.Errorf("expected 6 arrows after removing one, got %d", got)
	}
}

func TestRemoveElementsUnknownID(t *testing.T) {
	dir := drawn(t)
	before := readCanvas(t, dir)
	if _, err := (RemoveElements{Dir: dir}).Run(context.Background(), `{"ids":["ghost"]}`); err == nil {
		t.Fatal("expected an unknown id to fail")
	}
	if readCanvas(t, dir) != before {
		t.Error("the canvas changed despite the failed removal")
	}
}

// Emptying the diagram entirely is refused: there'd be nothing to draw.
func TestRemoveElementsRefusesToEmptyTheDiagram(t *testing.T) {
	dir := t.TempDir()
	if _, err := (GenerateDiagram{Dir: dir}).Run(context.Background(),
		`{"elements":[{"type":"box","id":"only","label":"Only box"}]}`); err != nil {
		t.Fatalf("draw: %v", err)
	}
	if _, err := (RemoveElements{Dir: dir}).Run(context.Background(), `{"ids":["only"]}`); err == nil {
		t.Fatal("expected removing the last element to fail")
	}
}

func TestRemoveElementsRejectsEmpty(t *testing.T) {
	if _, err := (RemoveElements{Dir: drawn(t)}).Run(context.Background(), `{"ids":[]}`); err == nil {
		t.Fatal("expected an empty batch to be rejected")
	}
}

// ---------- shared ----------

// Every CRUD tool points at generate_diagram when there's nothing to edit yet.
func TestCRUDWithoutADiagram(t *testing.T) {
	cases := map[string]func(dir string) error{
		"add": func(dir string) error {
			_, err := (AddElements{Dir: dir}).Run(context.Background(), `{"elements":[{"type":"box","id":"a","label":"A"}]}`)
			return err
		},
		"update": func(dir string) error {
			_, err := (UpdateElements{Dir: dir}).Run(context.Background(), `{"updates":[{"id":"a","label":"A"}]}`)
			return err
		},
		"remove": func(dir string) error {
			_, err := (RemoveElements{Dir: dir}).Run(context.Background(), `{"ids":["a"]}`)
			return err
		},
	}
	for name, run := range cases {
		t.Run(name, func(t *testing.T) {
			err := run(t.TempDir())
			if err == nil {
				t.Fatal("expected an error when no diagram exists")
			}
			if !strings.Contains(err.Error(), "generate_diagram") {
				t.Errorf("error should point at generate_diagram, got %q", err)
			}
		})
	}
}

// generate_diagram must leave a spec the CRUD tools can work from, and it has
// to round-trip to the same drawing.
func TestGenerateDiagramSavesRedrawableSpec(t *testing.T) {
	dir := drawn(t)
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

// A sequence of edits composes: add, update, remove, each seeing the last.
func TestCRUDComposesAcrossCalls(t *testing.T) {
	dir := drawn(t)
	ctx := context.Background()

	if _, err := (AddElements{Dir: dir}).Run(ctx, `{"elements":[
      {"type":"box","id":"captcha","label":"Solve captcha"},
      {"type":"arrow","from":"form","to":"captcha"}
    ]}`); err != nil {
		t.Fatalf("add: %v", err)
	}
	if _, err := (UpdateElements{Dir: dir}).Run(ctx,
		`{"updates":[{"id":"captcha","label":"Prove you are human","shape":"diamond"}]}`); err != nil {
		t.Fatalf("update: %v", err)
	}
	if _, err := (RemoveElements{Dir: dir}).Run(ctx,
		`{"ids":["captcha","form->captcha"]}`); err != nil {
		t.Fatalf("remove: %v", err)
	}

	svg := readCanvas(t, dir)
	for _, gone := range []string{"Solve captcha", "Prove you are human"} {
		if strings.Contains(svg, gone) {
			t.Errorf("%q survived the remove", gone)
		}
	}
	if !strings.Contains(svg, "Create account") {
		t.Error("the original diagram did not survive the edit sequence")
	}
}
