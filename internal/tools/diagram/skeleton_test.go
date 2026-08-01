package diagram

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// The three derivations convertToExcalidrawElements is responsible for, each
// tested against the thing that breaks when it's wrong.

// A Label becomes a second element bound to its container. Both halves have to
// be written — the text's containerId and the container's boundElements — or
// the label vanishes on import.
func TestConvertDerivesBoundLabel(t *testing.T) {
	els := convertToExcalidrawElements([]skeletonElement{{
		Type: "rectangle", ID: "box", X: 0, Y: 0, Width: 100, Height: 50,
		Label: &skeletonLabel{Text: "Hello"},
	}})

	if len(els) != 2 {
		t.Fatalf("a labelled shape should expand to 2 elements, got %d", len(els))
	}
	container, text := els[0], els[1]

	if text["type"] != "text" || text["text"] != "Hello" {
		t.Fatalf("expected a text element carrying the label, got %v", text)
	}
	if text["containerId"] != container["id"] {
		t.Errorf("text containerId = %v, want %v", text["containerId"], container["id"])
	}
	if text["autoResize"] != false {
		t.Error("bound text must not auto-resize, or a long label grows the shape")
	}
	bound, _ := container["boundElements"].([]map[string]any)
	if len(bound) != 1 || bound[0]["id"] != text["id"] {
		t.Errorf("container should list its bound text, got %v", container["boundElements"])
	}
}

// Start/End become bindings on the arrow AND entries on both targets. Without
// the second half the arrow detaches the first time a shape is dragged.
func TestConvertDerivesTwoWayArrowBinding(t *testing.T) {
	els := convertToExcalidrawElements([]skeletonElement{
		{Type: "rectangle", ID: "a", X: 0, Y: 0, Width: 80, Height: 40},
		{Type: "rectangle", ID: "b", X: 0, Y: 120, Width: 80, Height: 40},
		{Type: "arrow", X: 40, Y: 40, Width: 0, Height: 80,
			Start: &skeletonBinding{ID: "a"}, End: &skeletonBinding{ID: "b"}},
	})

	byID := map[string]map[string]any{}
	for _, el := range els {
		byID[el["id"].(string)] = el
	}

	var arrow map[string]any
	for _, el := range els {
		if el["type"] == "arrow" {
			arrow = el
		}
	}
	if arrow == nil {
		t.Fatal("no arrow in the converted scene")
	}

	start, _ := arrow["startBinding"].(map[string]any)
	end, _ := arrow["endBinding"].(map[string]any)
	if start == nil || start["elementId"] != "a" {
		t.Errorf("startBinding = %v, want a", arrow["startBinding"])
	}
	if end == nil || end["elementId"] != "b" {
		t.Errorf("endBinding = %v, want b", arrow["endBinding"])
	}

	// The other half: each shape must list the arrow.
	for _, id := range []string{"a", "b"} {
		bound, _ := byID[id]["boundElements"].([]map[string]any)
		found := false
		for _, entry := range bound {
			if entry["id"] == arrow["id"] {
				found = true
			}
		}
		if !found {
			t.Errorf("shape %q does not list the arrow in boundElements: %v", id, bound)
		}
	}
}

// A labelled arrow is a first-class skeleton, so the label binds to the arrow
// and travels with it — the old hand-built export dropped edge labels as loose
// text that drifted the moment anything moved.
func TestConvertDerivesLabelledArrow(t *testing.T) {
	els := convertToExcalidrawElements([]skeletonElement{{
		Type: "arrow", X: 0, Y: 0, Width: 100, Height: 0,
		Label: &skeletonLabel{Text: "yes"},
	}})

	var arrow, text map[string]any
	for _, el := range els {
		switch el["type"] {
		case "arrow":
			arrow = el
		case "text":
			text = el
		}
	}
	if arrow == nil || text == nil {
		t.Fatalf("expected an arrow and its label, got %d elements", len(els))
	}
	if text["containerId"] != arrow["id"] {
		t.Errorf("the label should be bound to the arrow, got containerId=%v", text["containerId"])
	}
	if text["text"] != "yes" {
		t.Errorf("label text = %v", text["text"])
	}
}

