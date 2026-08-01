package diagram

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// The saved spec is what makes editing possible at all. generate_diagram writes
// the elements it drew from; the CRUD tools load them, change one thing, and
// redraw. Without it, "rename that box" would mean handing a tool the whole
// diagram again — which is just generate_diagram with extra steps.

// specFile holds the last drawn diagram, next to the rendered canvases.
const specFile = "canvas.diagram.json"

// diagramSpec is exactly what generate_diagram was called with: enough to redraw
// the identical picture, and the thing the CRUD tools edit.
type diagramSpec struct {
	Title    string           `json:"title"`
	Elements []diagramElement `json:"elements"`
}

// clone returns a spec whose element slice can be mutated without touching the
// original. Every CRUD tool edits a clone and keeps it only once the redraw
// succeeds, so a change that would break the diagram leaves the canvas and the
// saved spec exactly as they were.
func (s diagramSpec) clone() diagramSpec {
	out := s
	out.Elements = append([]diagramElement(nil), s.Elements...)
	return out
}

// findElement locates an element by the id a caller would naturally use: a
// box's own id, or an arrow's explicit id, or an arrow written "from->to".
func findElement(elements []diagramElement, id string) int {
	for i, e := range elements {
		if elementID(e) == id {
			return i
		}
	}
	// Second pass, tolerant of spacing around the arrow: "a -> b" and "a->b"
	// are the same element to a human, and the model writes both.
	want := strings.ReplaceAll(id, " ", "")
	for i, e := range elements {
		if strings.ReplaceAll(elementID(e), " ", "") == want {
			return i
		}
	}
	return -1
}

// elementID is how an element is addressed. Boxes already have ids; arrows
// usually don't, so they fall back to "from->to", which reads naturally and is
// what a model guesses without being told.
func elementID(e diagramElement) string {
	if id := strings.TrimSpace(e.ID); id != "" {
		return id
	}
	if elementKind(e) == "arrow" {
		return strings.TrimSpace(e.From) + "->" + strings.TrimSpace(e.To)
	}
	return ""
}

// elementIDs lists what's addressable, for the error message when an id misses.
// Sorted so the same diagram always reports the same list.
func elementIDs(elements []diagramElement) []string {
	var ids []string
	for _, e := range elements {
		if id := elementID(e); id != "" {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	return ids
}

func loadSpec(dir string) (diagramSpec, error) {
	raw, err := os.ReadFile(filepath.Join(dir, specFile))
	if os.IsNotExist(err) {
		return diagramSpec{}, fmt.Errorf("there is no diagram to edit yet; call generate_diagram first")
	}
	if err != nil {
		return diagramSpec{}, err
	}
	var spec diagramSpec
	if err := json.Unmarshal(raw, &spec); err != nil {
		return diagramSpec{}, fmt.Errorf("the saved diagram is unreadable (%v); redraw it with generate_diagram", err)
	}
	return spec, nil
}

func saveSpec(dir string, spec diagramSpec) error {
	raw, err := json.MarshalIndent(spec, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, specFile), append(raw, '\n'), 0o644)
}