// Ids are derived when absent, and stable — redrawing the same diagram must not
// look like a brand new scene.
func TestConvertAssignsStableIDs(t *testing.T) {
	skels := []skeletonElement{
		{Type: "rectangle", X: 0, Y: 0, Width: 10, Height: 10},
		{Type: "ellipse", X: 0, Y: 20, Width: 10, Height: 10},
	}
	first := convertToExcalidrawElements(skels)
	second := convertToExcalidrawElements(skels)

	a, _ := json.Marshal(first)
	b, _ := json.Marshal(second)
	if string(a) != string(b) {
		t.Fatal("converting the same skeletons twice produced different elements")
	}
	for _, el := range first {
		if el["id"] == "" || el["id"] == nil {
			t.Errorf("element has no derived id: %v", el)
		}
	}
}

// Every element the converter emits must carry the full field set — Excalidraw
// builds fall over on import when one is missing.
func TestConvertEmitsCompleteElements(t *testing.T) {
	els := convertToExcalidrawElements([]skeletonElement{
		{Type: "rectangle", X: 0, Y: 0, Width: 10, Height: 10, Label: &skeletonLabel{Text: "x"}},
		{Type: "arrow", X: 0, Y: 0, Width: 10, Height: 10},
		{Type: "text", X: 0, Y: 0, Text: "free"},
		{Type: "line", X: 0, Y: 0, Width: 10, Height: 10, Points: [][]float64{{0, 0}, {10, 10}}},
	})

	required := []string{
		"id", "type", "x", "y", "width", "height", "angle", "strokeColor",
		"backgroundColor", "fillStyle", "strokeWidth", "strokeStyle", "roughness",
		"opacity", "groupIds", "seed", "version", "versionNonce", "isDeleted",
		"updated", "locked",
	}
	for _, el := range els {
		for _, field := range required {
			if _, ok := el[field]; !ok {
				t.Errorf("%v element missing required field %q", el["type"], field)
			}
		}
	}
}

// The skeleton file is the authoring form, and has to be exactly what
// convertToExcalidrawElements accepts: types, positions and intent, with none
// of the derived bookkeeping baked in.
func TestSkeletonFileIsAuthoringForm(t *testing.T) {
	dir := t.TempDir()
	if _, err := (GenerateDiagram{Dir: dir}).Run(context.Background(), signupElements); err != nil {
		t.Fatalf("draw: %v", err)
	}

	raw, err := os.ReadFile(filepath.Join(dir, skeletonFile))
	if err != nil {
		t.Fatalf("skeleton file not written: %v", err)
	}
	var skels []map[string]any
	if err := json.Unmarshal(raw, &skels); err != nil {
		t.Fatalf("skeleton file is not valid JSON: %v", err)
	}
	if len(skels) == 0 {
		t.Fatal("no skeletons written")
	}

	for _, s := range skels {
		if s["type"] == nil {
			t.Errorf("skeleton with no type: %v", s)
		}
		// None of the derived machinery belongs in the authoring form.
		for _, derived := range []string{"seed", "versionNonce", "boundElements", "containerId", "startBinding", "endBinding"} {
			if _, present := s[derived]; present {
				t.Errorf("skeleton should not carry derived field %q: %v", derived, s)
			}
		}
	}

	// A labelled shape is expressed as `label`, and an arrow's connection as
	// `start`/`end` — the whole point of the format.
	var labelled, bound int
	for _, s := range skels {
		if _, ok := s["label"]; ok {
			labelled++
		}
		if _, ok := s["start"]; ok {
			bound++
		}
	}
	if labelled == 0 {
		t.Error("expected labelled shapes in the skeleton form")
	}
	if bound == 0 {
		t.Error("expected arrows bound with start/end in the skeleton form")
	}
}
